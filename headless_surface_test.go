package neferclient_test

import (
	"context"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient"
	neferclientmocks "github.com/bnema/neferclient/mocks"
)

func connectHeadless(t *testing.T) *neferclient.Conn {
	t.Helper()
	socket := startHeadless(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		c, err := neferclient.Connect(ctx, socket)
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			for len(c.Outputs()) == 0 { // the virtual output may arrive late
				select {
				case <-c.Wake():
					require.NoError(t, c.Dispatch(nil))
				case <-ctx.Done():
					t.Fatal("no output announced")
				}
			}
			return c
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHeadlessLayerSurface(t *testing.T) {
	c := connectHeadless(t)
	s, err := c.NewLayerSurface(neferclient.LayerConfig{
		Level: neferclient.LayerTop, Anchors: neferclient.AnchorTop | neferclient.AnchorLeft,
		Width: 200, Height: 50,
	})
	require.NoError(t, err)

	var configured, feedback bool
	var cw, ch int32
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Configure(s.ID(), mock.Anything, mock.Anything).Run(func(_ neferclient.SurfaceID, w, hh int32) {
		configured, cw, ch = true, w, hh
	}).Return().Maybe()
	h.EXPECT().FeedbackDone(s.ID()).Run(func(neferclient.SurfaceID) { feedback = true }).Return().Maybe()
	h.EXPECT().Scale(s.ID(), mock.Anything).Return().Maybe()
	dispatchUntil(t, c, h, func() bool { return configured && feedback })

	require.Equal(t, int32(200), cw)
	require.Equal(t, int32(50), ch)
	fb := s.Feedback()
	require.NotNil(t, fb)
	require.NotZero(t, fb.MainDevice)
	require.NotEmpty(t, fb.Formats)
	t.Logf("main device %#x, %d formats", fb.MainDevice, len(fb.Formats))
	pw, ph, err := s.PhysicalSize()
	require.NoError(t, err)
	require.Positive(t, pw)
	require.Positive(t, ph)
	require.NoError(t, s.SetInputRegion([]neferclient.Rect{{X: 0, Y: 0, Width: 10, Height: 10}}))
	require.NoError(t, s.Close())
	require.NoError(t, c.Roundtrip())
}

func TestHeadlessLockUnlock(t *testing.T) {
	c := connectHeadless(t)
	l, err := c.Lock()
	require.NoError(t, err)
	require.Error(t, l.Unlock(), "unlock before locked must be refused")

	var locked bool
	var surfaces []*neferclient.Surface
	for _, o := range c.Outputs() {
		s, err := l.NewSurface(o.Global)
		require.NoError(t, err)
		surfaces = append(surfaces, s)
	}
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Locked().Run(func() { locked = true }).Return().Maybe()
	h.EXPECT().Configure(mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	h.EXPECT().FeedbackDone(mock.Anything).Return().Maybe()
	h.EXPECT().Scale(mock.Anything, mock.Anything).Return().Maybe()
	dispatchUntil(t, c, h, func() bool { return locked })
	require.True(t, l.Locked())
	for _, s := range surfaces {
		w, hh, _ := s.Size()
		t.Logf("lock surface %d: %dx%d", s.ID(), w, hh)
	}
	require.Error(t, l.Close(), "a locked session is not closed, only unlocked")
	require.NoError(t, l.Unlock())
	require.False(t, l.Locked())
	for _, s := range surfaces {
		require.NoError(t, s.Close())
	}
	require.NoError(t, c.Roundtrip())
}

// TestHeadlessSeat binds the seat against a real compositor and drives it
// through a surface: the seat bind, capability handling and cursor request
// must not fail, whatever devices the headless backend offers.
func TestHeadlessSeat(t *testing.T) {
	c := connectHeadless(t)
	s, err := c.NewLayerSurface(neferclient.LayerConfig{
		Level: neferclient.LayerTop, Anchors: neferclient.AnchorTop | neferclient.AnchorLeft,
		Width: 100, Height: 40, Keyboard: neferclient.KeyboardOnDemand,
	})
	require.NoError(t, err)
	seat := c.Seat()
	require.NotNil(t, seat)
	seat.SetSecret(neferclient.NewSecretBuffer(16))
	seat.SetSecret(nil)

	var configured bool
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Configure(s.ID(), mock.Anything, mock.Anything).Run(func(neferclient.SurfaceID, int32, int32) { configured = true }).Return().Maybe()
	h.EXPECT().FeedbackDone(mock.Anything).Return().Maybe()
	h.EXPECT().Scale(mock.Anything, mock.Anything).Return().Maybe()
	h.EXPECT().Pointer(mock.Anything).Return().Maybe()
	h.EXPECT().Key(mock.Anything).Return().Maybe()
	h.EXPECT().KeyboardFocus(mock.Anything, mock.Anything).Return().Maybe()
	h.EXPECT().SecretChanged(mock.Anything).Return().Maybe()
	dispatchUntil(t, c, h, func() bool { return configured })
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(h))
	require.NoError(t, seat.SetCursor(neferclient.CursorPointer))
	require.NoError(t, s.Close())
}

// newUdmabuf returns a linear DMA-BUF made from a udmabuf; the test is skipped
// when the machine lacks /dev/udmabuf.
func newUdmabuf(t *testing.T, w, h int) int {
	t.Helper()
	size := (w*h*4 + 4095) &^ 4095
	mem, err := unix.MemfdCreate("udmabuf", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(mem) })
	require.NoError(t, unix.Ftruncate(mem, int64(size)))
	_, err = unix.FcntlInt(uintptr(mem), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK)
	require.NoError(t, err)
	dev, err := unix.Open("/dev/udmabuf", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no udmabuf: %v", err)
	}
	defer unix.Close(dev)
	req := struct {
		memfd, flags uint32
		offset, size uint64
	}{memfd: uint32(mem), size: uint64(size)}
	fd, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dev), 0x40187542, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		t.Skipf("UDMABUF_CREATE: %v", errno)
	}
	t.Cleanup(func() { _ = unix.Close(int(fd)) })
	return int(fd)
}

// newSyncobjFD returns an exported DRM syncobj for use as a timeline and a
// function that signals one of its points; the test is skipped when there is
// no render node.
func newSyncobjFD(t *testing.T) (fd int, signal func(point uint64)) {
	t.Helper()
	dev, err := unix.Open("/dev/dri/renderD128", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no render node: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(dev) })
	create := struct{ handle, flags uint32 }{} // struct drm_syncobj_create
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dev), 0xC00864BF, uintptr(unsafe.Pointer(&create))); errno != 0 {
		t.Skipf("SYNCOBJ_CREATE: %v", errno)
	}
	exp := struct { // struct drm_syncobj_handle, 24 bytes
		handle, flags uint32
		fd, pad       int32
		point         uint64
	}{handle: create.handle, fd: -1}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dev), 0xC01864C1, uintptr(unsafe.Pointer(&exp))); errno != 0 {
		t.Skipf("SYNCOBJ_HANDLE_TO_FD: %v", errno)
	}
	t.Cleanup(func() { _ = unix.Close(int(exp.fd)) })
	signal = func(point uint64) {
		t.Helper()
		// The kernel reads these through addresses stored in arg. Nothing
		// between taking them and the syscall can move the stack, and
		// KeepAlive holds them until it returns.
		handles, points := [1]uint32{create.handle}, [1]uint64{point}
		arg := struct { // struct drm_syncobj_timeline_array, 24 bytes
			handles, points uint64
			count, flags    uint32
		}{uint64(uintptr(unsafe.Pointer(&handles))), uint64(uintptr(unsafe.Pointer(&points))), 1, 0}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dev), 0xC01864CD, uintptr(unsafe.Pointer(&arg)))
		runtime.KeepAlive(&handles)
		runtime.KeepAlive(&points)
		if errno != 0 {
			t.Fatalf("SYNCOBJ_TIMELINE_SIGNAL: %v", errno)
		}
	}
	return int(exp.fd), signal
}

// TestHeadlessReconfigureWhilePending drops every import of a mapped surface
// while its frame is pending, imports again and presents again: the compositor
// must raise no protocol error (the connection stays usable) and the frame
// still arrives. The first commit's objects are destroyed before its acquire
// point is signalled: a compositor that waits for acquire points still holds
// that commit unapplied at that moment. Headless NeferWL applies commits
// without waiting, so there it only shows the requests are accepted.
func TestHeadlessReconfigureWhilePending(t *testing.T) {
	c := connectHeadless(t)
	const w, h = 200, 50
	s, err := c.NewLayerSurface(neferclient.LayerConfig{
		Level: neferclient.LayerTop, Anchors: neferclient.AnchorTop | neferclient.AnchorLeft, Width: w, Height: h,
	})
	require.NoError(t, err)
	var configured, feedback bool
	var frames int
	hd := neferclientmocks.NewMockHandler(t)
	hd.EXPECT().Configure(s.ID(), mock.Anything, mock.Anything).Run(func(neferclient.SurfaceID, int32, int32) { configured = true }).Return().Maybe()
	hd.EXPECT().FeedbackDone(s.ID()).Run(func(neferclient.SurfaceID) { feedback = true }).Return().Maybe()
	hd.EXPECT().Scale(s.ID(), mock.Anything).Return().Maybe()
	hd.EXPECT().Frame(s.ID()).Run(func(neferclient.SurfaceID) { frames++ }).Return().Maybe()
	dispatchUntil(t, c, hd, func() bool { return configured && feedback })
	pw, ph, err := s.PhysicalSize()
	require.NoError(t, err)

	dmabuf := newUdmabuf(t, int(pw), int(ph))
	// Separate acquire and release syncobjs keep their points independent.
	acquire, signal := newSyncobjFD(t)
	release, _ := newSyncobjFD(t)
	imports := func() {
		t.Helper()
		require.NoError(t, s.ImportBuffer(1, &neferclient.Buffer{Width: pw, Height: ph, FourCC: 0x34325258, PlaneCount: 1,
			Planes: [4]neferclient.Plane{{FD: dmabuf, Stride: uint32(pw) * 4}}}))
		require.NoError(t, s.ImportTimeline(1, acquire))
		require.NoError(t, s.ImportTimeline(2, release))
	}
	present := func(point uint64) {
		t.Helper()
		require.NoError(t, s.Present(&neferclient.Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2,
			AcquirePoint: point, ReleasePoint: point, Opaque: true}))
	}

	imports()
	present(1) // acquire point 1 is not signalled yet
	// No Dispatch ran since the commit: its frame is pending for the library.
	require.ErrorContains(t, s.Present(&neferclient.Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2}), "frame callback pending")
	require.NoError(t, s.DestroyImports())
	require.NoError(t, c.Roundtrip()) // a protocol error would fail here
	signal(1)                         // lets a waiting compositor apply the commit
	dispatchUntil(t, c, hd, func() bool { return frames == 1 })

	imports()
	signal(2)
	present(2)
	dispatchUntil(t, c, hd, func() bool { return frames == 2 })
	require.NoError(t, s.DestroyImports())
	require.NoError(t, s.Close())
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(hd))
}
