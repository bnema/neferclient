package neferclient

import (
	"encoding/binary"
	"testing"

	"github.com/bnema/go-wayland-bindings/client/linuxdrmsyncobj"
	"github.com/bnema/go-wayland-bindings/client/viewporter"
	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// presentable returns a configured surface that sends through req.
func presentable(req surfaceRequests, scale float64) *Surface {
	return &Surface{id: 1, req: req, width: 100, height: 50, scale: scale, configured: true, frameReady: true, fbDone: true,
		buffers:   map[uint64]*wayland.Buffer{1: nil},
		timelines: map[uint64]*linuxdrmsyncobj.WpLinuxDrmSyncobjTimeline{1: nil, 2: nil},
		viewport:  &viewporter.WpViewport{}}
}

func TestPresentOrdering(t *testing.T) {
	m := newMocksurfaceRequests(t)
	s := presentable(m, 1.5)
	// NotBefore makes testify fail a call that arrives before its predecessor.
	dest := m.EXPECT().setDestination(int32(100), int32(50)).Return(nil).Once()
	opaque := m.EXPECT().setOpaqueRegion(int32(100), int32(50), true).Return(nil).Once().NotBefore(dest)
	acquire := m.EXPECT().setAcquire(uint64(1), uint64(7)).Return(nil).Once().NotBefore(opaque)
	release := m.EXPECT().setRelease(uint64(2), uint64(8)).Return(nil).Once().NotBefore(acquire)
	attach := m.EXPECT().attach(uint64(1)).Return(nil).Once().NotBefore(release)
	damage := m.EXPECT().damageBuffer(Rect{1, 2, 3, 4}).Return(nil).Once().NotBefore(attach)
	frame := m.EXPECT().frame().Return(nil).Once().NotBefore(damage)
	m.EXPECT().commit().Return(nil).Once().NotBefore(frame)

	p := &Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2, AcquirePoint: 7, ReleasePoint: 8,
		Damage: []Rect{{1, 2, 3, 4}}, Opaque: true}
	require.NoError(t, s.Present(p))
	require.False(t, s.frameReady)
	require.ErrorContains(t, s.Present(p), "frame callback pending") // no further request: strict mock
}

func TestPresentSkipsUnchangedState(t *testing.T) {
	m := newMocksurfaceRequests(t)
	s := presentable(m, 2)
	m.EXPECT().setDestination(int32(100), int32(50)).Return(nil).Once()
	m.EXPECT().setOpaqueRegion(int32(100), int32(50), true).Return(nil).Once()
	m.EXPECT().setAcquire(mock.Anything, mock.Anything).Return(nil).Twice()
	m.EXPECT().setRelease(mock.Anything, mock.Anything).Return(nil).Twice()
	m.EXPECT().attach(uint64(1)).Return(nil).Twice()
	m.EXPECT().damageBuffer(Rect{0, 0, 200, 100}).Return(nil).Twice() // no damage: whole physical buffer
	m.EXPECT().frame().Return(nil).Twice()
	m.EXPECT().commit().Return(nil).Twice()
	p := &Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2, Opaque: true}
	require.NoError(t, s.Present(p))
	s.frameReady = true
	require.NoError(t, s.Present(p)) // destination and opaque region are not resent
}

func TestPresentRefusals(t *testing.T) {
	m := newMocksurfaceRequests(t) // strict: a refused Present sends nothing
	p := &Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2}
	for name, mutate := range map[string]func(*Surface){
		"before configure": func(s *Surface) { s.configured = false },
		"before feedback":  func(s *Surface) { s.fbDone = false },
		"unknown buffer":   func(s *Surface) { delete(s.buffers, 1) },
		"unknown timeline": func(s *Surface) { delete(s.timelines, 2) },
		"closed":           func(s *Surface) { s.closed = true },
		"bad geometry":     func(s *Surface) { s.scale = 0 },
		"fractional without viewporter": func(s *Surface) {
			s.scale, s.viewport = 1.5, nil
		},
	} {
		s := presentable(m, 1)
		mutate(s)
		require.Error(t, s.Present(p), name)
	}
}

func TestPhysicalSize(t *testing.T) {
	s := presentable(nil, 1.25)
	w, h, err := s.PhysicalSize()
	require.NoError(t, err)
	require.Equal(t, [2]int32{125, 63}, [2]int32{w, h}) // 62.5 rounds up
}

// feedbackHandler is the measured input, not a double: it keeps what the
// owner-side feedback parsing produced.
type feedbackHandler struct {
	NopHandler
	done int
	errs []error
}

func (h *feedbackHandler) FeedbackDone(SurfaceID) { h.done++ }
func (h *feedbackHandler) Error(err error)        { h.errs = append(h.errs, err) }

func tableFD(t *testing.T, entries []Format) int {
	t.Helper()
	fd, err := unix.MemfdCreate("table", unix.MFD_CLOEXEC)
	require.NoError(t, err)
	var b []byte
	for _, e := range entries {
		b = binary.NativeEndian.AppendUint32(b, e.FourCC)
		b = binary.NativeEndian.AppendUint32(b, 0)
		b = binary.NativeEndian.AppendUint64(b, e.Modifier)
	}
	_, err = unix.Write(fd, b)
	require.NoError(t, err)
	return fd
}

func postRound(c *Conn, s *Surface, fd int, size uint32, device uint64, indices ...uint16) {
	id := uint32(s.id)
	c.q.post(&event{kind: evFeedbackTable, id: id, fd: int32(fd), flags: size})
	c.q.post(&event{kind: evFeedbackMainDevice, id: id, dev: device})
	e := event{kind: evFeedbackFormats, id: id}
	for _, i := range indices {
		binary.NativeEndian.PutUint16(e.name[e.nameLen:], i)
		e.nameLen += 2
	}
	c.q.post(&e)
	c.q.post(&event{kind: evFeedbackDone, id: id})
}

func newOwnerConn() (*Conn, *Surface) {
	c := &Conn{q: newQueue(false), surfaces: map[SurfaceID]*Surface{}, watched: map[int32]uint64{}, efd: -1, epfd: -1}
	s := presentable(nil, 1)
	c.surfaces[s.id], s.c, s.fbDone = s, c, false
	return c, s
}

func TestFeedbackParsedOnOwner(t *testing.T) {
	c, s := newOwnerConn()
	table := []Format{{0x34325258, 0}, {0x34325241, 7}, {0x34325258, 1 << 40}}
	postRound(c, s, tableFD(t, table), 16*3, 0xe280, 2, 0)
	h := &feedbackHandler{}
	require.NoError(t, c.Dispatch(h))
	require.Empty(t, h.errs)
	require.Equal(t, 1, h.done)
	fb := s.Feedback()
	require.NotNil(t, fb)
	require.Equal(t, uint64(0xe280), fb.MainDevice)
	require.Equal(t, []Format{table[2], table[0]}, fb.Formats, "tranche order")

	// A second round replaces the first without leaking the first one's formats.
	postRound(c, s, tableFD(t, table), 16*3, 0xe281, 1)
	require.NoError(t, c.Dispatch(h))
	require.Equal(t, &Feedback{MainDevice: 0xe281, Formats: []Format{table[1]}}, s.Feedback())
}

func TestFeedbackBadIndexAndSize(t *testing.T) {
	c, s := newOwnerConn()
	postRound(c, s, tableFD(t, []Format{{1, 1}}), 16, 5, 3)
	postRound(c, s, tableFD(t, nil), 15, 5)
	h := &feedbackHandler{}
	require.NoError(t, c.Dispatch(h))
	require.Len(t, h.errs, 2)
	require.ErrorContains(t, h.errs[0], "out of range")
	require.ErrorContains(t, h.errs[1], "invalid dmabuf format table size")
}

func fdOpen(fd int) bool { _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); return err == nil }

func TestCloseClosesUndrainedFeedbackFDs(t *testing.T) {
	c, s := newOwnerConn()
	ringed := tableFD(t, nil)
	c.q.post(&event{kind: evFeedbackTable, id: uint32(s.id), fd: int32(ringed)})
	held := tableFD(t, nil)
	c.q.pausing = true
	c.q.post(&event{kind: evFeedbackTable, id: uint32(s.id), fd: int32(held)}) // parked in held
	require.True(t, fdOpen(ringed))
	require.True(t, fdOpen(held))
	c.q.closeFDs()
	require.False(t, fdOpen(ringed), "ring event's descriptor")
	require.False(t, fdOpen(held), "parked event's descriptor")

	// An event that is drained owns its descriptor no more: closeFDs must not
	// touch a number the process may have reused.
	drained := tableFD(t, nil)
	c.q.post(&event{kind: evFeedbackTable, id: 99, fd: int32(drained)}) // pausing: held
	c.q.pausing = false
	c.q.endPause()
	var ev event
	for c.q.pop(&ev) { // drain everything: the earlier events and this one
	}
	c.q.closeFDs()
	require.True(t, fdOpen(drained))
	_ = unix.Close(drained)
}

func TestEventForClosedSurfaceDropsFD(t *testing.T) {
	c, s := newOwnerConn()
	fd := tableFD(t, nil)
	s.closed = true
	c.q.post(&event{kind: evFeedbackTable, id: uint32(s.id), fd: int32(fd)})
	require.NoError(t, c.Dispatch(nil))
	require.False(t, fdOpen(fd))
}

func TestLockRules(t *testing.T) {
	c := &Conn{q: newQueue(false), surfaces: map[SurfaceID]*Surface{}}
	l := &Lock{c: c, gen: 1}
	c.curLock = l
	require.ErrorContains(t, l.Unlock(), "before the locked event")
	c.q.post(&event{kind: evLocked, id: 2}) // another generation: ignored
	require.NoError(t, c.Dispatch(nil))
	require.False(t, l.Locked())
	c.q.post(&event{kind: evLocked, id: 1})
	require.NoError(t, c.Dispatch(nil))
	require.True(t, l.Locked())
	require.ErrorContains(t, l.Close(), "use Unlock")
	c.q.post(&event{kind: evLockFinished, id: 1})
	require.NoError(t, c.Dispatch(nil))
	require.False(t, l.Locked())
	require.ErrorContains(t, l.Unlock(), "before the locked event")
}
