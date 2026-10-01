package neferclient_test

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient"
	neferclientmocks "github.com/bnema/neferclient/mocks"
)

const (
	dmabufGlobal  = 2 // names in surfaceGlobals
	syncobjGlobal = 3
)

// feedbackPeer scripts zwp_linux_dmabuf_feedback_v1 rounds and tracks the
// lifetime of every wl_buffer and timeline object the client creates, over the
// wire peer. The peer answers a destructor with delete_id, so ids are recycled
// as with a real compositor and lifetimes are tracked per live object. A
// destructor is counted only for a live object: a second destroy of one would
// leave created != destroyed or break the wire (the peer's delete_id for an id
// already released is a protocol error, which fails the roundtrip).
type feedbackPeer struct {
	t   *testing.T
	srv *wireServer

	mu        sync.Mutex
	feedback  uint32          // the surface's feedback object
	live      map[uint32]bool // buffers and timelines created and not yet destroyed
	params    map[uint32]bool // zwp_linux_buffer_params_v1 objects
	created   int
	destroyed int
	violation []string // an id created while its previous object is still live
}

func newFeedbackPeer(t *testing.T, srv *wireServer) *feedbackPeer {
	p := &feedbackPeer{t: t, srv: srv, live: map[uint32]bool{}, params: map[uint32]bool{}}
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, body []byte) {
		srv.mu.Lock()
		dmabuf, syncobj := srv.bound[dmabufGlobal].id, srv.bound[syncobjGlobal].id
		srv.mu.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		create := func(id uint32) {
			if p.live[id] {
				p.violation = append(p.violation, fmt.Sprintf("object %d created twice", id))
			}
			p.live[id] = true
			p.created++
		}
		switch {
		case obj == dmabuf && op == 1 && len(body) == 4: // create_params(new_id)
			p.params[binary.LittleEndian.Uint32(body)] = true
		case obj == dmabuf && op == 2 && len(body) == 4: // get_default_feedback(new_id)
			p.feedback = binary.LittleEndian.Uint32(body)
		case obj == syncobj && op == 2 && len(body) == 4: // import_timeline(new_id, fd)
			create(binary.LittleEndian.Uint32(body))
		case p.params[obj] && op == 3 && len(body) == 20: // params.create_immed(new_id, w, h, format, flags)
			create(binary.LittleEndian.Uint32(body))
		case p.params[obj] && op == 0 && len(body) == 0: // params.destroy: the id may be reused
			delete(p.params, obj)
		case op == 0 && len(body) == 0 && p.live[obj]: // destructor of a live buffer or timeline
			delete(p.live, obj)
			p.destroyed++
		}
	}
	srv.mu.Unlock()
	return p
}

// stats returns how many tracked objects were created, destroyed and are live.
func (p *feedbackPeer) stats() (created, destroyed, live int, violations []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.created, p.destroyed, len(p.live), append([]string(nil), p.violation...)
}

// round sends one complete feedback round: the format table, the main device
// and one tranche listing the given table indices.
func (p *feedbackPeer) round(table []neferclient.Format, device uint64, indices ...uint16) {
	p.t.Helper()
	p.mu.Lock()
	fb := p.feedback
	p.mu.Unlock()
	require.NotZero(p.t, fb, "the client requested its feedback object")

	var entries []byte
	for _, f := range table {
		entries = binary.NativeEndian.AppendUint32(entries, f.FourCC)
		entries = binary.NativeEndian.AppendUint32(entries, 0)
		entries = binary.NativeEndian.AppendUint64(entries, f.Modifier)
	}
	fd, err := unix.MemfdCreate("table", unix.MFD_CLOEXEC)
	require.NoError(p.t, err)
	defer unix.Close(fd)
	_, err = unix.Write(fd, entries)
	require.NoError(p.t, err)
	_, _, err = p.srv.conn.WriteMsgUnix(frame(fb, 1, appendU32(nil, uint32(len(entries)))), unix.UnixRights(fd), nil)
	require.NoError(p.t, err)

	dev := appendU32(nil, 8)
	dev = binary.NativeEndian.AppendUint64(dev, device)
	p.srv.write(frame(fb, 2, dev))
	var idx []byte
	for _, i := range indices {
		idx = binary.NativeEndian.AppendUint16(idx, i)
	}
	n := len(idx)
	for len(idx)%4 != 0 {
		idx = append(idx, 0)
	}
	p.srv.write(frame(fb, 5, append(appendU32(nil, uint32(n)), idx...)))
	p.srv.write(frame(fb, 3, nil)) // tranche_done
	p.srv.write(frame(fb, 0, nil)) // done
}

func connectFeedbackSurface(t *testing.T) (*neferclient.Conn, *neferclient.Surface, *feedbackPeer) {
	t.Helper()
	c, srv := connectWire(t, surfaceGlobals, surfaceOutputs)
	peer := newFeedbackPeer(t, srv)
	s, err := c.NewToplevel("feedback", 64, 48)
	require.NoError(t, err)
	require.NoError(t, c.Roundtrip())
	return c, s, peer
}

// TestFeedbackChangeAfterStartup: every complete round is reported; Equal tells
// a changed main device or format table from a repeated round, and Clone keeps
// the feedback a renderer was built from valid across later rounds.
func TestFeedbackChangeAfterStartup(t *testing.T) {
	c, s, peer := connectFeedbackSurface(t)
	require.Nil(t, s.Feedback())

	var done int
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().FeedbackDone(s.ID()).Run(func(neferclient.SurfaceID) { done++ }).Return()
	wait := func(n int) {
		t.Helper()
		dispatchUntil(t, c, h, func() bool { return done >= n })
	}
	table := []neferclient.Format{{FourCC: 0x34325258}, {FourCC: 0x34325241, Modifier: 7}, {FourCC: 0x34325258, Modifier: 1 << 40}}

	peer.round(table, 0xe280, 0, 1)
	wait(1)
	inUse := s.Feedback().Clone() // what the first renderer was built from
	require.Equal(t, &neferclient.Feedback{MainDevice: 0xe280, Formats: table[:2]}, inUse)
	require.True(t, s.Feedback().Equal(inUse))

	peer.round(table, 0xe280, 0, 1) // the compositor repeats its preference
	wait(2)
	require.True(t, s.Feedback().Equal(inUse), "a repeated round is not a change")

	peer.round(table, 0xe281, 0, 1) // another main device
	wait(3)
	require.False(t, s.Feedback().Equal(inUse), "main device changed")
	require.Equal(t, uint64(0xe280), inUse.MainDevice, "the clone is unaffected by the round")

	peer.round(table, 0xe281, 0, 1, 2) // same device, more formats
	wait(4)
	changed := s.Feedback().Clone()
	require.Equal(t, table, changed.Formats)

	peer.round(table, 0xe281, 2, 1, 0) // same formats, other order: tranche order matters
	wait(5)
	require.False(t, s.Feedback().Equal(changed), "format order changed")
	require.Len(t, changed.Formats, 3, "the clone keeps its own formats")
	require.Equal(t, table[0], changed.Formats[0])
	// The library double-buffers its format storage: this round writes the
	// buffer the earlier round used, which a shallow clone would share.
	peer.round(table, 0xe281, 2, 2, 2)
	wait(6)
	require.Equal(t, table, changed.Formats, "the clone does not alias library storage")
	require.Equal(t, table[:2], inUse.Formats)

	var none *neferclient.Feedback
	require.True(t, none.Equal(nil))
	require.False(t, none.Equal(inUse))
	require.False(t, inUse.Equal(nil))
	require.Nil(t, none.Clone())
}

// TestDestroyImportsWithPendingFrame: the buffers and timelines are dropped
// while a frame is pending, the old ids are refused, no request goes to a
// destroyed object, the new imports present once the frame fired, and every
// object is destroyed exactly once, Close included.
func TestDestroyImportsWithPendingFrame(t *testing.T) {
	c, s, peer := connectFeedbackSurface(t)
	s.MarkPresentable(64, 48)
	surfaceID := s.SurfaceObjectID()
	var callback, commits int
	var cbMu sync.Mutex
	peer.srv.mu.Lock()
	inner := peer.srv.hook
	peer.srv.hook = func(obj uint32, op uint16, body []byte) {
		inner(obj, op, body)
		if obj == surfaceID {
			cbMu.Lock()
			defer cbMu.Unlock()
			switch {
			case op == 3 && len(body) == 4:
				callback = int(binary.LittleEndian.Uint32(body))
			case op == 6:
				commits++
			}
		}
	}
	peer.srv.mu.Unlock()

	memfd, err := unix.MemfdCreate("imports", unix.MFD_CLOEXEC)
	require.NoError(t, err)
	defer unix.Close(memfd)
	buf := &neferclient.Buffer{Width: 64, Height: 48, FourCC: 0x34325258, PlaneCount: 1,
		Planes: [4]neferclient.Plane{{FD: memfd, Stride: 256}}}
	imports := func() {
		t.Helper()
		require.NoError(t, s.ImportBuffer(1, buf))
		require.NoError(t, s.ImportBuffer(2, buf))
		require.NoError(t, s.ImportTimeline(1, memfd))
		require.NoError(t, s.ImportTimeline(2, memfd))
	}
	imports()
	p := &neferclient.Present{Buffer: 1, AcquireTimeline: 1, ReleaseTimeline: 2, AcquirePoint: 1, ReleasePoint: 2}
	require.NoError(t, s.Present(p))
	require.NoError(t, c.Roundtrip())
	cbMu.Lock()
	cb, committed := callback, commits
	cbMu.Unlock()
	require.NotZero(t, cb, "a frame is pending")
	require.Positive(t, committed)
	created, destroyed, live, bad := peer.stats()
	require.Equal(t, [3]int{4, 0, 4}, [3]int{created, destroyed, live})
	require.Empty(t, bad)

	open := openFDs(t)
	require.NoError(t, s.DestroyImports())
	require.NoError(t, s.DestroyImports(), "nothing left to destroy")
	require.NoError(t, c.Roundtrip())
	require.Equal(t, open, openFDs(t), "no descriptor is closed or leaked")
	created, destroyed, live, bad = peer.stats()
	require.Equal(t, [3]int{4, 4, 0}, [3]int{created, destroyed, live}, "every old object destroyed exactly once")
	require.Empty(t, bad)

	// The old ids are gone: Present, DestroyBuffer and DestroyTimeline send nothing.
	require.ErrorContains(t, s.Present(p), "frame callback pending") // gate first
	require.ErrorContains(t, s.DestroyBuffer(1), "unknown buffer 1")
	require.ErrorContains(t, s.DestroyTimeline(1), "unknown timeline 1")

	// The frame fires; the surface is still presentable with re-imported ids.
	h := neferclientmocks.NewMockHandler(t)
	var framed bool
	h.EXPECT().Frame(s.ID()).Run(func(neferclient.SurfaceID) { framed = true }).Return()
	peer.srv.write(frame(uint32(cb), 0, appendU32(nil, 1)))
	peer.srv.write(frame(1, 1, appendU32(nil, uint32(cb))))
	dispatchUntil(t, c, h, func() bool { return framed })
	require.ErrorContains(t, s.Present(p), "unknown buffer 1")
	imports()
	require.NoError(t, s.Present(p))
	require.NoError(t, c.Roundtrip())
	created, destroyed, live, bad = peer.stats()
	require.Equal(t, [3]int{8, 4, 4}, [3]int{created, destroyed, live})
	require.Empty(t, bad)

	// Close destroys what is imported now, and only that.
	require.NoError(t, s.Close())
	require.ErrorIs(t, s.DestroyImports(), neferclient.ErrClosed)
	require.NoError(t, c.Roundtrip())
	created, destroyed, live, bad = peer.stats()
	require.Equal(t, [3]int{8, 8, 0}, [3]int{created, destroyed, live}, "Close destroys the new objects once")
	require.Empty(t, bad)
	require.NoError(t, c.Dispatch(nil))
}
