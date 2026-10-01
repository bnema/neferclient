package neferclient_test

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient"
)

// countingHandler is the measured system's input, not a double of a port: it
// only counts, so the test sees the cost of the library alone.
type countingHandler struct {
	neferclient.NopHandler
	fdReady, errs, frames int
	keys                  int
	lastText              string // set only by the allocation-free comparison below
}

func (h *countingHandler) FDReady(uint64)              { h.fdReady++ }
func (h *countingHandler) Error(error)                 { h.errs++ }
func (h *countingHandler) Frame(neferclient.SurfaceID) { h.frames++ }

// Key records the text through a comparison that does not allocate: the
// string is only built the first time a different text shows up.
func (h *countingHandler) Key(ev *neferclient.KeyEvent) {
	h.keys++
	if string(ev.Text) != h.lastText && ev.Pressed {
		h.lastText = string(ev.Text)
	}
}

// TestAllocDispatch measures the whole steady-state pipeline over a real
// socketpair: wire frames in, reader decode, queue, Dispatch (output state
// updates, an output commit and a watched-fd notification with epoll re-arm). testing's
// AllocsPerRun counts every goroutine, so reader and epoll allocations are
// included.
func TestAllocDispatch(t *testing.T) {
	skipUnderRace(t)
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}}, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	obj := srv.waitBound(7)

	const scaleEvents = 200
	var batch []byte
	for i := range scaleEvents {
		batch = append(batch, frame(obj.id, 3, appendU32(nil, uint32(1+i%2)))...)
	}
	batch = append(batch, frame(obj.id, 2, nil)...) // wl_output.done: commitOutput runs
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	require.NoError(t, err)
	defer unix.Close(efd)
	require.NoError(t, c.WatchFD(efd, 1))
	one := [8]byte{1}
	var drain [8]byte

	h := &countingHandler{}
	step := func() {
		if _, err := srv.conn.Write(batch); err != nil {
			t.Fatal(err)
		}
		_, _ = unix.Write(efd, one[:])
		for c.QueueLen() < scaleEvents+2 {
			runtime.Gosched()
		}
		_, _ = unix.Read(efd, drain[:])
		if err := c.Dispatch(h); err != nil {
			t.Fatal(err)
		}
	}
	step() // warm-up beyond AllocsPerRun's own: grow pools and buffers
	step()
	allocs := testing.AllocsPerRun(50, step)
	t.Logf("allocs per Dispatch of %d events: %v", scaleEvents+2, allocs)
	require.Zero(t, allocs)
	require.Positive(t, h.fdReady)
	require.Zero(t, h.errs)
}

// TestAllocPresent measures the steady-state present loop over a real
// socketpair: Present (all requests, including the frame callback), the peer
// answering the callback, reader decode, queue and Dispatch applying Frame.
// The peer parses and discards everything else.
func TestAllocPresent(t *testing.T) {
	skipUnderRace(t)
	globals := []wireGlobal{{1, wayland.CompositorInterface, 6}, {2, "zwp_linux_dmabuf_v1", 4},
		{3, "wp_linux_drm_syncobj_manager_v1", 1}, {4, "wp_viewporter", 1}, {5, "xdg_wm_base", 6}}
	c, srv := connectWire(t, globals, nil)
	s, err := c.NewToplevel("alloc", 64, 48)
	require.NoError(t, err)
	s.MarkPresentable(64, 48)

	surfaceID := s.SurfaceObjectID()
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, body []byte) {
		if obj == surfaceID && op == 3 && len(body) == 4 { // wl_surface.frame(new_id)
			srv.write(frame(binary.LittleEndian.Uint32(body), 0, appendU32(nil, 1)))
			srv.write(frame(1, 1, body)) // wl_display.delete_id: the id is recycled
		}
	}
	srv.mu.Unlock()

	memfd, err := unix.MemfdCreate("alloc", unix.MFD_CLOEXEC)
	require.NoError(t, err)
	defer unix.Close(memfd)
	require.NoError(t, s.ImportBuffer(1, &neferclient.Buffer{Width: 64, Height: 48, FourCC: 0x34325258,
		Planes: [4]neferclient.Plane{{FD: memfd, Stride: 256}}, PlaneCount: 1}))
	require.NoError(t, s.ImportTimeline(1, memfd))
	require.NoError(t, s.ImportTimeline(2, memfd))

	var point uint64
	damage := []neferclient.Rect{{X: 0, Y: 0, Width: 16, Height: 16}}
	present := func() {
		point++
		if err := s.Present(&neferclient.Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2,
			AcquirePoint: point, ReleasePoint: point, Damage: damage, Opaque: true}); err != nil {
			t.Fatal(err)
		}
	}

	// The request path: every request Present sends, over the real socket,
	// without the frame callback.
	s.SkipFrameRequests()
	requests := func() {
		s.ResetFrame()
		present()
	}
	for range 4 { // warm-up beyond AllocsPerRun's own
		requests()
	}
	allocs := testing.AllocsPerRun(100, requests)
	t.Logf("allocs per Present (requests only): %v", allocs)
	require.Zero(t, allocs)

	// The complete cycle adds the frame callback. wlturbo registers each
	// server-created wl_callback with two sync.Map stores (Context.proxies and
	// Display.objects) and a sync.Map store always allocates a node, so two
	// allocations per frame are inherent to the transport (wlturbo is out of
	// scope for this change). They are pinned so a regression above that shows.
	s.UseWireRequests()
	h := &countingHandler{}
	cycle := func() {
		present()
		for c.QueueLen() < 1 {
			runtime.Gosched()
		}
		if err := c.Dispatch(h); err != nil {
			t.Fatal(err)
		}
	}
	s.ResetFrame()
	for range 4 {
		cycle()
	}
	full := testing.AllocsPerRun(100, cycle)
	t.Logf("allocs per Present+frame round trip: %v (wlturbo callback registration)", full)
	require.LessOrEqual(t, full, 2.0)
	require.Positive(t, h.frames)
	require.Zero(t, h.errs)
}
