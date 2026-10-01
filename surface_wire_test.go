package neferclient_test

import (
	"encoding/binary"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient"
)

var surfaceGlobals = []wireGlobal{{1, "wl_compositor", 6}, {2, "zwp_linux_dmabuf_v1", 4},
	{3, "wp_linux_drm_syncobj_manager_v1", 1}, {4, "wp_viewporter", 1}, {5, "xdg_wm_base", 6},
	{6, "ext_session_lock_manager_v1", 1}, {7, outputIface, 4}}

var surfaceOutputs = map[uint32]outputSpec{7: {"DP-1", 1, 640, 480}}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	return len(ents)
}

// GO-009 and GO-012: failure paths close each duplicated descriptor exactly
// once, and descriptor 0 is never touched.
func TestImportErrorPathsCloseDupsOnce(t *testing.T) {
	c, _ := connectWire(t, surfaceGlobals, surfaceOutputs)
	s, err := c.NewToplevel("fd", 64, 48)
	require.NoError(t, err)
	memfd, err := unix.MemfdCreate("fd", unix.MFD_CLOEXEC)
	require.NoError(t, err)
	defer unix.Close(memfd)
	_, err = unix.FcntlInt(0, unix.F_GETFD, 0)
	require.NoError(t, err, "stdin is open for the test")

	before := openFDs(t)
	// Plane 0 duplicates, plane 1 cannot: the first duplicate must be closed.
	err = s.ImportBuffer(1, &neferclient.Buffer{Width: 64, Height: 48, FourCC: 1, PlaneCount: 2,
		Planes: [4]neferclient.Plane{{FD: memfd}, {FD: 9999}}})
	require.Error(t, err)
	require.Equal(t, before, openFDs(t), "no descriptor leaked")
	_, err = unix.FcntlInt(0, unix.F_GETFD, 0)
	require.NoError(t, err, "descriptor 0 untouched")
	require.Error(t, s.ImportTimeline(1, 9999))
	require.Equal(t, before, openFDs(t))

	// Success paths: the request consumes the duplicates.
	require.NoError(t, s.ImportBuffer(1, &neferclient.Buffer{Width: 64, Height: 48, FourCC: 1, PlaneCount: 1,
		Planes: [4]neferclient.Plane{{FD: memfd, Stride: 256}}}))
	require.NoError(t, s.ImportTimeline(1, memfd))
	require.Equal(t, before, openFDs(t), "sent descriptors are closed, the caller's stays open")
	require.Error(t, s.ImportBuffer(1, &neferclient.Buffer{Width: 64, Height: 48, FourCC: 1, PlaneCount: 1,
		Planes: [4]neferclient.Plane{{FD: memfd}}}), "duplicate id")
	require.Equal(t, before, openFDs(t))
}

// requestLog counts the commits the wire peer sees.
type requestLog struct{ commits atomic.Int32 }

// GO-003: before the first Present the region is staged without a commit; a
// lock surface refuses it.
func TestSetInputRegionCommitsOnlyOnceMapped(t *testing.T) {
	c, srv := connectWire(t, surfaceGlobals, surfaceOutputs)
	s, err := c.NewToplevel("input", 64, 48)
	require.NoError(t, err)
	require.NoError(t, c.Roundtrip()) // the peer consumed the initial commit
	var log requestLog
	surfaceID := s.SurfaceObjectID()
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, _ []byte) {
		if obj == surfaceID && op == 6 {
			log.commits.Add(1)
		}
	}
	srv.mu.Unlock()
	s.MarkPresentable(64, 48)

	require.NoError(t, s.SetInputRegion([]neferclient.Rect{{X: 0, Y: 0, Width: 4, Height: 4}}))
	require.NoError(t, c.Roundtrip())
	require.Zero(t, log.commits.Load(), "unmapped surface: staged, not committed")

	s.MarkPresented()
	require.NoError(t, s.SetInputRegion([]neferclient.Rect{{X: 1, Y: 1, Width: 4, Height: 4}}))
	require.NoError(t, c.Roundtrip())
	require.Equal(t, int32(1), log.commits.Load(), "mapped surface: committed")
	require.NoError(t, s.SetInputRegion([]neferclient.Rect{{X: 1, Y: 1, Width: 4, Height: 4}}))
	require.NoError(t, c.Roundtrip())
	require.Equal(t, int32(1), log.commits.Load(), "unchanged region sends nothing")

	l, err := c.Lock()
	require.NoError(t, err)
	ls, err := l.NewSurface(7)
	require.NoError(t, err)
	require.Error(t, ls.SetInputRegion(nil), "lock surfaces take all input")
}

// GO-007: one lock surface per output.
func TestLockRefusesSecondSurfacePerOutput(t *testing.T) {
	c, _ := connectWire(t, surfaceGlobals, surfaceOutputs)
	l, err := c.Lock()
	require.NoError(t, err)
	first, err := l.NewSurface(7)
	require.NoError(t, err)
	_, err = l.NewSurface(7)
	require.ErrorContains(t, err, "already has a lock surface")
	require.NoError(t, first.Close())
	again, err := l.NewSurface(7)
	require.NoError(t, err, "after closing the first one the output is free again")
	require.NoError(t, again.Close())
}

// watchLock records the ext_session_lock_v1 id the peer saw requested and
// counts its destroy requests.
func watchLock(t *testing.T, srv *wireServer) (id func() uint32, destroys *atomic.Int32) {
	t.Helper()
	var lockID atomic.Uint32
	destroys = new(atomic.Int32)
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, body []byte) {
		srv.mu.Lock()
		manager := srv.bound[6].id // the lock manager is bound by the first Lock call
		srv.mu.Unlock()
		switch {
		case obj == manager && op == 1 && len(body) == 4: // lock(new_id)
			lockID.Store(binary.LittleEndian.Uint32(body))
		case obj == lockID.Load() && obj != 0 && op == 0: // destroy
			destroys.Add(1)
		}
	}
	srv.mu.Unlock()
	return lockID.Load, destroys
}

// GO-006: a locked event already on the wire when Close runs wins: destroying
// a locked lock is a protocol error, so Close reports it instead.
func TestLockCloseSeesLockedInFlight(t *testing.T) {
	c, srv := connectWire(t, surfaceGlobals, surfaceOutputs)
	lockID, destroys := watchLock(t, srv)

	l, err := c.Lock()
	require.NoError(t, err)
	require.NoError(t, c.Roundtrip())  // the peer saw the lock request
	srv.write(frame(lockID(), 0, nil)) // ext_session_lock_v1.locked, not yet dispatched

	require.ErrorContains(t, l.Close(), "use Unlock")
	require.Zero(t, destroys.Load(), "no destroy request was sent")
	require.True(t, l.Locked(), "treated as locked")
	require.NoError(t, c.Roundtrip())
	require.Zero(t, destroys.Load())
}

func TestLockCloseDestroysUnlockedLock(t *testing.T) {
	c, srv := connectWire(t, surfaceGlobals, surfaceOutputs)
	_, destroys := watchLock(t, srv)
	l, err := c.Lock()
	require.NoError(t, err)
	require.NoError(t, l.Close())
	require.NoError(t, c.Roundtrip())
	require.Equal(t, int32(1), destroys.Load())
	require.NoError(t, l.Close(), "idempotent")
}

// GO-002 reproduction, enabled once wlturbo handles delete_id for objects the
// server creates: closing a surface with a pending frame callback must not turn
// the compositor's later callback done and delete_id into a transport error.
func TestCloseWithPendingFrame(t *testing.T) {
	t.Skip("needs wlturbo server-side delete_id handling")
	c, srv := connectWire(t, surfaceGlobals, surfaceOutputs)
	s, err := c.NewToplevel("close", 64, 48)
	require.NoError(t, err)
	s.MarkPresentable(64, 48)
	surfaceID := s.SurfaceObjectID()
	var callback atomic.Uint32
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, body []byte) {
		if obj == surfaceID && op == 3 && len(body) == 4 {
			callback.Store(binary.LittleEndian.Uint32(body))
		}
	}
	srv.mu.Unlock()
	memfd, err := unix.MemfdCreate("close", unix.MFD_CLOEXEC)
	require.NoError(t, err)
	defer unix.Close(memfd)
	require.NoError(t, s.ImportBuffer(1, &neferclient.Buffer{Width: 64, Height: 48, FourCC: 1, PlaneCount: 1,
		Planes: [4]neferclient.Plane{{FD: memfd, Stride: 256}}}))
	require.NoError(t, s.ImportTimeline(1, memfd))
	require.NoError(t, s.ImportTimeline(2, memfd))
	require.NoError(t, s.Present(&neferclient.Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2, AcquirePoint: 1, ReleasePoint: 2}))
	require.NoError(t, c.Roundtrip())
	require.NoError(t, s.Close())

	srv.write(frame(callback.Load(), 0, appendU32(nil, 1)))
	srv.write(frame(1, 1, appendU32(nil, callback.Load())))
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(nil))
}
