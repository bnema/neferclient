package neferclient_test

import (
	"encoding/binary"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/neferclient"
	neferclientmocks "github.com/bnema/neferclient/mocks"
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
	if len(globals) > 8 {
		e.layer, err = c.NewLayerSurface(neferclient.LayerConfig{Level: neferclient.LayerTop,
			Anchors: neferclient.AnchorTop | neferclient.AnchorLeft, Width: 200, Height: 30})
		require.NoError(t, err)
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
func (e *popupEnv) xdgSurfaceOf(t *testing.T, surf uint32) uint32 {
	t.Helper()
	wm := e.wmBase()
	i := e.log.find(0, func(r popupReq) bool { return r.obj == wm && r.op == 2 && len(r.body) == 8 && r.u32(1) == surf })
	require.GreaterOrEqual(t, i, 0, "xdg_wm_base.get_xdg_surface")
	return e.log.at(i).u32(0)
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
	getPopup := e.log.find(n, e.log.op(xdg, 2))
	require.GreaterOrEqual(t, getPopup, 0, "xdg_surface.get_popup")
	gp := e.log.at(getPopup)
	popupID, positioner := gp.u32(0), gp.u32(2)
	require.Zero(t, gp.u32(1), "layer parent: null xdg parent")

	// The layer surface is the one created for e.layer.
	layerID := e.log.at(e.log.find(0, func(r popupReq) bool { return r.op == 0 && len(r.body) > 8 && r.u32(1) == e.layer.SurfaceObjectID() })).u32(0)
	layerPopup := e.log.find(n, e.log.op(layerID, 5))
	require.Greater(t, layerPopup, getPopup, "layer_surface.get_popup follows xdg_surface.get_popup")
	require.Equal(t, popupID, e.log.at(layerPopup).u32(0))

	grab := e.log.find(n, e.log.op(popupID, 1))
	require.Greater(t, grab, layerPopup, "grab follows get_popup")
	require.Equal(t, uint32(42), e.log.at(grab).u32(1))
	require.NotZero(t, e.log.at(grab).u32(0), "seat")
	commit := e.log.find(n, e.log.op(popupSurf, 6))
	require.Greater(t, commit, grab, "grab precedes the first commit")

	// Positioner state.
	pos := func(op uint16) popupReq {
		i := e.log.find(n, e.log.op(positioner, op))
		require.GreaterOrEqual(t, i, 0, "positioner op %d", op)
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
	i := e.log.find(n, e.log.op(xdg, 2))
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, parentXdg, e.log.at(i).u32(1))
	popupID := e.log.at(i).u32(0)
	require.Negative(t, e.log.find(n, e.log.op(popupID, 1)), "no grab requested")
}

func TestPopupNestedOnPopup(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.top, Place: place})
	require.NoError(t, err)
	child, err := e.c.NewPopup(neferclient.PopupConfig{Parent: p, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	i := e.log.find(0, e.log.op(e.xdgSurfaceOf(t, child.SurfaceObjectID()), 2))
	require.Equal(t, e.xdgSurfaceOf(t, p.SurfaceObjectID()), e.log.at(i).u32(1))
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
	popupID := e.log.at(e.log.find(0, e.log.op(xdg, 2))).u32(0)

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
	ack := e.log.find(0, e.log.op(xdg, 4))
	require.GreaterOrEqual(t, ack, 0, "ack_configure")
	require.Equal(t, uint32(9), e.log.at(ack).u32(0))
	gw, gh, _ := p.Size()
	require.Equal(t, [2]int32{120, 80}, [2]int32{gw, gh})

	e.srv.write(frame(popupID, 1, nil)) // popup_done
	require.NoError(t, e.c.Roundtrip())
	dispatchUntil(t, e.c, hd, func() bool { return done })
	require.NoError(t, p.Close())
	require.NoError(t, e.c.Roundtrip())
	require.NotEmpty(t, e.log.all(e.log.op(popupID, 0)), "xdg_popup.destroy")
	destroyPopup := e.log.find(0, e.log.op(popupID, 0))
	destroyXdg := e.log.find(0, e.log.op(xdg, 0))
	require.Less(t, destroyPopup, destroyXdg, "popup destroyed before its xdg_surface")
}

func TestPopupReposition(t *testing.T) {
	e := newPopupEnv(t, popupGlobals)
	p, err := e.c.NewPopup(neferclient.PopupConfig{Parent: e.layer, Place: place})
	require.NoError(t, err)
	require.NoError(t, e.c.Roundtrip())
	xdg := e.xdgSurfaceOf(t, p.SurfaceObjectID())
	popupID := e.log.at(e.log.find(0, e.log.op(xdg, 2))).u32(0)

	np := place
	np.Anchor.X, np.Width = 50, 90
	require.NoError(t, p.Reposition(np))
	require.NoError(t, e.c.Roundtrip())
	i := e.log.find(0, e.log.op(popupID, 2))
	require.GreaterOrEqual(t, i, 0, "xdg_popup.reposition")
	positioner := e.log.at(i).u32(0)
	require.NotEqual(t, uint32(0), e.log.at(i).u32(1), "token")
	setSize := e.log.at(e.log.find(i-8, e.log.op(positioner, 1)))
	require.Equal(t, uint32(90), setSize.u32(0))
	require.NotEmpty(t, e.log.all(e.log.op(positioner, 0)), "positioner destroyed")

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
	require.NoError(t, e.c.Roundtrip())
	n := e.log.len()

	with := func(f func(*neferclient.PopupConfig)) neferclient.PopupConfig {
		cfg := neferclient.PopupConfig{Parent: e.top, Place: place}
		f(&cfg)
		return cfg
	}
	for name, cfg := range map[string]neferclient.PopupConfig{
		"nil parent":      with(func(c *neferclient.PopupConfig) { c.Parent = nil }),
		"closed parent":   with(func(c *neferclient.PopupConfig) { c.Parent = closed }),
		"zero width":      with(func(c *neferclient.PopupConfig) { c.Place.Width = 0 }),
		"negative height": with(func(c *neferclient.PopupConfig) { c.Place.Height = -1 }),
		"empty anchor":    with(func(c *neferclient.PopupConfig) { c.Place.Anchor.Width = 0 }),
		"bad edge":        with(func(c *neferclient.PopupConfig) { c.Place.Edge = 9 }),
		"bad gravity":     with(func(c *neferclient.PopupConfig) { c.Place.Gravity = 9 }),
		"bad adjust":      with(func(c *neferclient.PopupConfig) { c.Place.Adjust = 64 }),
		"grab no serial":  with(func(c *neferclient.PopupConfig) { c.Grab = true }),
	} {
		_, err := e.c.NewPopup(cfg)
		require.Error(t, err, name)
	}
	var ce *neferclient.CapabilityError
	_, err = e.c.NewPopup(with(func(c *neferclient.PopupConfig) { c.Grab, c.Serial = true, 1 }))
	require.ErrorAs(t, err, &ce, "grab without a seat")
	require.NoError(t, e.c.Roundtrip())
	require.Equal(t, n, e.log.len(), "no request was sent")
	require.NoError(t, e.top.Close(), "failed popups left no child behind")
}
