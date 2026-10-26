package neferclient_test

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/neferclient"
	neferclientmocks "bnema.dev/neferclient/mocks"
)

const (
	wmBaseGlobal = 5
	layerGlobal  = 10
)

var popupGlobals = append(append([]wireGlobal{}, seatGlobals...), wireGlobal{layerGlobal, "zwlr_layer_shell_v1", 4})

// popupLog records every request the peer sees, in order.
type popupLog struct {
	mu   sync.Mutex
	reqs []popupReq
}

type popupReq struct {
	obj  uint32
	op   uint16
	body []byte
}

func newPopupLog(srv *wireServer) *popupLog {
	l := &popupLog{}
	srv.mu.Lock()
	srv.hook = func(obj uint32, op uint16, body []byte) {
		l.mu.Lock()
		l.reqs = append(l.reqs, popupReq{obj, op, append([]byte(nil), body...)})
		l.mu.Unlock()
	}
	srv.mu.Unlock()
	return l
}

func (l *popupLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

func (r popupReq) u32(i int) uint32 { return binary.LittleEndian.Uint32(r.body[4*i:]) }

// find returns the index of the first request at or after from that matches.
func (l *popupLog) find(from int, match func(popupReq) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.reqs); i++ {
		if match(l.reqs[i]) {
			return i
		}
	}
	return -1
}

// findLast returns the index of the last request that matches, or -1. Object
// ids are recycled once the compositor confirms a destruction, so a lookup of
// a live object must take the newest request.
func (l *popupLog) findLast(match func(popupReq) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.reqs) - 1; i >= 0; i-- {
		if match(l.reqs[i]) {
			return i
		}
	}
	return -1
}

// mustFind is find that fails the test when nothing matches.
func (l *popupLog) mustFind(t *testing.T, from int, match func(popupReq) bool, msg string) int {
	t.Helper()
	i := l.find(from, match)
	require.GreaterOrEqual(t, i, 0, msg)
	return i
}

func (l *popupLog) at(i int) popupReq {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs[i]
}

func (l *popupLog) all(match func(popupReq) bool) []popupReq {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []popupReq
	for _, r := range l.reqs {
		if match(r) {
			out = append(out, r)
		}
	}
	return out
}

func (l *popupLog) op(obj uint32, op uint16) func(popupReq) bool {
	return func(r popupReq) bool { return r.obj == obj && r.op == op }
}

// popupEnv is a connection with a layer surface, a toplevel and the request log.
type popupEnv struct {
	c     *neferclient.Conn
	srv   *wireServer
	log   *popupLog
	layer *neferclient.Surface
	top   *neferclient.Surface
}

func newPopupEnv(t *testing.T, globals []wireGlobal) *popupEnv {
	t.Helper()
	c, srv := connectWire(t, globals, surfaceOutputs)
	e := &popupEnv{c: c, srv: srv, log: newPopupLog(srv)}
	var err error
	e.top, err = c.NewToplevel("parent", 64, 48)
	require.NoError(t, err)
	hasLayer := slices.ContainsFunc(globals, func(g wireGlobal) bool { return g.iface == "zwlr_layer_shell_v1" })
	e.top.MarkPresentable(64, 48) // stands in for the configure round: popups need a configured parent
	if hasLayer {
		e.layer, err = c.NewLayerSurface(neferclient.LayerConfig{Level: neferclient.LayerTop,
			Anchors: neferclient.AnchorTop | neferclient.AnchorLeft, Width: 200, Height: 30})
		require.NoError(t, err)
		e.layer.MarkPresentable(200, 30)
	}
	require.NoError(t, c.Roundtrip())
	return e
}

func (e *popupEnv) wmBase() uint32 {
	e.srv.mu.Lock()
	defer e.srv.mu.Unlock()
	return e.srv.bound[wmBaseGlobal].id
}

// xdgSurfaceOf returns the xdg_surface id created for wl_surface surf.
// getPopup returns the newest xdg_surface.get_popup request of xdg.
func (e *popupEnv) getPopup(t *testing.T, xdg uint32) (int, popupReq) {
	t.Helper()
	i := e.log.findLast(e.log.op(xdg, 2))
	require.GreaterOrEqual(t, i, 0, "xdg_surface.get_popup")
	return i, e.log.at(i)
}

func (e *popupEnv) xdgSurfaceOf(t *testing.T, surf uint32) uint32 {
	t.Helper()
	wm := e.wmBase()
	i := e.log.findLast(func(r popupReq) bool { return r.obj == wm && r.op == 2 && len(r.body) == 8 && r.u32(1) == surf })
	require.GreaterOrEqual(t, i, 0, "xdg_wm_base.get_xdg_surface")
	return e.log.at(i).u32(0)
}

// configurePopup has the peer configure popup p and dispatches it, which
// makes p a valid parent for further popups.
func (e *popupEnv) configurePopup(t *testing.T, p *neferclient.Surface) {
	t.Helper()
	require.NoError(t, e.c.Roundtrip())
	xdg := e.xdgSurfaceOf(t, p.SurfaceObjectID())
	_, gp := e.getPopup(t, xdg)
	e.srv.write(frame(gp.u32(0), 0, appendU32(appendU32(appendU32(appendU32(nil, 0), 0), 60), 40)))
	e.srv.write(frame(xdg, 0, appendU32(nil, 1)))
	require.NoError(t, e.c.Roundtrip())
	dispatchUntil(t, e.c, neferclient.NopHandler{}, func() bool { w, _, _ := p.Size(); return w == 60 })
	require.NoError(t, e.c.Roundtrip()) // the ack_configure reached the peer
}

// popupID returns the xdg_popup object id of p.
func (e *popupEnv) popupID(t *testing.T, p *neferclient.Surface) uint32 {
	t.Helper()
	_, gp := e.getPopup(t, e.xdgSurfaceOf(t, p.SurfaceObjectID()))
	return gp.u32(0)
}

var place = neferclient.PopupPlacement{
	Anchor: neferclient.Rect{X: 10, Y: 0, Width: 20, Height: 30},
	Edge:   neferclient.EdgeBottom, Gravity: neferclient.EdgeBottomRight,
	Adjust: neferclient.AdjustSlideX | neferclient.AdjustFlipY | neferclient.AdjustResizeY,
	Offset: [2]int32{3, 4}, Width: 120, Height: 80,
}

func TestPopupOnLayerParentGrabsBeforeFirstCommit(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	n := e.log.len()
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.layer, Place: place, Grab: true, Serial: 42})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())

	popupSurf := p.SurfaceObjectID()
	xdg := e.xdgSurfaceOf(t, popupSurf)
	getPopup, gp := e.getPopup(t, xdg)
	popupID, positioner := gp.u32(0), gp.u32(2)
	require.Zero(t, gp.u32(1), "layer parent: null xdg parent")

	// The layer surface is the one created for e.layer.
	layerID := e.log.at(e.log.mustFind(t, 0, func(r popupReq) bool { return r.op == 0 && len(r.body) > 8 && r.u32(1) == e.layer.SurfaceObjectID() }, "get_layer_surface")).u32(0)
	layerPopup := e.log.mustFind(t, n, e.log.op(layerID, 5), "layer_surface.get_popup")
	require.Greater(t, layerPopup, getPopup, "layer_surface.get_popup follows xdg_surface.get_popup")
	require.Equal(t, popupID, e.log.at(layerPopup).u32(0))

	grab := e.log.mustFind(t, n, e.log.op(popupID, 1), "xdg_popup.grab")
	require.Greater(t, grab, layerPopup, "grab follows get_popup")
	require.Equal(t, uint32(42), e.log.at(grab).u32(1))
	require.NotZero(t, e.log.at(grab).u32(0), "seat")
	commit := e.log.mustFind(t, n, e.log.op(popupSurf, 6), "wl_surface.commit")
	require.Greater(t, commit, grab, "grab precedes the first commit")

	// Positioner state.
	pos := func(op uint16) popupReq {
		i := e.log.mustFind(t, n, e.log.op(positioner, op), "positioner request")
		require.Less(t, i, getPopup)
		return e.log.at(i)
	}
	require.Equal(t, []uint32{120, 80}, []uint32{pos(1).u32(0), pos(1).u32(1)})
	require.Equal(t, []uint32{10, 0, 20, 30}, []uint32{pos(2).u32(0), pos(2).u32(1), pos(2).u32(2), pos(2).u32(3)})
	require.Equal(t, uint32(neferclient.EdgeBottom), pos(3).u32(0))
	require.Equal(t, uint32(neferclient.EdgeBottomRight), pos(4).u32(0))
	require.Equal(t, uint32(1|8|32), pos(5).u32(0))
	require.Equal(t, []uint32{3, 4}, []uint32{pos(6).u32(0), pos(6).u32(1)})
	require.NotEmpty(t, e.log.all(e.log.op(positioner, 0)), "positioner destroyed")
}

func TestPopupOnToplevelParentUsesParentXdgSurface(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	n := e.log.len()
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.top, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	parentXdg := e.xdgSurfaceOf(t, e.top.SurfaceObjectID())
	xdg := e.xdgSurfaceOf(t, p.SurfaceObjectID())
	_, gp := e.getPopup(t, xdg)
	require.Equal(t, parentXdg, gp.u32(1))
	popupID := gp.u32(0)
	require.Negative(t, e.log.find(n, e.log.op(popupID, 1)), "no grab requested")
}

func TestPopupNestedOnPopup(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.top, Place: place})
	require.NoError(t, err)
	_, err = e.c.NewPopup(neferclient.PopupConfig{Parent: p, Place: place})
	require.ErrorContains(t, err, "not configured yet", "a popup parent is mapped only once configured")
	e.configurePopup(t, p)
	child, err := e.c.NewPopup(neferclient.PopupConfig{Parent: p, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	_, gp := e.getPopup(t, e.xdgSurfaceOf(t, child.SurfaceObjectID()))
	require.Equal(t, e.xdgSurfaceOf(t, p.SurfaceObjectID()), gp.u32(1))
	require.Error(t, p.Close(), "popup with an open child")
	require.NoError(t, child.Close())
	require.NoError(t, p.Close())
}

func TestPopupConfigureAcksAndDone(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.layer, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	xdg := e.xdgSurfaceOf(t, p.SurfaceObjectID())
	_, gp := e.getPopup(t, xdg)
	popupID := gp.u32(0)

	var w, h int32
	var done bool
	hd := neferclientmocks.NewMockHandler(t)
	hd.EXPECT().Configure(p.ID(), mock.Anything, mock.Anything).Run(func(_ neferclient.SurfaceID, cw, ch int32) { w, h = cw, ch }).Return().Once()
	hd.EXPECT().PopupDone(p.ID()).Run(func(neferclient.SurfaceID) { done = true }).Return().Once()

	cfg := appendU32(appendU32(appendU32(appendU32(nil, 5), 6), 120), 80)
	e.srv.write(frame(popupID, 0, cfg))           // xdg_popup.configure(5,6,120,80)
	e.srv.write(frame(xdg, 0, appendU32(nil, 9))) // xdg_surface.configure(9)
	require.NoError(t, e.c.Roundtrip())
	dispatchUntil(t, e.c, hd, func() bool { return w != 0 })
	require.Equal(t, [2]int32{120, 80}, [2]int32{w, h})
	require.NoError(t, e.c.Roundtrip())
	ack := e.log.mustFind(t, 0, e.log.op(xdg, 4), "ack_configure")
	require.Equal(t, uint32(9), e.log.at(ack).u32(0))
	gw, gh, _ := p.Size()
	require.Equal(t, [2]int32{120, 80}, [2]int32{gw, gh})

	e.srv.write(frame(popupID, 1, nil)) // popup_done
	require.NoError(t, e.c.Roundtrip())
	dispatchUntil(t, e.c, hd, func() bool { return done })
	require.NoError(t, p.Close())
	require.NoError(t, e.c.Roundtrip())
	destroyPopup := e.log.mustFind(t, 0, e.log.op(popupID, 0), "xdg_popup.destroy")
	destroyXdg := e.log.mustFind(t, 0, e.log.op(xdg, 0), "xdg_surface.destroy")
	require.Less(t, destroyPopup, destroyXdg, "popup destroyed before its xdg_surface")
}

func TestPopupReposition(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.layer, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	xdg := e.xdgSurfaceOf(t, p.SurfaceObjectID())
	_, gp := e.getPopup(t, xdg)
	popupID := gp.u32(0)

	np := place
	np.Anchor.X, np.Width = 50, 90
	n := e.log.len()
	require.NoError(t, p.Reposition(np))
	require.NoError(t, e.c.Roundtrip())
	i := e.log.mustFind(t, n, e.log.op(popupID, 2), "xdg_popup.reposition")
	positioner := e.log.at(i).u32(0)
	token := e.log.at(i).u32(1)
	setSize := e.log.at(e.log.mustFind(t, n, e.log.op(positioner, 1), "positioner.set_size"))
	require.Equal(t, uint32(90), setSize.u32(0))
	require.Less(t, e.log.mustFind(t, n, e.log.op(positioner, 1), "set_size"), i)
	require.Greater(t, e.log.mustFind(t, i, e.log.op(positioner, 0), "positioner.destroy"), i)

	n = e.log.len()
	require.NoError(t, p.Reposition(np))
	require.NoError(t, e.c.Roundtrip())
	second := e.log.at(e.log.mustFind(t, n, e.log.op(popupID, 2), "second xdg_popup.reposition"))
	require.NotEqual(t, token, second.u32(1), "each reposition carries its own token")

	bad := np
	bad.Width = 0
	require.Error(t, p.Reposition(bad))
	require.Error(t, e.top.Reposition(np), "not a popup")
}

func TestPopupRepositionNeedsXdgV3(t *testing.T) {
	old := append([]wireGlobal{}, popupGlobals...)
	old[wmBaseGlobal-1].version = 2
	require.Equal(t, "xdg_wm_base", old[wmBaseGlobal-1].iface)
	e := newPopupEnv(t, old)
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.layer, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	n := e.log.len()
	var ce *neferclient.CapabilityError
	require.ErrorAs(t, p.Reposition(place), &ce)
	require.NoError(t, e.c.Roundtrip())
	require.Equal(t, n, e.log.len(), "nothing sent")
}

func TestPopupInvalidConfigsSendNothing(t *testing.T) {
	e := newPopupEnv(t, surfaceGlobals) // no seat, no layer shell
	closed, err := e.c.NewToplevel("closed", 64, 48)
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	unconfigured, err := e.c.NewToplevel("unconfigured", 64, 48)
	require.NoError(t, err)
	l, err := e.c.Lock()
	require.NoError(t, err)
	lockSurf, err := l.NewSurface(7)
	require.NoError(t, err)
	lockSurf.MarkPresentable(64, 48)
	other, _ := connectWire(t, surfaceGlobals, surfaceOutputs)
	foreign, err := other.NewToplevel("foreign", 64, 48)
	require.NoError(t, err)
	foreign.MarkPresentable(64, 48)
	require.NoError(t, e.c.Roundtrip())
	n := e.log.len()

	with := func(f func(*neferclient.PopupConfig)) neferclient.PopupConfig {
		cfg := neferclient.PopupConfig{Parent: e.top, Place: place}
		f(&cfg)
		return cfg
	}
	var capErr *neferclient.CapabilityError
	for name, tc := range map[string]struct {
		cfg  neferclient.PopupConfig
		want func(error)
	}{
		"nil parent":      {with(func(c *neferclient.PopupConfig) { c.Parent = nil }), func(err error) { require.ErrorContains(t, err, "needs a parent surface") }},
		"foreign parent":  {with(func(c *neferclient.PopupConfig) { c.Parent = foreign }), func(err error) { require.ErrorContains(t, err, "of this connection") }},
		"closed parent":   {with(func(c *neferclient.PopupConfig) { c.Parent = closed }), func(err error) { require.ErrorIs(t, err, neferclient.ErrClosed) }},
		"lock parent":     {with(func(c *neferclient.PopupConfig) { c.Parent = lockSurf }), func(err error) { require.ErrorContains(t, err, "must be a layer surface, a toplevel or a popup") }},
		"unconfigured":    {with(func(c *neferclient.PopupConfig) { c.Parent = unconfigured }), func(err error) { require.ErrorContains(t, err, "not configured yet") }},
		"zero width":      {with(func(c *neferclient.PopupConfig) { c.Place.Width = 0 }), func(err error) { require.ErrorContains(t, err, "invalid popup size") }},
		"negative height": {with(func(c *neferclient.PopupConfig) { c.Place.Height = -1 }), func(err error) { require.ErrorContains(t, err, "invalid popup size") }},
		"empty anchor":    {with(func(c *neferclient.PopupConfig) { c.Place.Anchor.Width = 0 }), func(err error) { require.ErrorContains(t, err, "anchor rectangle") }},
		"anchor overflow": {with(func(c *neferclient.PopupConfig) {
			c.Place.Anchor = neferclient.Rect{X: math.MaxInt32, Width: 10, Height: 5}
		}), func(err error) { require.ErrorContains(t, err, "overflows") }},
		"bad edge":       {with(func(c *neferclient.PopupConfig) { c.Place.Edge = 9 }), func(err error) { require.ErrorContains(t, err, "invalid popup edge") }},
		"bad gravity":    {with(func(c *neferclient.PopupConfig) { c.Place.Gravity = 9 }), func(err error) { require.ErrorContains(t, err, "invalid popup edge") }},
		"bad adjust":     {with(func(c *neferclient.PopupConfig) { c.Place.Adjust = 64 }), func(err error) { require.ErrorContains(t, err, "constraint adjustment") }},
		"grab no serial": {with(func(c *neferclient.PopupConfig) { c.Grab = true }), func(err error) { require.ErrorContains(t, err, "serial") }},
		"grab no seat":   {with(func(c *neferclient.PopupConfig) { c.Grab, c.Serial = true, 1 }), func(err error) { require.ErrorAs(t, err, &capErr) }},
	} {
		_, err := e.c.NewPopup(tc.cfg)
		require.Error(t, err, name)
		tc.want(err)
	}
	require.NoError(t, e.c.Roundtrip())
	require.Equal(t, n, e.log.len(), "no request was sent")
	require.NoError(t, e.top.Close(), "failed popups left no child behind")
}

// A failed seat bind is the cause of the grab's CapabilityError.
func TestPopupGrabReportsSeatBindError(t *testing.T) {
	e := newPopupEnv(t, surfaceGlobals)
	bindErr := errors.New("bind refused")
	e.c.SetSeatErr(bindErr)
	_, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.top, Place: place, Grab: true, Serial: 1})
	var ce *neferclient.CapabilityError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, "wl_seat", ce.Name)
	require.ErrorIs(t, err, bindErr)
}

// A grab needs a grabbing parent popup, and while a grab chain is open only
// its topmost popup can be the parent of the next grab. Refusals send nothing.
func TestPopupGrabChain(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	grab := func(parent *neferclient.Surface) (*neferclient.Surface, error) {
		return e.c.NewPopup(neferclient.PopupConfig{Parent: parent, Place: place, Grab: true, Serial: 7})
	}
	refused := func(parent *neferclient.Surface, want string) {
		t.Helper()
		require.NoError(t, e.c.Roundtrip()) // earlier requests are logged
		n := e.log.len()
		_, err := grab(parent)
		require.ErrorContains(t, err, want)
		require.NoError(t, e.c.Roundtrip())
		require.Equal(t, n, e.log.len(), "no request was sent")
	}

	plain, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.layer, Place: place})
	require.NoError(t, err)
	e.configurePopup(t, plain)
	refused(plain, "needs a grabbing parent popup")

	a, err := grab(e.layer)
	require.NoError(t, err)
	e.configurePopup(t, a)
	refused(e.top, "topmost grabbing popup")
	refused(e.layer, "topmost grabbing popup")
	refused(plain, "needs a grabbing parent popup")

	b, err := grab(a)
	require.NoError(t, err)
	e.configurePopup(t, b)
	refused(a, "topmost grabbing popup")

	// Closing the topmost popup returns the grab to its parent.
	require.NoError(t, b.Close())
	c2, err := grab(a)
	require.NoError(t, err)
	require.NoError(t, c2.Close())
	require.NoError(t, a.Close())

	// A popup_done ends the grab of the dismissed popup, too.
	a2, err := grab(e.layer)
	require.NoError(t, err)
	refused(e.top, "topmost grabbing popup")
	e.srv.write(frame(e.popupID(t, a2), 1, nil))
	require.NoError(t, e.c.Roundtrip())
	var done []neferclient.SurfaceID
	hd := neferclientmocks.NewMockHandler(t)
	hd.EXPECT().PopupDone(mock.Anything).Run(func(id neferclient.SurfaceID) { done = append(done, id) }).Return().Once()
	dispatchUntil(t, e.c, hd, func() bool { return len(done) > 0 })
	require.Equal(t, []neferclient.SurfaceID{a2.ID()}, done)
	top, err := grab(e.top)
	require.NoError(t, err, "the chain is closed after popup_done")
	require.NoError(t, top.Close())
	require.NoError(t, a2.Close())
	require.NoError(t, plain.Close())
}
