package neferclient

import (
	"errors"
	"fmt"

	"github.com/bnema/go-wayland-bindings/client/cursorshape"
	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/wlturbo/wl"
	"golang.org/x/sys/unix"
)

// maxSeatVersion is the highest wl_seat version bound. Version 5 adds the
// wl_seat.release request and wl_pointer.frame, 8 adds wl_pointer.axis_value120.
const maxSeatVersion = 9

// CursorShape is a wp_cursor_shape_v1 shape. The values are the protocol's.
type CursorShape uint32

const (
	CursorDefault CursorShape = iota + 1
	CursorContextMenu
	CursorHelp
	CursorPointer
	CursorProgress
	CursorWait
	CursorCell
	CursorCrosshair
	CursorText
	CursorVerticalText
	CursorAlias
	CursorCopy
	CursorMove
	CursorNoDrop
	CursorNotAllowed
	CursorGrab
	CursorGrabbing
	CursorEResize
	CursorNResize
	CursorNEResize
	CursorNWResize
	CursorSResize
	CursorSEResize
	CursorSWResize
	CursorWResize
	CursorEWResize
	CursorNSResize
	CursorNESWResize
	CursorNWSEResize
	CursorColResize
	CursorRowResize
	CursorAllScroll
	CursorZoomIn
	CursorZoomOut
	CursorDndAsk    // needs cursor-shape version 2
	CursorAllResize // needs cursor-shape version 2
)

// PointerKind tells which pointer event a [PointerEvent] is.
type PointerKind uint8

const (
	PointerEnter PointerKind = iota + 1
	PointerLeave
	PointerMotion
	PointerButton
	// PointerAxis is a scroll step on Axis. DX or DY carry the continuous
	// value in logical pixels; Value120 carries the wheel value in 1/120 of a
	// notch. A compositor can send both for one physical step: use one.
	PointerAxis
	// PointerAxisStop ends a scroll sequence on Axis.
	PointerAxisStop
)

// Scroll axes of [PointerEvent.Axis].
const (
	AxisVertical   = 0
	AxisHorizontal = 1
)

// PointerEvent is one pointer event over a surface of the connection. X and Y
// are surface-local logical pixels (already logical: never divide by scale)
// and hold the latest position for every kind.
type PointerEvent struct {
	Surface  SurfaceID
	Kind     PointerKind
	X, Y     float64
	DX, DY   float64 // PointerAxis: continuous scroll value
	Button   uint32  // PointerButton: evdev button code
	Pressed  bool    // PointerButton
	Axis     uint32  // PointerAxis, PointerAxisStop: AxisVertical or AxisHorizontal
	Value120 int32   // PointerAxis: wheel value in 1/120 notches
}

// KeyEvent is one key press, repeat or release.
type KeyEvent struct {
	Surface   SurfaceID
	Keycode   uint32 // evdev key code; 0 when Secret is set
	Keysym    uint32 // xkb keysym in the current layout and modifier state; 0 when Secret is set
	Modifiers Modifiers
	Group     uint32 // effective layout index
	Pressed   bool
	Repeat    bool
	// Secret is set while a [SecretBuffer] is installed, for the press,
	// repeats and release of a key that produced text (or is a dead or compose
	// key): the text went into the buffer, and Keycode and Keysym are 0 so the
	// event does not reveal which character was typed. Other keys (Enter,
	// Escape, BackSpace, modifiers, Control combinations) keep their keysym.
	Secret bool
	// Text is the UTF-8 text of the key, nil for releases and keys without
	// text. It aliases library storage that is wiped when Handler.Key
	// returns. It is always nil while a [SecretBuffer] is installed.
	Text []byte
}

// Seat is the connection's first wl_seat: pointer, keyboard and cursor. It
// belongs to the owner goroutine. Without a seat global every method is a
// no-op. The seat is bound once, at first use: a seat announced later is not
// picked up, and a removed one is not replaced (no seat hotplug).
type Seat struct {
	c     *Conn
	proxy *wayland.Seat
	name  uint32 // registry name of the global
	ver   uint32

	pointer *wayland.Pointer
	ptrGen  uint32 // generation of pointer; bumped on every create and drop
	ptrSurf SurfaceID
	px, py  float64
	pev     PointerEvent

	keyboard *wayland.Keyboard
	kbGen    uint32
	kbSurf   SurfaceID
	kb       *keyboard
	kev      KeyEvent

	secret *SecretBuffer

	shapes      *cursorshape.WpCursorShapeManager
	shapesVer   uint32
	shapeDev    *cursorshape.WpCursorShapeDevice
	enterSerial uint32 // latest pointer enter serial, 0 while outside
	shape       CursorShape
}

// Seat returns the connection's seat (nil after Close), binding wl_seat on first use. The seat
// is also bound when the first surface is created. Input starts flowing to the
// Handler once the compositor announces the capabilities.
func (c *Conn) Seat() *Seat {
	c.ensureSeat()
	return c.seat
}

// ensureSeat binds wl_seat and the optional cursor-shape manager once. A
// failure is reported by the next Dispatch through Handler.Error.
func (c *Conn) ensureSeat() {
	if c.seat != nil || c.closed {
		return
	}
	s := &Seat{c: c}
	c.seat = s
	g, ok := c.display.Registry().FindGlobal(wayland.SeatInterface)
	if !ok {
		return
	}
	q := c.q
	proxy, v, err := c.Bind(wayland.SeatInterface, maxSeatVersion, func(ctx *wl.Context) *wayland.Seat {
		p := wayland.NewSeat(ctx)
		p.OnCapabilities(func(caps uint32) {
			e := event{kind: evSeatCaps, a: int32(caps)}
			q.post(&e)
		})
		return p
	})
	if err != nil {
		c.seatErr = fmt.Errorf("neferclient: bind wl_seat: %w", err)
		return
	}
	s.proxy, s.name, s.ver = proxy, g.Name, v
	m, mv, err := c.Bind(cursorshape.WpCursorShapeManagerInterface, 2, cursorshape.NewWpCursorShapeManager)
	if err != nil {
		if !errors.Is(err, ErrGlobalNotFound) {
			c.seatErr = fmt.Errorf("neferclient: bind cursor shape manager: %w", err)
		}
		return
	}
	s.shapes, s.shapesVer = m, mv
}

// SetSecret installs b to receive typed text, or removes it with nil. While
// installed, printable text goes only into b and never into KeyEvent.Text;
// BackSpace removes one code point; every other key is still delivered as a
// KeyEvent without text. Keys that produced text are delivered with
// KeyEvent.Secret set and Keycode and Keysym 0, for the press, repeats and
// release; Enter, Escape, BackSpace and Control combinations keep their
// keysym. Installing or removing cancels held keys and repeat. The previous
// buffer is not wiped: that is the caller's call.
//
// Limits: runtime/secret (GOEXPERIMENT=runtimesecret) erases Go registers,
// stack and heap temporaries, but not the native memory of libxkbcommon, which
// processes the key text.
func (s *Seat) SetSecret(b *SecretBuffer) {
	if s == nil {
		return
	}
	s.secret = b
	if s.kb != nil {
		s.kb.setSecret(b)
	}
}

// SetCursor asks the compositor to show shape for the pointer. It is a no-op
// when the compositor has no cursor-shape support, the pointer is outside the
// connection's surfaces, or shape is unchanged since the last enter.
func (s *Seat) SetCursor(shape CursorShape) error {
	if s == nil || s.c.closed {
		return ErrClosed
	}
	if shape < CursorDefault || shape > CursorAllResize {
		return fmt.Errorf("neferclient: invalid cursor shape %d", shape)
	}
	if shape >= CursorDndAsk && s.shapes != nil && s.shapesVer < 2 {
		return &CapabilityError{Name: cursorshape.WpCursorShapeManagerInterface, Cause: fmt.Errorf("shape %d requires version 2", shape)}
	}
	if s.shapes == nil || s.pointer == nil || s.ptrSurf == 0 || shape == s.shape {
		return nil
	}
	if s.shapeDev == nil {
		dev, err := s.shapes.GetPointer(s.pointer)
		if err != nil {
			return fmt.Errorf("neferclient: cursor shape device: %w", err)
		}
		s.shapeDev = dev
	}
	if err := s.shapeDev.SetShape(s.enterSerial, uint32(shape)); err != nil {
		return fmt.Errorf("neferclient: set cursor shape: %w", err)
	}
	s.shape = shape
	return nil
}

// surfaceByObject finds the surface whose wl_surface has object id obj.
func (c *Conn) surfaceByObject(obj uint32) *Surface {
	for _, s := range c.surfaces {
		if !s.closed && s.surf != nil && s.surf.ID() == obj {
			return s
		}
	}
	return nil
}

func (c *Conn) seatTimer(fd int32) bool {
	return c.seat != nil && c.seat.kb != nil && int(fd) == c.seat.kb.tfd
}

// seatRepeat handles the repeat timer: at most one repeat per expiration.
func (c *Conn) seatRepeat(h Handler) {
	s := c.seat
	if s.kb.expired() {
		ke := &s.kev
		*ke = KeyEvent{Surface: s.kbSurf}
		ok, err := s.kb.repeat(ke)
		switch {
		case err != nil:
			h.Error(err)
		case ok:
			s.deliverKey(ke, h)
		}
	}
	if err := s.kb.takeTimerErr(); err != nil {
		h.Error(err)
	}
	if !c.closed {
		if err := c.armFD(s.kb.tfd, unix.EPOLL_CTL_MOD); err != nil {
			h.Error(err)
		}
	}
}

func (s *Seat) deliverKey(ke *KeyEvent, h Handler) {
	h.Key(ke)
	clear(s.kb.scratch[:]) // ordinary text must not outlive the call
	*ke = KeyEvent{}
	if s.kb.changed {
		h.SecretChanged(s.secret.Len())
	}
}

// seatSurfaceGone forgets focus on a closing surface.
func (c *Conn) seatSurfaceGone(sf *Surface) {
	s := c.seat
	if s == nil {
		return
	}
	if s.ptrSurf == sf.id {
		s.ptrSurf = 0
		s.enterSerial, s.shape = 0, 0
	}
	if s.kbSurf == sf.id {
		s.kbSurf = 0
		if s.kb != nil {
			s.kb.setFocus(false)
		}
	}
}

func (c *Conn) seatGlobalRemoved(global uint32, h Handler) {
	if s := c.seat; s != nil && s.proxy != nil && s.name == global {
		s.release(h)
	}
}

// release drops the devices and the seat proxy.
func (s *Seat) release(h Handler) {
	s.dropPointer(h)
	s.dropKeyboard(h)
	if s.ver >= 5 {
		_ = s.proxy.Release()
	} else {
		s.c.wlctx.Unregister(s.proxy)
	}
	s.proxy = nil
}

func releaseDevice(ver uint32, c *wl.Context, p interface {
	wl.Proxy
	Release() error
}) {
	if ver >= 3 {
		_ = p.Release()
	} else {
		c.Unregister(p)
	}
}

func (s *Seat) dropPointer(h Handler) {
	if s.pointer == nil {
		return
	}
	if s.shapeDev != nil {
		_ = s.shapeDev.Destroy()
		s.shapeDev = nil
	}
	s.enterSerial, s.shape = 0, 0
	releaseDevice(s.ver, s.c.wlctx, s.pointer)
	s.pointer = nil
	s.ptrGen++
	if id := s.ptrSurf; id != 0 {
		s.ptrSurf = 0
		pe := &s.pev
		*pe = PointerEvent{Surface: id, Kind: PointerLeave, X: s.px, Y: s.py}
		h.Pointer(pe)
	}
}

func (s *Seat) dropKeyboard(h Handler) {
	if s.keyboard == nil {
		return
	}
	releaseDevice(s.ver, s.c.wlctx, s.keyboard)
	s.keyboard = nil
	s.kbGen++
	if id := s.kbSurf; id != 0 {
		s.kbSurf = 0
		s.kb.setFocus(false)
		h.KeyboardFocus(id, false)
	} else if s.kb != nil {
		s.kb.setFocus(false)
	}
}

func (s *Seat) newPointer() error {
	c := s.c
	c.pause() // handlers go on after the request, before any event is read
	defer c.resume()
	p, err := s.proxy.GetPointer()
	if err != nil {
		return fmt.Errorf("neferclient: get wl_pointer: %w", err)
	}
	s.pointer = p
	s.ptrGen++
	gen, q := s.ptrGen, c.q
	post := func(e *event) {
		e.version = gen
		q.post(e)
	}
	p.OnEnter(func(serial, surf uint32, x, y wl.Fixed) {
		post(&event{kind: evPtrEnter, global: surf, serial: serial, a: int32(x), b: int32(y)})
	})
	p.OnLeave(func(_, surf uint32) { post(&event{kind: evPtrLeave, global: surf}) })
	p.OnMotion(func(_ uint32, x, y wl.Fixed) { post(&event{kind: evPtrMotion, a: int32(x), b: int32(y)}) })
	p.OnButton(func(serial, _, button, state uint32) {
		post(&event{kind: evPtrButton, serial: serial, flags: button, a: int32(state)})
	})
	p.OnAxis(func(_, axis uint32, v wl.Fixed) { post(&event{kind: evPtrAxis, flags: axis, a: int32(v)}) })
	p.OnAxisValue120(func(axis uint32, v int32) { post(&event{kind: evPtrAxisValue, flags: axis, a: v}) })
	p.OnAxisStop(func(_, axis uint32) { post(&event{kind: evPtrAxisStop, flags: axis}) })
	return nil
}

func (s *Seat) newKeyboard() error {
	c := s.c
	if s.kb == nil {
		kb, err := newKeyboard(composeLocale())
		if err != nil {
			return fmt.Errorf("neferclient: keyboard: %w", err)
		}
		if err = c.armFD(kb.tfd, unix.EPOLL_CTL_ADD); err != nil {
			kb.close()
			return err // armFD wraps it
		}
		kb.secret = s.secret
		s.kb = kb
	}
	c.pause()
	defer c.resume()
	k, err := s.proxy.GetKeyboard()
	if err != nil {
		return fmt.Errorf("neferclient: get wl_keyboard: %w", err)
	}
	s.keyboard = k
	s.kbGen++
	gen, q := s.kbGen, c.q
	post := func(e *event) {
		e.version = gen
		q.post(e)
	}
	k.OnKeymap(func(format uint32, fd *wl.OwnedFD, size uint32) {
		n, err := fd.Take()
		if err != nil {
			post(&event{kind: evSeatBad, a: badKeymapFD})
			return
		}
		post(&event{kind: evKbKeymap, fd: int32(n), flags: size, a: int32(format)})
	})
	k.OnEnter(func(serial, surf uint32, _ []byte) { post(&event{kind: evKbEnter, global: surf, serial: serial}) })
	k.OnLeave(func(_, surf uint32) { post(&event{kind: evKbLeave, global: surf}) })
	k.OnKey(func(serial, _, key, state uint32) {
		post(&event{kind: evKbKey, serial: serial, flags: key, a: int32(state)})
	})
	k.OnModifiers(func(_, depressed, latched, locked, group uint32) {
		post(&event{kind: evKbModifiers, flags: depressed, dev: uint64(latched) | uint64(locked)<<32, a: int32(group)})
	})
	k.OnRepeatInfo(func(rate, delay int32) { post(&event{kind: evKbRepeat, a: rate, b: delay}) })
	return nil
}

// Codes of evSeatBad.
const badKeymapFD int32 = 100

// applySeat handles the seat events on the owner.
func (c *Conn) applySeat(ev *event, h Handler) {
	defer ev.closeFD()
	s := c.seat
	if s == nil {
		return
	}
	switch ev.kind {
	case evSeatBad:
		h.Error(errors.New("neferclient: keymap descriptor unavailable"))
	case evSeatCaps:
		s.applyCaps(uint32(ev.a), h)
	case evPtrEnter, evPtrLeave, evPtrMotion, evPtrButton, evPtrAxis, evPtrAxisValue, evPtrAxisStop:
		if s.pointer != nil && ev.version == s.ptrGen {
			s.applyPointer(ev, h)
		}
	default:
		if s.keyboard != nil && ev.version == s.kbGen {
			s.applyKeyboard(ev, h)
			if err := s.kb.takeTimerErr(); err != nil {
				h.Error(err)
			}
		}
	}
}

func (s *Seat) applyCaps(caps uint32, h Handler) {
	if s.proxy == nil {
		return
	}
	if caps&1 == 0 {
		s.dropPointer(h)
	}
	if caps&2 == 0 {
		s.dropKeyboard(h)
	}
	if caps&1 != 0 && s.pointer == nil {
		if err := s.newPointer(); err != nil {
			h.Error(err)
		}
	}
	if caps&2 != 0 && s.keyboard == nil {
		if err := s.newKeyboard(); err != nil {
			h.Error(err)
		}
	}
}

func (s *Seat) applyPointer(ev *event, h Handler) {
	pe := &s.pev
	*pe = PointerEvent{}
	fixed := func(v int32) float64 { return wl.Fixed(v).Float64() }
	switch ev.kind {
	case evPtrEnter:
		sf := s.c.surfaceByObject(ev.global)
		if sf == nil {
			return
		}
		s.ptrSurf = sf.id
		s.px, s.py = fixed(ev.a), fixed(ev.b)
		s.enterSerial, s.shape = ev.serial, 0 // the compositor forgets the cursor on leave
		pe.Kind = PointerEnter
	case evPtrLeave:
		if sf := s.c.surfaceByObject(ev.global); s.ptrSurf == 0 || sf == nil || sf.id != s.ptrSurf {
			return
		}
		pe.Kind = PointerLeave
		pe.Surface = s.ptrSurf
		pe.X, pe.Y = s.px, s.py
		s.ptrSurf = 0
		s.enterSerial, s.shape = 0, 0
		h.Pointer(pe)
		return
	default:
		if s.ptrSurf == 0 {
			return
		}
		switch ev.kind {
		case evPtrMotion:
			s.px, s.py = fixed(ev.a), fixed(ev.b)
			pe.Kind = PointerMotion
		case evPtrButton:
			pe.Kind, pe.Button, pe.Pressed = PointerButton, ev.flags, ev.a == 1
		case evPtrAxis:
			pe.Kind, pe.Axis = PointerAxis, ev.flags
			if ev.flags == AxisVertical {
				pe.DY = fixed(ev.a)
			} else {
				pe.DX = fixed(ev.a)
			}
		case evPtrAxisValue:
			pe.Kind, pe.Axis, pe.Value120 = PointerAxis, ev.flags, ev.a
		case evPtrAxisStop:
			pe.Kind, pe.Axis = PointerAxisStop, ev.flags
		}
	}
	pe.Surface, pe.X, pe.Y = s.ptrSurf, s.px, s.py
	h.Pointer(pe)
}

func (s *Seat) applyKeyboard(ev *event, h Handler) {
	kb := s.kb
	switch ev.kind {
	case evKbKeymap:
		fd := int(ev.fd)
		ev.fd = -1     // replaceFD consumes fd; the deferred closeFD must not touch the number again
		if ev.a == 0 { // no_keymap: the compositor sends no usable map; nothing to report
			_ = unix.Close(fd)
			return
		}
		if ev.a != 1 || ev.flags == 0 || ev.flags > keymapMax {
			_ = unix.Close(fd)
			h.Error(fmt.Errorf("neferclient: unsupported keymap format %d or size %d", ev.a, ev.flags))
			return
		}
		if err := kb.replaceFD(fd, int(ev.flags)); err != nil {
			h.Error(fmt.Errorf("neferclient: keymap: %w", err))
		}
	case evKbEnter:
		sf := s.c.surfaceByObject(ev.global)
		if sf == nil {
			return
		}
		s.kbSurf = sf.id
		kb.setFocus(true)
		h.KeyboardFocus(sf.id, true)
	case evKbLeave:
		sf := s.c.surfaceByObject(ev.global)
		if sf == nil || s.kbSurf != sf.id {
			return
		}
		s.kbSurf = 0
		kb.setFocus(false)
		h.KeyboardFocus(sf.id, false)
	case evKbKey:
		ke := &s.kev
		*ke = KeyEvent{Surface: s.kbSurf}
		if ev.a == 1 {
			ok, err := kb.press(ev.flags, ke)
			switch {
			case err != nil:
				h.Error(err)
			case ok:
				s.deliverKey(ke, h)
			}
		} else if kb.release(ev.flags, ke) {
			s.deliverKey(ke, h)
		}
	case evKbModifiers:
		if err := kb.setMask(ev.flags, uint32(ev.dev), uint32(ev.dev>>32), uint32(ev.a)); err != nil {
			h.Error(err)
		}
	case evKbRepeat:
		kb.setRepeat(ev.a, ev.b)
	}
}

func (c *Conn) closeSeat() {
	if s := c.seat; s != nil && s.kb != nil {
		s.kb.close()
		s.kb = nil
	}
}
