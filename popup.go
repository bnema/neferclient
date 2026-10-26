package neferclient

import (
	"errors"
	"fmt"
	"math"

	"bnema.dev/go-wayland-bindings/client/xdgshell"
)

// PopupEdge names an edge or corner, used both as the anchor on the parent's
// anchor rectangle and as the gravity of the popup. The values equal the
// xdg_positioner anchor and gravity enums.
type PopupEdge uint8

const (
	EdgeNone PopupEdge = iota
	EdgeTop
	EdgeBottom
	EdgeLeft
	EdgeRight
	EdgeTopLeft
	EdgeBottomLeft
	EdgeTopRight
	EdgeBottomRight
)

// PopupAdjust is a bit set telling the compositor how to move a popup that
// would not fit on the output. The bits equal xdg_positioner
// constraint_adjustment.
type PopupAdjust uint8

const (
	AdjustSlideX PopupAdjust = 1 << iota
	AdjustSlideY
	AdjustFlipX
	AdjustFlipY
	AdjustResizeX
	AdjustResizeY
)

const adjustAll = AdjustSlideX | AdjustSlideY | AdjustFlipX | AdjustFlipY | AdjustResizeX | AdjustResizeY

// PopupPlacement tells the compositor where a popup goes relative to its
// parent. Every length is in logical pixels.
type PopupPlacement struct {
	Anchor        Rect // on the parent surface; Width and Height must be > 0
	Edge, Gravity PopupEdge
	Adjust        PopupAdjust
	Offset        [2]int32
	Width, Height int32 // popup size; must be > 0
}

// PopupConfig requests an xdg_popup.
type PopupConfig struct {
	// Parent is a layer surface, a toplevel or another popup. It must be open.
	Parent *Surface
	Place  PopupPlacement
	// Grab takes an explicit grab: the popup gets the keyboard and is
	// dismissed by a click outside it. It needs a bound seat.
	Grab bool
	// Serial is the [PointerEvent.Serial] of the press that opened the popup.
	// It is required (non-zero) with Grab.
	Serial uint32
}

func validatePlacement(p *PopupPlacement) error {
	switch {
	case p.Anchor.Width <= 0 || p.Anchor.Height <= 0:
		return fmt.Errorf("neferclient: popup anchor rectangle %dx%d must be positive", p.Anchor.Width, p.Anchor.Height)
	case p.Width <= 0 || p.Height <= 0:
		return fmt.Errorf("neferclient: invalid popup size %dx%d", p.Width, p.Height)
	case p.Edge > EdgeBottomRight || p.Gravity > EdgeBottomRight:
		return fmt.Errorf("neferclient: invalid popup edge %d or gravity %d", p.Edge, p.Gravity)
	case p.Adjust&^adjustAll != 0:
		return fmt.Errorf("neferclient: invalid popup constraint adjustment %#x", uint8(p.Adjust))
	case int64(p.Anchor.X)+int64(p.Anchor.Width) > math.MaxInt32 || int64(p.Anchor.Y)+int64(p.Anchor.Height) > math.MaxInt32:
		return errors.New("neferclient: popup anchor rectangle overflows")
	}
	return nil
}

// newPositioner builds an xdg_positioner from p. The caller destroys it.
func (c *Conn) newPositioner(p *PopupPlacement) (*xdgshell.XdgPositioner, error) {
	pos, err := c.g.xdg.CreatePositioner()
	if err != nil {
		return nil, err
	}
	for _, e := range []error{
		pos.SetSize(p.Width, p.Height),
		pos.SetAnchorRect(p.Anchor.X, p.Anchor.Y, p.Anchor.Width, p.Anchor.Height),
		pos.SetAnchor(uint32(p.Edge)),
		pos.SetGravity(uint32(p.Gravity)),
		pos.SetConstraintAdjustment(uint32(p.Adjust)),
		pos.SetOffset(p.Offset[0], p.Offset[1]),
	} {
		if e != nil {
			return nil, errors.Join(e, pos.Destroy())
		}
	}
	return pos, nil
}

// NewPopup creates an xdg_popup on a layer surface, a toplevel or another
// popup. It becomes presentable after the first Handler.Configure, which
// reports the size the compositor gave it. Handle [Handler.PopupDone]: the
// compositor dismisses the popup on an outside click or when the grab is
// denied, and the popup must then be closed.
//
// The parent must be mapped: configured by the compositor and, for the
// compositor to place the popup, presented at least once. NewPopup returns an
// error for a parent that has not been configured yet; a compositor dismisses
// (PopupDone) a popup whose parent shows nothing.
//
// A grab needs a seat and the serial of the user action. Its parent must be a
// toplevel or layer surface, or the grabbing popup that is the topmost of the
// connection's grab chain; only one chain exists at a time. Other grabs return
// an error before any request is sent (xdg-shell would raise a protocol error).
//
// Close popups before their parent: Close of a surface with open child popups
// fails.
func (c *Conn) NewPopup(cfg PopupConfig) (*Surface, error) {
	if c.closed {
		return nil, ErrClosed
	}
	parent := cfg.Parent
	switch {
	case parent == nil || parent.c != c:
		return nil, errors.New("neferclient: popup needs a parent surface of this connection")
	case parent.closed:
		return nil, fmt.Errorf("neferclient: popup parent: %w", ErrClosed)
	case parent.role != roleLayer && parent.role != roleToplevel && parent.role != rolePopup:
		return nil, errors.New("neferclient: popup parent must be a layer surface, a toplevel or a popup")
	case !parent.configured:
		return nil, errors.New("neferclient: popup parent is not configured yet: wait for its first Handler.Configure")
	}
	if err := validatePlacement(&cfg.Place); err != nil {
		return nil, err
	}
	if cfg.Grab {
		if cfg.Serial == 0 {
			return nil, errors.New("neferclient: popup grab needs the serial of the triggering input event")
		}
		c.ensureSeat()
		if c.seat == nil || c.seat.proxy == nil {
			cause := c.seatErr
			if cause == nil {
				cause = errors.New("popup grab needs a seat")
			}
			return nil, &CapabilityError{Name: "wl_seat", Cause: cause}
		}
		if parent.role == rolePopup && !parent.grabbed {
			return nil, errors.New("neferclient: popup grab needs a grabbing parent popup")
		}
		if c.grabTop != nil && parent != c.grabTop {
			return nil, errors.New("neferclient: popup grab needs the topmost grabbing popup as parent while a grab is open")
		}
	}
	if err := c.ensureXdg(); err != nil {
		return nil, err
	}
	c.pause()
	defer c.resume()
	s, err := c.newSurface(rolePopup, cfg.Place.Width, cfg.Place.Height)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.destroy()
		}
	}()
	if s.xdg, err = c.g.xdg.GetXdgSurface(s.surf); err != nil {
		return nil, err
	}
	pos, err := c.newPositioner(&cfg.Place)
	if err != nil {
		return nil, err
	}
	var parentXdg *xdgshell.XdgSurface
	if parent.role != roleLayer {
		parentXdg = parent.xdg
	}
	s.popup, err = s.xdg.GetPopup(parentXdg, pos)
	// The compositor copies the state at get_popup.
	if err = errors.Join(err, pos.Destroy()); err != nil {
		return nil, err
	}
	if parent.role == roleLayer {
		if err = parent.layer.GetPopup(s.popup); err != nil {
			return nil, err
		}
	}
	q, sid := c.q, uint32(s.id)
	s.popup.OnConfigure(func(_, _, w, h int32) {
		e := event{kind: evPopupConfigure, id: sid, a: w, b: h}
		if w < 0 || h < 0 || w > maxLayerDimension || h > maxLayerDimension {
			e = event{kind: evBadEvent, id: sid, a: badConfigure}
		}
		q.post(&e)
	})
	s.popup.OnPopupDone(func() {
		e := event{kind: evPopupDone, id: sid}
		q.post(&e)
	})
	s.xdg.OnConfigure(func(serial uint32) {
		e := event{kind: evXdgConfigure, id: sid, serial: serial}
		q.post(&e)
	})
	if cfg.Grab {
		if err = s.popup.Grab(c.seat.proxy, cfg.Serial); err != nil {
			return nil, err
		}
		s.grabbed, c.grabTop = true, s
	}
	if err = s.finishSurface(); err != nil {
		return nil, err
	}
	if err = s.surf.Commit(); err != nil {
		return nil, err
	}
	s.parent = parent
	parent.children++
	ok = true
	return s, nil
}

// Reposition asks the compositor to place the popup again. The following
// Handler.Configure reports the resulting size. It needs xdg_wm_base version 3.
func (s *Surface) Reposition(p PopupPlacement) error {
	switch {
	case s.closed || s.c.closed:
		return ErrClosed
	case s.role != rolePopup:
		return errors.New("neferclient: Reposition needs a popup")
	case s.c.g.xdgVersion < 3:
		return &CapabilityError{Name: xdgshell.XdgWmBaseInterface, Cause: fmt.Errorf("reposition requires version 3, negotiated %d", s.c.g.xdgVersion)}
	}
	if err := validatePlacement(&p); err != nil {
		return err
	}
	pos, err := s.c.newPositioner(&p)
	if err != nil {
		return err
	}
	s.repoToken++
	return errors.Join(s.popup.Reposition(pos, s.repoToken), pos.Destroy())
}

// dropGrab ends the surface's grab: the topmost grabbing popup becomes its
// parent, if that one still grabs.
func (s *Surface) dropGrab() {
	if !s.grabbed {
		return
	}
	s.grabbed = false
	if c := s.c; c.grabTop == s {
		c.grabTop = nil
		if p := s.parent; p != nil && p.grabbed {
			c.grabTop = p
		}
	}
}
