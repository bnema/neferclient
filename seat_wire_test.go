package neferclient_test

import (
	"encoding/binary"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient"
	neferclientmocks "github.com/bnema/neferclient/mocks"
)

const (
	seatGlobal   = 8
	shapesGlobal = 9
)

var seatGlobals = append(append([]wireGlobal{}, surfaceGlobals...),
	wireGlobal{seatGlobal, "wl_seat", 9}, wireGlobal{shapesGlobal, "wp_cursor_shape_manager_v1", 2})

// seatPeer is the compositor side of a seat: it learns the object ids the
// client creates and scripts input events.
type seatPeer struct {
	t   *testing.T
	srv *wireServer

	mu                       sync.Mutex
	pointer, keyboard, shape uint32
	shapeSerial, shapeValue  uint32
	shapeSets                int
	pointerReleased          bool
}

func newSeatPeer(t *testing.T, srv *wireServer) *seatPeer {
	p := &seatPeer{t: t, srv: srv}
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, body []byte) {
		srv.mu.Lock()
		seat, shapes := srv.bound[seatGlobal].id, srv.bound[shapesGlobal].id
		srv.mu.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case obj == seat && seat != 0 && op == 0 && len(body) == 4:
			p.pointer = binary.LittleEndian.Uint32(body)
		case obj == seat && seat != 0 && op == 1 && len(body) == 4:
			p.keyboard = binary.LittleEndian.Uint32(body)
		case obj == shapes && shapes != 0 && op == 1 && len(body) == 8:
			p.shape = binary.LittleEndian.Uint32(body)
		case obj == p.shape && p.shape != 0 && op == 1 && len(body) == 8:
			p.shapeSerial = binary.LittleEndian.Uint32(body)
			p.shapeValue = binary.LittleEndian.Uint32(body[4:])
			p.shapeSets++
		case obj == p.pointer && p.pointer != 0 && op == 1:
			p.pointerReleased = true
		}
	}
	srv.mu.Unlock()
	return p
}

func (p *seatPeer) ids() (pointer, keyboard uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pointer, p.keyboard
}

func (p *seatPeer) seatID() uint32 {
	p.srv.mu.Lock()
	defer p.srv.mu.Unlock()
	return p.srv.bound[seatGlobal].id
}

func fixed(v float64) uint32 { return uint32(int32(v * 256)) }

func negU32(v int32) uint32 { return uint32(-v) }

func (p *seatPeer) capabilities(caps uint32) {
	p.srv.write(frame(p.seatID(), 0, appendU32(nil, caps)))
}

// keymap sends wl_keyboard.keymap with testdata/fr.xkb in a descriptor.
func (p *seatPeer) keymap(keyboard uint32, format uint32) {
	p.t.Helper()
	data, err := os.ReadFile("testdata/fr.xkb")
	require.NoError(p.t, err)
	data = append(data, 0)
	fd, err := unix.MemfdCreate("keymap", unix.MFD_CLOEXEC)
	require.NoError(p.t, err)
	defer unix.Close(fd)
	_, err = unix.Write(fd, data)
	require.NoError(p.t, err)
	body := appendU32(nil, format)
	body = appendU32(body, uint32(len(data)))
	_, _, err = p.srv.conn.WriteMsgUnix(frame(keyboard, 0, body), unix.UnixRights(fd), nil)
	require.NoError(p.t, err)
}

func (p *seatPeer) keyboardEnter(kb, surface uint32) {
	body := appendU32(appendU32(nil, 1), surface)
	body = appendU32(body, 0) // empty keys array
	p.srv.write(frame(kb, 1, body))
}

func (p *seatPeer) key(kb, code, state uint32) []byte {
	b := appendU32(appendU32(nil, 1), 0)
	b = appendU32(appendU32(b, code), state)
	return frame(kb, 3, b)
}

func seatSetup(t *testing.T) (*neferclient.Conn, *wireServer, *seatPeer, *neferclient.Surface) {
	t.Helper()
	c, srv := connectWire(t, seatGlobals, surfaceOutputs)
	peer := newSeatPeer(t, srv)
	s, err := c.NewToplevel("seat", 64, 48)
	require.NoError(t, err)
	require.NotNil(t, c.Seat())
	require.NoError(t, c.Roundtrip())
	peer.capabilities(3)
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(neferclient.NopHandler{}))
	require.NoError(t, c.Roundtrip())
	return c, srv, peer, s
}

func TestSeatPointerEvents(t *testing.T) {
	c, srv, peer, s := seatSetup(t)
	pointer, keyboard := peer.ids()
	require.NotZero(t, pointer)
	require.NotZero(t, keyboard)
	surf := s.SurfaceObjectID()

	var got []neferclient.PointerEvent
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Pointer(mock.Anything).Run(func(ev *neferclient.PointerEvent) { got = append(got, *ev) }).Return()

	// A motion outside any surface is ignored; the enter binds the position.
	srv.write(frame(pointer, 2, append(appendU32(appendU32(nil, 0), fixed(1)), appendU32(nil, fixed(2))...)))
	b := appendU32(appendU32(nil, 77), surf)
	b = appendU32(appendU32(b, fixed(10.5)), fixed(20.25))
	srv.write(frame(pointer, 0, b)) // enter
	srv.write(frame(pointer, 2, append(appendU32(appendU32(nil, 0), fixed(11)), appendU32(nil, fixed(21))...)))
	srv.write(frame(pointer, 3, appendU32(appendU32(appendU32(appendU32(nil, 78), 0), 0x110), 1))) // button press
	srv.write(frame(pointer, 4, appendU32(appendU32(appendU32(nil, 0), 0), fixed(15))))            // axis vertical
	srv.write(frame(pointer, 9, appendU32(appendU32(nil, 1), negU32(120))))                        // value120 horizontal
	srv.write(frame(pointer, 7, appendU32(appendU32(nil, 0), 0)))                                  // axis stop
	srv.write(frame(pointer, 1, appendU32(appendU32(nil, 79), surf)))                              // leave
	srv.write(frame(pointer, 2, append(appendU32(appendU32(nil, 0), fixed(3)), appendU32(nil, fixed(4))...)))
	require.NoError(t, c.Roundtrip())
	dispatchUntil(t, c, h, func() bool { return len(got) >= 7 })

	require.Len(t, got, 7)
	id := s.ID()
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerEnter, X: 10.5, Y: 20.25}, got[0])
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerMotion, X: 11, Y: 21}, got[1])
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerButton, X: 11, Y: 21, Button: 0x110, Pressed: true}, got[2])
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerAxis, X: 11, Y: 21, Axis: 0, DY: 15}, got[3])
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerAxis, X: 11, Y: 21, Axis: 1, Value120: -120}, got[4])
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerAxisStop, X: 11, Y: 21}, got[5])
	require.Equal(t, neferclient.PointerEvent{Surface: id, Kind: neferclient.PointerLeave, X: 11, Y: 21}, got[6])
}

func TestSeatCursorShape(t *testing.T) {
	c, srv, peer, s := seatSetup(t)
	pointer, _ := peer.ids()
	seat := c.Seat()
	require.NoError(t, seat.SetCursor(neferclient.CursorPointer), "outside: no-op")
	require.NoError(t, c.Roundtrip())
	require.Zero(t, peer.shapeSets)

	enter := func(serial uint32) {
		b := appendU32(appendU32(nil, serial), s.SurfaceObjectID())
		srv.write(frame(pointer, 0, appendU32(appendU32(b, fixed(1)), fixed(1))))
		require.NoError(t, c.Roundtrip())
		require.NoError(t, c.Dispatch(neferclient.NopHandler{}))
	}
	enter(40)
	require.NoError(t, seat.SetCursor(neferclient.CursorText))
	require.NoError(t, seat.SetCursor(neferclient.CursorText), "unchanged: nothing sent")
	require.NoError(t, c.Roundtrip())
	peer.mu.Lock()
	require.Equal(t, 1, peer.shapeSets)
	require.Equal(t, uint32(40), peer.shapeSerial)
	require.Equal(t, uint32(neferclient.CursorText), peer.shapeValue)
	peer.mu.Unlock()

	enter(41) // a new enter resets the shape and carries a new serial
	require.NoError(t, seat.SetCursor(neferclient.CursorText))
	require.NoError(t, c.Roundtrip())
	peer.mu.Lock()
	require.Equal(t, 2, peer.shapeSets)
	require.Equal(t, uint32(41), peer.shapeSerial)
	peer.mu.Unlock()
	require.Error(t, seat.SetCursor(0))
	require.Error(t, seat.SetCursor(neferclient.CursorAllResize+1))
}

func TestSeatKeyboardFocusTextAndSecret(t *testing.T) {
	c, srv, peer, s := seatSetup(t)
	_, kb := peer.ids()
	surf := s.SurfaceObjectID()
	seat := c.Seat()

	var keys []neferclient.KeyEvent
	var texts []string
	var focus []bool
	var changed []int
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Key(mock.Anything).Run(func(ev *neferclient.KeyEvent) {
		keys = append(keys, *ev)
		texts = append(texts, string(ev.Text)) // the only place text is copied: the assertion of the test
	}).Return()
	h.EXPECT().KeyboardFocus(s.ID(), mock.Anything).Run(func(_ neferclient.SurfaceID, f bool) { focus = append(focus, f) }).Return()
	h.EXPECT().SecretChanged(mock.Anything).Run(func(n int) { changed = append(changed, n) }).Return()

	peer.keymap(kb, 1) // French AZERTY: evdev 16 is "a"
	srv.write(frame(kb, 5, append(appendU32(nil, 0), appendU32(nil, 0)...)))
	peer.keyboardEnter(kb, surf)
	srv.write(peer.key(kb, 16, 1))
	srv.write(peer.key(kb, 16, 0))
	require.NoError(t, c.Roundtrip())
	dispatchUntil(t, c, h, func() bool { return len(keys) >= 2 })
	require.Equal(t, []bool{true}, focus)
	require.Equal(t, "a", texts[0])
	require.True(t, keys[0].Pressed)
	require.Equal(t, s.ID(), keys[0].Surface)
	require.Equal(t, "", texts[1])
	require.False(t, keys[1].Pressed)
	require.Empty(t, changed)

	// With a secret buffer: the text goes only into it.
	buf := neferclient.NewSecretBuffer(8)
	seat.SetSecret(buf)
	srv.write(peer.key(kb, 16, 1))
	srv.write(peer.key(kb, 16, 0))
	srv.write(peer.key(kb, 30, 1)) // "q" on AZERTY
	srv.write(peer.key(kb, 30, 0))
	srv.write(peer.key(kb, 14, 1)) // BackSpace
	srv.write(peer.key(kb, 14, 0))
	srv.write(peer.key(kb, 28, 1)) // Return: delivered, no text
	require.NoError(t, c.Roundtrip())
	dispatchUntil(t, c, h, func() bool { return len(keys) >= 2+7 })
	for _, text := range texts[2:] {
		require.Empty(t, text)
	}
	require.Equal(t, "a", string(buf.Bytes()))
	require.Equal(t, 1, buf.Len())
	require.Equal(t, []int{1, 2, 1}, changed)

	srv.write(frame(kb, 2, appendU32(appendU32(nil, 5), surf))) // leave
	require.NoError(t, c.Roundtrip())
	dispatchUntil(t, c, h, func() bool { return len(focus) >= 2 })
	require.Equal(t, []bool{true, false}, focus)
	buf.Wipe()
}

// TestSeatKeymapFDClosedOnClose: a keymap descriptor queued but never drained
// is closed by Close. The descriptor count after Close must not depend on
// whether a keymap was left in the queue.
func TestSeatKeymapFDClosedOnClose(t *testing.T) {
	run := func(withKeymap bool) (after int) {
		t.Run("run", func(t *testing.T) {
			c, _, peer, _ := seatSetup(t)
			_, kb := peer.ids()
			if withKeymap {
				before := openFDs(t)
				peer.keymap(kb, 1)
				require.NoError(t, c.Roundtrip()) // queued, not dispatched
				require.Greater(t, openFDs(t), before, "the queued event owns a descriptor")
			}
			require.NoError(t, c.Close())
			after = openFDs(t)
		})
		return after
	}
	without := run(false)
	require.Equal(t, without, run(true), "descriptor closed with the connection")
}

// TestSeatKeymapFDClosedOnBadFormat: an unknown keymap format is reported and
// its descriptor closed; no_keymap (0) is closed silently.
func TestSeatKeymapFDClosedOnBadFormat(t *testing.T) {
	c, _, peer, _ := seatSetup(t)
	_, kb := peer.ids()
	before := openFDs(t)

	peer.keymap(kb, 0) // no_keymap: no error, no leak
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(neferclientmocks.NewMockHandler(t))) // any Handler call fails
	require.Equal(t, before, openFDs(t))

	peer.keymap(kb, 7) // unknown format
	require.NoError(t, c.Roundtrip())
	var errs int
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Error(mock.Anything).Run(func(error) { errs++ }).Return()
	dispatchUntil(t, c, h, func() bool { return errs == 1 })
	require.Equal(t, before, openFDs(t))
}

func TestSeatCapabilityLossReleasesDevices(t *testing.T) {
	c, srv, peer, s := seatSetup(t)
	pointer, kb := peer.ids()
	b := appendU32(appendU32(nil, 5), s.SurfaceObjectID())
	srv.write(frame(pointer, 0, appendU32(appendU32(b, fixed(1)), fixed(1))))
	peer.keyboardEnter(kb, s.SurfaceObjectID())
	require.NoError(t, c.Roundtrip())

	var got []neferclient.PointerEvent
	var focus []bool
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Pointer(mock.Anything).Run(func(ev *neferclient.PointerEvent) { got = append(got, *ev) }).Return()
	h.EXPECT().KeyboardFocus(s.ID(), mock.Anything).Run(func(_ neferclient.SurfaceID, f bool) { focus = append(focus, f) }).Return()
	dispatchUntil(t, c, h, func() bool { return len(got) == 1 && len(focus) == 1 })

	peer.capabilities(0)
	require.NoError(t, c.Roundtrip())
	dispatchUntil(t, c, h, func() bool { return len(got) == 2 && len(focus) == 2 })
	require.Equal(t, neferclient.PointerLeave, got[1].Kind, "a synthetic leave closes the focus")
	require.False(t, focus[1])
	require.NoError(t, c.Roundtrip())
	peer.mu.Lock()
	require.True(t, peer.pointerReleased)
	peer.mu.Unlock()
}

// TestSeatRepeatThroughEpoll: the repeat timerfd is watched by the connection's
// epoll loop and the repeat arrives through Dispatch.
func TestSeatRepeatThroughEpoll(t *testing.T) {
	c, srv, peer, s := seatSetup(t)
	_, kb := peer.ids()
	peer.keymap(kb, 1)
	srv.write(frame(kb, 5, appendU32(appendU32(nil, 100), 1))) // 100/s after 1 ms
	peer.keyboardEnter(kb, s.SurfaceObjectID())
	srv.write(peer.key(kb, 16, 1))
	require.NoError(t, c.Roundtrip())
	var repeats int
	var texts []string
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().KeyboardFocus(mock.Anything, mock.Anything).Return()
	h.EXPECT().Key(mock.Anything).Run(func(ev *neferclient.KeyEvent) {
		if ev.Repeat {
			repeats++
			texts = append(texts, string(ev.Text))
		}
	}).Return()
	dispatchUntil(t, c, h, func() bool { return repeats >= 3 })
	require.Equal(t, "a", texts[0])

	srv.write(peer.key(kb, 16, 0))
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(h))
	n := repeats
	time.Sleep(50 * time.Millisecond) // five repeat intervals at 100/s
	require.NoError(t, c.Dispatch(h))
	require.Equal(t, n, repeats, "release stops the repeat")
}

// TestAllocKey measures the steady-state key path over a real socketpair, on
// the real code path (no mocks): key frames in, reader decode, queue, Dispatch
// with xkb state and translation, and the Handler call.
//
// Every path is zero-allocation: releases, text and secret presses, and
// modifier masks (purego-xkbcommon v0.2.0 dispatches without allocating).
func TestAllocKey(t *testing.T) {
	skipUnderRace(t)
	c, srv, peer, s := seatSetup(t)
	_, kb := peer.ids()
	peer.keymap(kb, 1)
	srv.write(frame(kb, 5, appendU32(appendU32(nil, 0), 0))) // repeat disabled
	peer.keyboardEnter(kb, s.SurfaceObjectID())
	require.NoError(t, c.Roundtrip())
	h := &countingHandler{}
	require.NoError(t, c.Dispatch(h))
	require.Zero(t, h.errs)

	run := func(batch []byte, events int) func() {
		return func() {
			if _, err := srv.conn.Write(batch); err != nil {
				t.Fatal(err)
			}
			for c.QueueLen() < events {
				runtime.Gosched()
			}
			if err := c.Dispatch(h); err != nil {
				t.Fatal(err)
			}
		}
	}
	measure := func(name string, step func()) float64 {
		step()
		step()
		allocs := testing.AllocsPerRun(20, step)
		t.Logf("%s: allocs per Dispatch: %v", name, allocs)
		return allocs
	}

	const pairs = 50
	var releases []byte // releases of keys that were never pressed: queue + decode + dispatch only
	for range 2 * pairs {
		releases = append(releases, peer.key(kb, 16, 0)...)
	}
	require.Zero(t, measure("pipeline, 100 releases", run(releases, 2*pairs)), "the library itself allocates nothing per key")
	require.Zero(t, h.keys, "unheld releases are not delivered")

	var batch []byte
	for range pairs {
		batch = append(batch, peer.key(kb, 16, 1)...)
		batch = append(batch, peer.key(kb, 16, 0)...)
	}
	step := run(batch, 2*pairs)
	text := measure("text, 50 presses + releases", step)
	require.Zero(t, text, "text presses allocate nothing")
	require.Equal(t, "a", h.lastText)

	// Modifier masks: an identical mask is a no-op and allocates nothing; a
	// changing one updates the xkb state, also without allocating.
	mods := func(depressed uint32) []byte {
		b := appendU32(appendU32(nil, 1), depressed)
		return frame(kb, 4, appendU32(appendU32(appendU32(b, 0), 0), 0))
	}
	var same, changing []byte
	for range 100 {
		same = append(same, mods(0)...)
	}
	for i := range 100 {
		changing = append(changing, mods(uint32(i%2))...)
	}
	require.Zero(t, measure("modifiers, 100 identical masks", run(same, 100)), "identical masks cost nothing")
	changes := measure("modifiers, 100 alternating masks", run(changing, 100))
	require.Zero(t, changes, "mask changes allocate nothing")

	buf := neferclient.NewSecretBuffer(4096)
	c.Seat().SetSecret(buf)
	secret := measure("secret, 50 presses + releases", step)
	require.Zero(t, secret, "secret presses allocate nothing")
	require.Empty(t, h.lastText, "no text reaches the handler in secret mode")
	require.Positive(t, buf.Len())
	buf.Wipe()
	require.Zero(t, h.errs)
}

// TestAllocPointer measures the pointer path over a real socketpair: a batch
// of motion, button and axis frames in, reader decode, queue, Dispatch and the
// Handler call. Pointer events need no xkb, so the whole path allocates
// nothing.
func TestAllocPointer(t *testing.T) {
	skipUnderRace(t)
	c, srv, peer, s := seatSetup(t)
	pointer, _ := peer.ids()
	enter := appendU32(appendU32(nil, 5), s.SurfaceObjectID())
	srv.write(frame(pointer, 0, appendU32(appendU32(enter, fixed(1)), fixed(1))))
	require.NoError(t, c.Roundtrip())
	h := &countingHandler{}
	require.NoError(t, c.Dispatch(h))
	require.Equal(t, 1, h.pointers)

	const rounds = 40
	var batch []byte
	for i := range rounds {
		batch = append(batch, frame(pointer, 2, append(appendU32(appendU32(nil, 0), fixed(float64(i))), appendU32(nil, fixed(2))...))...)
		batch = append(batch, frame(pointer, 3, appendU32(appendU32(appendU32(appendU32(nil, 6), 0), 0x110), uint32(i%2)))...)
		batch = append(batch, frame(pointer, 4, appendU32(appendU32(appendU32(nil, 0), 0), fixed(2)))...)
		batch = append(batch, frame(pointer, 9, appendU32(appendU32(nil, 0), 120))...)
	}
	step := func() {
		if _, err := srv.conn.Write(batch); err != nil {
			t.Fatal(err)
		}
		for c.QueueLen() < 4*rounds {
			runtime.Gosched()
		}
		if err := c.Dispatch(h); err != nil {
			t.Fatal(err)
		}
	}
	step()
	step()
	before := h.pointers
	allocs := testing.AllocsPerRun(20, step)
	t.Logf("allocs per Dispatch of %d pointer events: %v", 4*rounds, allocs)
	require.Zero(t, allocs)
	require.Greater(t, h.pointers, before)
	require.Zero(t, h.errs)
}
