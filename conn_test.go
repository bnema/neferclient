package neferclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/neferclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	neferclientmocks "github.com/bnema/neferclient/mocks"
)

// dispatchUntil dispatches whenever Wake fires until done reports true.
func dispatchUntil(t *testing.T, c *neferclient.Conn, h neferclient.Handler, done func() bool) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		require.NoError(t, c.Dispatch(h))
		if done() {
			return
		}
		select {
		case <-c.Wake():
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}
}

func TestSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	t.Setenv("WAYLAND_DISPLAY", "")
	p, err := neferclient.SocketPath("")
	require.NoError(t, err)
	assert.Equal(t, "/run/user/1/wayland-0", p)
	t.Setenv("WAYLAND_DISPLAY", "wayland-7")
	p, _ = neferclient.SocketPath("")
	assert.Equal(t, "/run/user/1/wayland-7", p)
	p, _ = neferclient.SocketPath("x")
	assert.Equal(t, "/run/user/1/x", p)
	p, _ = neferclient.SocketPath("/tmp/sock")
	assert.Equal(t, "/tmp/sock", p)
	t.Setenv("XDG_RUNTIME_DIR", "")
	_, err = neferclient.SocketPath("rel")
	assert.Error(t, err)
}

func TestConnectInitialOutputs(t *testing.T) {
	c, srv := connectWire(t,
		[]wireGlobal{{1, "wl_seat", 9}, {7, outputIface, 4}, {9, outputIface, 2}},
		map[uint32]outputSpec{7: {"DP-1", 2, 1920, 1080}, 9: {"ignored-before-v4", 1, 800, 600}})
	assert.Equal(t, []neferclient.Output{
		{Global: 7, Name: "DP-1", Scale: 2, Width: 1920, Height: 1080},
		{Global: 9, Name: "", Scale: 1, Width: 800, Height: 600},
	}, c.Outputs())
	assert.Equal(t, uint32(4), srv.waitBound(7).version, "bound at min(announced, 4)")
	assert.Equal(t, uint32(2), srv.waitBound(9).version)

	// Setup events are applied silently: nothing is left for the Handler.
	h := neferclientmocks.NewMockHandler(t)
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(h))
}

func TestConnectContextCancelled(t *testing.T) {
	client, _ := socketPair(t) // peer never answers
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c, err := neferclient.ConnectConn(ctx, client)
	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	cancelled, stop := context.WithCancel(context.Background())
	stop()
	_, err = neferclient.Connect(cancelled, "")
	assert.ErrorIs(t, err, context.Canceled)
	//lint:ignore SA1012 a nil context is the case under test
	_, err = neferclient.Connect(nil, "")
	assert.Error(t, err)
}

func TestHotplug(t *testing.T) {
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}},
		map[uint32]outputSpec{7: {"DP-1", 1, 1920, 1080}, 11: {"HDMI-A-1", 2, 3840, 2160}})
	h := neferclientmocks.NewMockHandler(t)
	var added []neferclient.Output
	h.EXPECT().OutputAdded(mock.Anything).Run(func(out *neferclient.Output) { added = append(added, *out) }).Once()
	srv.announce(wireGlobal{11, outputIface, 4})
	dispatchUntil(t, c, h, func() bool { return len(added) == 1 })
	assert.Equal(t, neferclient.Output{Global: 11, Name: "HDMI-A-1", Scale: 2, Width: 3840, Height: 2160}, added[0])
	require.Len(t, c.Outputs(), 2)

	h.EXPECT().OutputRemoved(uint32(7)).Once()
	srv.remove(7)
	dispatchUntil(t, c, h, func() bool { return len(c.Outputs()) == 1 })
	assert.Equal(t, uint32(11), c.Outputs()[0].Global)
}

func TestOutputPropertyChangeIsSilent(t *testing.T) {
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}}, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	obj := srv.waitBound(7)
	srv.write(frame(obj.id, 3, appendU32(nil, 3))) // scale 3
	srv.write(frame(obj.id, 2, nil))               // done
	h := neferclientmocks.NewMockHandler(t)        // no expectations: any call fails
	dispatchUntil(t, c, h, func() bool { return c.Outputs()[0].Scale == 3 })
}

func TestRingBackpressureKeepsOrder(t *testing.T) {
	const total = 3 * neferclient.RingSize
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}}, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	obj := srv.waitBound(7)
	var buf []byte
	for i := 1; i <= total; i++ {
		buf = append(buf, frame(obj.id, 3, appendU32(nil, uint32(i)))...)
	}
	buf = append(buf, frame(obj.id, 2, nil)...)
	go srv.write(buf)
	waitFor(t, func() bool { return c.QueueLen() == neferclient.RingSize }, "full ring")

	// A pause (Roundtrip) must not deadlock against a producer blocked on the
	// full ring, and must not reorder what it held back.
	require.NoError(t, c.Roundtrip())
	dispatchUntil(t, c, nil, func() bool { return c.Outputs()[0].Scale == total })
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBind(t *testing.T) {
	c, srv := connectWire(t, []wireGlobal{{3, wayland.CompositorInterface, 6}}, nil)
	comp, v, err := c.Bind[*wayland.Compositor](wayland.CompositorInterface, 4, wayland.NewCompositor)
	require.NoError(t, err)
	assert.NotNil(t, comp)
	assert.Equal(t, uint32(4), v)
	assert.Equal(t, uint32(4), srv.waitBound(3).version)

	_, _, err = c.Bind[*wayland.Shm](wayland.ShmInterface, 1, wayland.NewShm)
	assert.ErrorIs(t, err, neferclient.ErrGlobalNotFound)
}

func TestWatchFD(t *testing.T) {
	c, _ := connectWire(t, nil, nil)
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	require.NoError(t, err)
	defer unix.Close(efd)
	require.NoError(t, c.WatchFD(efd, 42))
	assert.Error(t, c.WatchFD(efd, 43), "double watch")

	h := neferclientmocks.NewMockHandler(t)
	got := 0
	h.EXPECT().FDReady(uint64(42)).Run(func(uint64) {
		got++
		var b [8]byte
		_, _ = unix.Read(efd, b[:])
	}).Times(2)
	signal := func() {
		one := [8]byte{1}
		_, err := unix.Write(efd, one[:])
		require.NoError(t, err)
	}
	signal()
	dispatchUntil(t, c, h, func() bool { return got == 1 })
	signal() // re-armed after the first FDReady returned
	dispatchUntil(t, c, h, func() bool { return got == 2 })

	require.NoError(t, c.UnwatchFD(efd))
	assert.Error(t, c.UnwatchFD(efd))
	signal()
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(h)) // would fail the mock's Times(2)
}

func TestUnwatchInsideHandler(t *testing.T) {
	c, _ := connectWire(t, nil, nil)
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	require.NoError(t, err)
	defer unix.Close(efd)
	require.NoError(t, c.WatchFD(efd, 1))
	h := neferclientmocks.NewMockHandler(t)
	called := false
	h.EXPECT().FDReady(uint64(1)).Run(func(uint64) {
		called = true
		require.NoError(t, c.UnwatchFD(efd))
	}).Once()
	one := [8]byte{1}
	_, _ = unix.Write(efd, one[:])
	dispatchUntil(t, c, h, func() bool { return called })
}

func TestTransportErrorSurfacesAfterQueuedEvents(t *testing.T) {
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}}, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	obj := srv.waitBound(7)
	srv.write(frame(obj.id, 3, appendU32(nil, 5)))
	srv.write(frame(obj.id, 2, nil))
	srv.closeConn()
	timeout := time.After(10 * time.Second)
	var err error
	for err == nil {
		err = c.Dispatch(nil)
		if err != nil {
			break
		}
		select {
		case <-c.Wake():
		case <-timeout:
			t.Fatal("transport error never surfaced")
		}
	}
	assert.Equal(t, int32(5), c.Outputs()[0].Scale, "events queued before the failure are applied first")
}

func TestHandlerCannotReenterDispatch(t *testing.T) {
	c, srv := connectWire(t, nil, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	h := neferclientmocks.NewMockHandler(t)
	var inner error
	h.EXPECT().OutputAdded(mock.Anything).Run(func(*neferclient.Output) { inner = c.Dispatch(h) }).Once()
	srv.announce(wireGlobal{7, outputIface, 4})
	dispatchUntil(t, c, h, func() bool { return inner != nil })
	assert.Error(t, inner)
}

func TestClose(t *testing.T) {
	c, _ := connectWire(t, nil, nil)
	require.NoError(t, c.Close())
	assert.NoError(t, c.Close())
	assert.ErrorIs(t, c.Dispatch(nil), neferclient.ErrClosed)
	assert.ErrorIs(t, c.Roundtrip(), neferclient.ErrClosed)
	assert.ErrorIs(t, c.WatchFD(0, 1), neferclient.ErrClosed)
	_, _, err := c.Bind[*wayland.Shm](wayland.ShmInterface, 1, wayland.NewShm)
	assert.ErrorIs(t, err, neferclient.ErrClosed)
	var nilConn *neferclient.Conn
	assert.NoError(t, nilConn.Close())
}

func TestCloseWhileReaderBlockedOnFullRing(t *testing.T) {
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}}, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	obj := srv.waitBound(7)
	var buf []byte
	for i := 0; i < 2*neferclient.RingSize; i++ {
		buf = append(buf, frame(obj.id, 3, appendU32(nil, 2))...)
	}
	go srv.write(buf)
	waitFor(t, func() bool { return c.QueueLen() == neferclient.RingSize }, "full ring")
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with a producer blocked on the full ring")
	}
}
