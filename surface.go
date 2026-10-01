package neferclient

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/bnema/go-wayland-bindings/client/extsessionlock"
	"github.com/bnema/go-wayland-bindings/client/fractionalscale"
	"github.com/bnema/go-wayland-bindings/client/linuxdmabuf"
	"github.com/bnema/go-wayland-bindings/client/linuxdrmsyncobj"
	"github.com/bnema/go-wayland-bindings/client/viewporter"
	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/go-wayland-bindings/client/wlrlayershell"
	"github.com/bnema/go-wayland-bindings/client/xdgshell"
	"github.com/bnema/wlturbo/wl"
	"golang.org/x/sys/unix"
)

// LayerLevel is the wlr-layer-shell stacking layer. The zero value is invalid.
type LayerLevel uint8

const (
	LayerBackground LayerLevel = iota + 1
	LayerBottom
	LayerTop
	LayerOverlay
)

// Anchor is a bit set of output edges a layer surface attaches to.
type Anchor uint8

const (
	AnchorTop Anchor = 1 << iota
	AnchorBottom
	AnchorLeft
	AnchorRight
)

const anchorAll = AnchorTop | AnchorBottom | AnchorLeft | AnchorRight

// KeyboardMode is the layer surface keyboard interactivity. The zero value is
// KeyboardNone. KeyboardOnDemand needs zwlr_layer_shell_v1 version 4.
type KeyboardMode uint8

const (
	KeyboardNone KeyboardMode = iota
	KeyboardExclusive
	KeyboardOnDemand
)

// maxLayerDimension bounds a compositor-assigned layer surface extent.
const maxLayerDimension = 16384

// DefaultLayerNamespace is used when [LayerConfig.Namespace] is empty.
const DefaultLayerNamespace = "neferclient"

// LayerConfig requests a wlr-layer-shell surface. Width and Height give the
// initial logical size; an axis anchored to both opposite edges is sized by
// the compositor and reported through Handler.Configure.
type LayerConfig struct {
	Output        string // wl_output name; empty lets the compositor choose
	Namespace     string // defaults to [DefaultLayerNamespace]
	Level         LayerLevel
	Anchors       Anchor
	Keyboard      KeyboardMode
	ExclusiveZone int32    // >0 reserves, 0 neutral, -1 ignores other zones
	Margin        [4]int32 // top, right, bottom, left
	// InputRects, when non-nil, restricts pointer input to these rectangles
	// (logical pixels). An empty non-nil slice makes the surface click-through.
	InputRects []Rect
	// Width and Height are the requested logical size. They are required on
	// every axis that is not anchored to both opposite edges.
	Width, Height int32
}

type surfaceRole uint8

const (
	roleToplevel surfaceRole = iota + 1
	roleLayer
	roleLock
)

// Surface is one wl_surface with its role and the DMA-BUF presentation
// objects. Every method belongs to the owner goroutine.
type Surface struct {
	c    *Conn
	id   SurfaceID
	role surfaceRole

	surf     *wayland.Surface
	viewport *viewporter.WpViewport
	frac     *fractionalscale.WpFractionalScale
	sync     *linuxdrmsyncobj.WpLinuxDrmSyncobjSurface
	feedback *linuxdmabuf.LinuxDmabufFeedback
	xdg      *xdgshell.XdgSurface
	toplevel *xdgshell.XdgToplevel
	layer    *wlrlayershell.LayerSurface
	lockSurf *extsessionlock.ExtSessionLockSurface
	lock     *Lock

	width, height int32
	scale         float64
	pendW, pendH  int32 // toplevel size announced before the xdg configure
	configured    bool
	frameReady    bool
	closed        bool

	// destination/opaque state last sent; they are double-buffered surface
	// state that persists across commits, so unchanged values are not resent.
	sentDestW, sentDestH int32
	sentBufScale         int32  // starts at 1, the protocol default
	presented            bool   // a Present committed: the surface is mapped
	output               uint32 // lock surfaces: the covered wl_output global
	opaqueSet            bool
	opaqueW, opaqueH     int32

	cb  *frameCallback
	req surfaceRequests

	inputRects  []Rect
	inputCustom bool

	buffers   map[uint64]importedBuffer
	timelines map[uint64]*linuxdrmsyncobj.WpLinuxDrmSyncobjTimeline

	fb      Feedback
	table   []Format // current format table
	pending Feedback // round in progress; Formats reused across rounds
	fbDone  bool
}

// frameCallback is a reusable wl_callback: the library keeps a single frame
// callback pending per surface, so one proxy object serves all of them.
type frameCallback struct {
	wl.BaseProxy
	q  *queue
	id SurfaceID
}

func (f *frameCallback) EventSignature(op uint16) (string, bool) {
	if op == 0 {
		return "uint,", true
	}
	return "", false
}

func (f *frameCallback) Dispatch(ev *wl.Event) {
	if ev.Opcode != 0 {
		return
	}
	_ = ev.Uint32()
	f.Context().Unregister(f)
	e := event{kind: evFrame, id: uint32(f.id)}
	f.q.post(&e)
}

// ID returns the surface's identifier used in Handler callbacks.
func (s *Surface) ID() SurfaceID { return s.id }

// Size returns the current logical size and scale (1 until announced). Before
// the first configure the size is the requested one (zero for lock surfaces).
func (s *Surface) Size() (w, h int32, scale float64) { return s.width, s.height, s.scale }

// Feedback returns the latest complete dmabuf feedback, or nil before the
// first Handler.FeedbackDone. The value and its slice stay valid until the
// next FeedbackDone.
func (s *Surface) Feedback() *Feedback {
	if !s.fbDone {
		return nil
	}
	return &s.fb
}

// PhysicalSize returns the buffer size in pixels for the current logical size
// and scale.
func (s *Surface) PhysicalSize() (int32, int32, error) {
	if s.scale <= 0 || s.width <= 0 || s.height <= 0 {
		return 0, 0, errors.New("neferclient: invalid surface geometry")
	}
	if s.scale != math.Trunc(s.scale) && s.viewport == nil {
		return 0, 0, &CapabilityError{Name: viewporter.WpViewporterInterface, Cause: fmt.Errorf("fractional scale %g requires viewporter", s.scale)}
	}
	// Ignore sub-nanopixel floating noise at exact integer boundaries.
	x, y := math.Ceil(float64(s.width)*s.scale-1e-9), math.Ceil(float64(s.height)*s.scale-1e-9)
	if x > math.MaxInt32 || y > math.MaxInt32 {
		return 0, 0, errors.New("neferclient: buffer dimensions overflow")
	}
	return int32(x), int32(y), nil
}

func validateLayer(o *LayerConfig) error {
	switch {
	case o.Level < LayerBackground || o.Level > LayerOverlay:
		return fmt.Errorf("neferclient: invalid layer level %d", o.Level)
	case o.Anchors&^anchorAll != 0:
		return fmt.Errorf("neferclient: invalid layer anchors %#x", uint8(o.Anchors))
	case o.Keyboard > KeyboardOnDemand:
		return fmt.Errorf("neferclient: invalid keyboard mode %d", o.Keyboard)
	case o.ExclusiveZone < -1:
		return fmt.Errorf("neferclient: invalid exclusive zone %d", o.ExclusiveZone)
	case strings.ContainsRune(o.Namespace, 0), strings.ContainsRune(o.Output, 0):
		return errors.New("neferclient: layer namespace or output contains NUL")
	case o.Width < 0 || o.Height < 0:
		return errors.New("neferclient: negative layer size")
	}
	if o.Anchors&(AnchorLeft|AnchorRight) != AnchorLeft|AnchorRight && o.Width == 0 ||
		o.Anchors&(AnchorTop|AnchorBottom) != AnchorTop|AnchorBottom && o.Height == 0 {
		return errors.New("neferclient: layer size required on axes not anchored to both edges")
	}
	return validateRects(o.InputRects)
}

func validateRects(rects []Rect) error {
	for i, r := range rects {
		if r.Width <= 0 || r.Height <= 0 {
			return fmt.Errorf("neferclient: input rect %d has non-positive size %dx%d", i, r.Width, r.Height)
		}
		if int64(r.X)+int64(r.Width) > math.MaxInt32 || int64(r.Y)+int64(r.Height) > math.MaxInt32 {
			return fmt.Errorf("neferclient: input rect %d overflows", i)
		}
	}
	return nil
}

// newSurface creates the wl_surface and the add-on objects shared by every
// role. The reader must be paused: handlers are installed after the requests.
func (c *Conn) newSurface(role surfaceRole, w, h int32) (*Surface, error) {
	if c.closed {
		return nil, ErrClosed
	}
	if err := c.ensureCore(); err != nil {
		return nil, err
	}
	c.ensureSeat() // optional: input flows to the Handler once the seat is bound
	g := &c.g
	c.nextSurf++
	s := &Surface{c: c, id: c.nextSurf, role: role, width: w, height: h, scale: 1, sentBufScale: 1,
		buffers: map[uint64]importedBuffer{}, timelines: map[uint64]*linuxdrmsyncobj.WpLinuxDrmSyncobjTimeline{}}
	ok := false
	defer func() {
		if !ok {
			s.destroy()
		}
	}()
	var err error
	if s.surf, err = g.compositor.CreateSurface(); err != nil {
		return nil, err
	}
	q, sid := c.q, uint32(s.id)
	s.surf.OnPreferredBufferScale(func(f int32) {
		e := event{kind: evBufferScale, id: sid, a: f}
		q.post(&e)
	})
	if g.viewporter != nil {
		if s.viewport, err = g.viewporter.GetViewport(s.surf); err != nil {
			return nil, err
		}
	}
	if g.fscale != nil {
		if s.frac, err = g.fscale.GetFractionalScale(s.surf); err != nil {
			return nil, err
		}
		s.frac.OnPreferredScale(func(n uint32) {
			e := event{kind: evPreferredScale, id: sid, a: int32(n)}
			q.post(&e)
		})
	}
	// The role objects come next (callers); sync surface and feedback after.
	s.cb = &frameCallback{q: q, id: s.id}
	s.cb.SetContext(c.wlctx)
	s.req = wireRequests{s: s}
	c.surfaces[s.id] = s
	ok = true
	return s, nil
}

// finishSurface creates the explicit-sync and feedback objects once the role
// exists.
func (s *Surface) finishSurface() error {
	c := s.c
	var err error
	if s.sync, err = c.g.syncobj.GetSurface(s.surf); err != nil {
		return err
	}
	if s.feedback, err = c.g.dmabuf.GetDefaultFeedback(); err != nil {
		return err
	}
	s.watchFeedback()
	return nil
}

func (s *Surface) watchFeedback() {
	q, sid := s.c.q, uint32(s.id)
	s.feedback.OnFormatTable(func(fd *wl.OwnedFD, size uint32) {
		n, err := fd.Take()
		if err != nil {
			e := event{kind: evBadEvent, id: sid, a: badFeedbackFD}
			q.post(&e)
			return
		}
		e := event{kind: evFeedbackTable, id: sid, fd: int32(n), flags: size}
		q.post(&e)
	})
	s.feedback.OnMainDevice(func(dev []byte) {
		if len(dev) != 8 {
			e := event{kind: evBadEvent, id: sid, a: badMainDevice}
			q.post(&e)
			return
		}
		e := event{kind: evFeedbackMainDevice, id: sid, dev: binary.NativeEndian.Uint64(dev)}
		q.post(&e)
	})
	s.feedback.OnTrancheFormats(func(data []byte) {
		if len(data)%2 != 0 {
			e := event{kind: evBadEvent, id: sid, a: badTranche}
			q.post(&e)
			return
		}
		for len(data) > 0 { // chunks of up to maxNameLen/2 indices
			n := min(len(data), maxNameLen)
			e := event{kind: evFeedbackFormats, id: sid}
			e.setBytes(data[:n])
			q.post(&e)
			data = data[n:]
		}
	})
	s.feedback.OnDone(func() {
		e := event{kind: evFeedbackDone, id: sid}
		q.post(&e)
	})
}

// Codes of evBadEvent.
const (
	badFeedbackFD int32 = iota + 1
	badMainDevice
	badTranche
	badConfigure
)

var badEventErrors = [...]string{
	badFeedbackFD: "neferclient: dmabuf format table descriptor unavailable",
	badMainDevice: "neferclient: invalid dmabuf main_device length",
	badTranche:    "neferclient: odd dmabuf tranche format list",
	badConfigure:  "neferclient: configure size out of range",
}

// configureEvent builds a layer or lock configure event, or a badConfigure
// event when the compositor announced an out-of-range size.
func configureEvent(kind eventKind, sid, serial, w, h uint32) event {
	if w > maxLayerDimension || h > maxLayerDimension {
		return event{kind: evBadEvent, id: sid, a: badConfigure}
	}
	return event{kind: kind, id: sid, serial: serial, a: int32(w), b: int32(h)}
}

// NewLayerSurface creates a wlr-layer-shell surface. It becomes presentable
// after the first Handler.Configure.
func (c *Conn) NewLayerSurface(cfg LayerConfig) (*Surface, error) {
	if c.closed {
		return nil, ErrClosed
	}
	if err := validateLayer(&cfg); err != nil {
		return nil, err
	}
	if err := c.ensureLayerShell(); err != nil {
		return nil, err
	}
	if cfg.Keyboard == KeyboardOnDemand && c.g.layerVersion < 4 {
		return nil, &CapabilityError{Name: wlrlayershell.LayerShellInterface, Cause: fmt.Errorf("on-demand keyboard requires version 4, negotiated %d", c.g.layerVersion)}
	}
	var out *wayland.Output
	if cfg.Output != "" {
		for _, o := range c.outputs {
			if o.done && o.pub.Name == cfg.Output {
				out = o.proxy
				break
			}
		}
		if out == nil {
			return nil, &CapabilityError{Name: wayland.OutputInterface, Cause: fmt.Errorf("output %q not found", cfg.Output)}
		}
	}
	c.pause()
	defer c.resume()
	s, err := c.newSurface(roleLayer, cfg.Width, cfg.Height)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.destroy()
		}
	}()
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultLayerNamespace
	}
	level := [...]uint32{LayerBackground: wlrlayershell.LAYER_BACKGROUND, LayerBottom: wlrlayershell.LAYER_BOTTOM,
		LayerTop: wlrlayershell.LAYER_TOP, LayerOverlay: wlrlayershell.LAYER_OVERLAY}[cfg.Level]
	if s.layer, err = c.g.layer.GetLayerSurface(s.surf, out, level, ns); err != nil {
		return nil, err
	}
	q, sid := c.q, uint32(s.id)
	s.layer.OnClosed(func() {
		e := event{kind: evClosed, id: sid}
		q.post(&e)
	})
	s.layer.OnConfigure(func(serial, w, h uint32) {
		e := configureEvent(evLayerConfigure, sid, serial, w, h)
		q.post(&e)
	})
	// Opposite anchors let the compositor assign that axis (size 0).
	reqW, reqH := uint32(cfg.Width), uint32(cfg.Height)
	if cfg.Anchors&(AnchorLeft|AnchorRight) == AnchorLeft|AnchorRight {
		reqW = 0
	}
	if cfg.Anchors&(AnchorTop|AnchorBottom) == AnchorTop|AnchorBottom {
		reqH = 0
	}
	var anchor uint32
	for _, m := range [...][2]uint32{{uint32(AnchorTop), wlrlayershell.ANCHOR_TOP}, {uint32(AnchorBottom), wlrlayershell.ANCHOR_BOTTOM},
		{uint32(AnchorLeft), wlrlayershell.ANCHOR_LEFT}, {uint32(AnchorRight), wlrlayershell.ANCHOR_RIGHT}} {
		if uint32(cfg.Anchors)&m[0] != 0 {
			anchor |= m[1]
		}
	}
	for _, e := range []error{
		s.layer.SetSize(reqW, reqH),
		s.layer.SetAnchor(anchor),
		s.layer.SetExclusiveZone(cfg.ExclusiveZone),
		s.layer.SetMargin(cfg.Margin[0], cfg.Margin[1], cfg.Margin[2], cfg.Margin[3]),
		s.layer.SetKeyboardInteractivity(uint32(cfg.Keyboard)),
	} {
		if e != nil {
			return nil, e
		}
	}
	if cfg.InputRects != nil {
		if _, err = s.stageInputRects(cfg.InputRects); err != nil {
			return nil, err
		}
	}
	if err = s.finishSurface(); err != nil {
		return nil, err
	}
	if err = s.surf.Commit(); err != nil { // initial empty commit asks for configure
		return nil, err
	}
	ok = true
	return s, nil
}

// NewToplevel creates an xdg-toplevel window with the given title and initial
// logical size. It becomes presentable after the first Handler.Configure.
func (c *Conn) NewToplevel(title string, w, h int32) (*Surface, error) {
	if c.closed {
		return nil, ErrClosed
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("neferclient: invalid logical size %dx%d", w, h)
	}
	if err := c.ensureXdg(); err != nil {
		return nil, err
	}
	c.pause()
	defer c.resume()
	s, err := c.newSurface(roleToplevel, w, h)
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
	if s.toplevel, err = s.xdg.GetToplevel(); err != nil {
		return nil, err
	}
	q, sid := c.q, uint32(s.id)
	s.toplevel.OnClose(func() {
		e := event{kind: evClosed, id: sid}
		q.post(&e)
	})
	s.toplevel.OnConfigure(func(w, h int32, _ []byte) {
		e := event{kind: evXdgToplevel, id: sid, a: w, b: h}
		q.post(&e)
	})
	s.xdg.OnConfigure(func(serial uint32) {
		e := event{kind: evXdgConfigure, id: sid, serial: serial}
		q.post(&e)
	})
	if err = s.toplevel.SetTitle(title); err != nil {
		return nil, err
	}
	if err = s.finishSurface(); err != nil {
		return nil, err
	}
	if err = s.surf.Commit(); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// applySurface handles every surface, lock and xdg_wm_base event on the owner.
func (c *Conn) applySurface(ev *event, h Handler) {
	defer ev.closeFD()
	switch ev.kind {
	case evPing:
		if c.g.xdg != nil {
			if err := c.g.xdg.Pong(ev.serial); err != nil {
				h.Error(err)
			}
		}
		return
	case evLocked, evLockFinished:
		l := c.curLock
		if l == nil || l.gen != ev.id {
			return
		}
		if ev.kind == evLocked {
			l.locked = true
			h.Locked()
		} else {
			l.finished = true
			h.LockFinished()
		}
		return
	}
	s := c.surfaces[SurfaceID(ev.id)]
	if s == nil || s.closed {
		return // closed while the event was queued
	}
	id := s.id
	switch ev.kind {
	case evBadEvent:
		h.Error(errors.New(badEventErrors[ev.a]))
	case evClosed:
		h.Closed(id)
	case evXdgToplevel:
		s.pendW, s.pendH = ev.a, ev.b
	case evXdgConfigure:
		if s.pendW > 0 && s.pendH > 0 {
			s.width, s.height = s.pendW, s.pendH
		}
		s.configure(s.xdg.AckConfigure(ev.serial), h)
	case evLayerConfigure:
		if ev.a > 0 {
			s.width = ev.a
		}
		if ev.b > 0 {
			s.height = ev.b
		}
		s.configure(s.layer.AckConfigure(ev.serial), h)
	case evLockConfigure:
		s.width, s.height = ev.a, ev.b
		s.configure(s.lockSurf.AckConfigure(ev.serial), h)
	case evFrame:
		s.frameReady = true
		h.Frame(id)
	case evPreferredScale:
		if ev.a <= 0 {
			h.Error(errors.New("neferclient: invalid fractional scale zero"))
			return
		}
		s.setScale(float64(ev.a)/120, h)
	case evBufferScale:
		if s.frac == nil && ev.a > 0 {
			s.setScale(float64(ev.a), h)
		}
	case evFeedbackTable:
		fd := int(ev.fd)
		ev.fd = -1 // readTable consumes fd; the deferred closeFD must not touch the number again
		if err := s.readTable(fd, ev.flags); err != nil {
			h.Error(err)
		}
	case evFeedbackMainDevice:
		s.pending.MainDevice = ev.dev
	case evFeedbackFormats:
		b := ev.nameBytes()
		for i := 0; i+1 < len(b); i += 2 {
			idx := int(binary.NativeEndian.Uint16(b[i:]))
			if idx >= len(s.table) {
				h.Error(fmt.Errorf("neferclient: dmabuf format table index %d out of range", idx))
				continue
			}
			s.pending.Formats = append(s.pending.Formats, s.table[idx])
		}
	case evFeedbackDone:
		s.fb, s.pending = s.pending, Feedback{Formats: s.fb.Formats[:0]}
		s.fbDone = true
		h.FeedbackDone(id)
	}
}

func (s *Surface) configure(ackErr error, h Handler) {
	if ackErr != nil {
		h.Error(ackErr)
		return
	}
	if !s.configured {
		s.configured, s.frameReady = true, true
	}
	h.Configure(s.id, s.width, s.height)
}

func (s *Surface) setScale(v float64, h Handler) {
	if v == s.scale {
		return
	}
	s.scale = v
	h.Scale(s.id, v)
}

// readTable snapshots the format table with pread (no mapping of
// compositor-owned memory) and always consumes fd.
func (s *Surface) readTable(fd int, size uint32) error {
	defer unix.Close(fd)
	if size%16 != 0 || size > 16<<20 {
		return fmt.Errorf("neferclient: invalid dmabuf format table size %d", size)
	}
	c := s.c
	c.tableBuf = slices.Grow(c.tableBuf[:0], int(size))[:size]
	b := c.tableBuf
	for off := 0; off < len(b); {
		n, err := unix.Pread(fd, b[off:], int64(off))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("neferclient: read dmabuf format table: %w", err)
		}
		if n == 0 {
			return errors.New("neferclient: truncated dmabuf format table")
		}
		off += n
	}
	s.table = s.table[:0]
	for i := 0; i < len(b); i += 16 {
		s.table = append(s.table, Format{binary.NativeEndian.Uint32(b[i:]), binary.NativeEndian.Uint64(b[i+8:])})
	}
	return nil
}

// SetInputRegion sets the input region (logical pixels). nil restores the
// whole surface; a non-nil empty slice passes all input through. The region is
// double-buffered state: it is committed immediately only once the surface
// presented a buffer; before that the next Present applies it (committing
// earlier would map a surface without a buffer, which lock surfaces forbid and
// layer surfaces answer with an error). Lock surfaces always take all input
// and refuse it.
func (s *Surface) SetInputRegion(rects []Rect) error {
	if s.closed {
		return ErrClosed
	}
	if s.role == roleLock {
		return errors.New("neferclient: lock surfaces take all input")
	}
	changed, err := s.stageInputRects(rects)
	if err != nil || !changed || !s.presented {
		return err
	}
	return s.surf.Commit()
}

func (s *Surface) stageInputRects(rects []Rect) (bool, error) {
	if err := validateRects(rects); err != nil {
		return false, err
	}
	if (rects != nil) == s.inputCustom && slices.Equal(rects, s.inputRects) {
		return false, nil
	}
	if rects == nil {
		if err := s.surf.SetInputRegion(nil); err != nil {
			return false, err
		}
	} else {
		region, err := s.c.g.compositor.CreateRegion()
		if err != nil {
			return false, err
		}
		for _, r := range rects {
			if err = region.Add(r.X, r.Y, r.Width, r.Height); err != nil {
				break
			}
		}
		if err == nil {
			err = s.surf.SetInputRegion(region)
		}
		_ = region.Destroy()
		if err != nil {
			return false, err
		}
	}
	s.inputCustom = rects != nil
	s.inputRects = append(s.inputRects[:0], rects...)
	return true, nil
}

// Close destroys the surface, its buffers, timelines and role objects,
// children before parents. It is safe to call more than once. The caller must
// stop using buffers it imported first (the compositor keeps them until it
// processes the destruction).
func (s *Surface) Close() error {
	if s == nil || s.closed {
		return nil
	}
	if s.c.closed {
		s.closed = true
		return nil
	}
	return s.destroy()
}

func (s *Surface) destroy() error {
	s.closed = true
	delete(s.c.surfaces, s.id)
	s.c.seatSurfaceGone(s)
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	for id, b := range s.buffers {
		add(b.buf.Destroy())
		delete(s.buffers, id)
	}
	for id, t := range s.timelines {
		add(t.Destroy())
		delete(s.timelines, id)
	}
	if s.feedback != nil {
		add(s.feedback.Destroy())
	}
	if s.sync != nil {
		add(s.sync.Destroy())
	}
	if s.lockSurf != nil {
		add(s.lockSurf.Destroy())
	}
	if s.layer != nil {
		add(s.layer.Destroy())
	}
	if s.toplevel != nil {
		add(s.toplevel.Destroy())
	}
	if s.xdg != nil {
		add(s.xdg.Destroy())
	}
	if s.frac != nil {
		add(s.frac.Destroy())
	}
	if s.viewport != nil {
		add(s.viewport.Destroy())
	}
	if s.surf != nil {
		add(s.surf.Destroy())
	}
	if s.cb != nil {
		// A pending frame callback is owned by the compositor: abandon it so
		// its late done and delete_id are discarded. An error only means no
		// callback was pending.
		_ = s.c.wlctx.Abandon(s.cb)
	}
	return errors.Join(errs...)
}
