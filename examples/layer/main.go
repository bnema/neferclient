// Command layer shows a wlr-layer-shell surface with a counter button: the
// smallest end-to-end use of neferclient (Wayland client) and NeferGUI
// (renderer). The two libraries do not import each other; this file is the
// glue, and every step is marked.
//
// Everything runs on one owner goroutine: the loop in run waits for a wake-up,
// calls Conn.Dispatch, and all Handler methods, renderer calls and Present
// calls happen inside that call or right after it.
//
// Only the button takes pointer input: the view stages its rectangle with
// Frame.SetInputRects and the program forwards it to Surface.SetInputRegion,
// so the rest of the surface is click-through.
//
// With -frames N the program redraws continuously and exits after N frames
// (used by the headless test); without it, it runs until the surface is closed
// or it is interrupted. -log-pointer prints pointer enter and leave events.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
)

// Layout of the surface, in logical pixels: padding, a label row, a gap, then
// the button. The headless test samples a pixel inside the button and moves the
// pointer over the label and the button at the same places.
const (
	surfaceW, surfaceH = 240, 90
	pad, labelH, gap   = 10, 20, 10
	buttonW, buttonH   = 200, 40
	buttonY            = pad + labelH + gap
	buttonColor        = "#2060c0"

	// guiMods is the set of modifiers NeferGUI knows. neferclient and NeferGUI
	// use the same bit order for them, so the plain bits can be copied.
	guiMods = neferclient.ModShift | neferclient.ModCtrl | neferclient.ModAlt | neferclient.ModSuper
)

type model struct{ count int }

// The styles and the clickable rectangle are built once from the layout
// constants, so they cannot disagree. NeferGUI does not report where a control
// was laid out; inputRects is the button. SetInputRects copies the slice.
var (
	columnStyle = nefergui.Inline(fmt.Sprintf("padding:%dpx;gap:%dpx;color:#ffffff", pad, gap))
	labelStyle  = nefergui.Inline(fmt.Sprintf("height:%dpx", labelH))
	buttonStyle = nefergui.Inline(fmt.Sprintf("width:%dpx;height:%dpx;background:%s", buttonW, buttonH, buttonColor))
	inputRects  = []nefergui.Rect{{X: pad, Y: buttonY, Width: buttonW, Height: buttonH}}
)

// view builds one frame from the model. Button.Activated is true during the
// frame built after a click.
func view(f *nefergui.Frame, m *model) {
	root := f.Root().Column(columnStyle)
	root.TextInt("Count: ", int64(m.count), labelStyle)
	if root.Button("Add one", nefergui.Key("add"), buttonStyle).Activated() {
		m.count++
	}
	if err := f.SetInputRects(inputRects); err != nil {
		panic(err) // a fixed, valid rectangle: an error is a programming bug
	}
}

type app struct {
	neferclient.NopHandler // methods this program does not need

	conn *neferclient.Conn
	surf *neferclient.Surface
	seat *neferclient.Seat

	r      *nefergui.Renderer
	rwake  <-chan struct{} // Renderer.Wake: nil until the renderer exists
	out    nefergui.Output
	model  model
	damage []neferclient.Rect
	region []neferclient.Rect // input region sent to the compositor, reused

	logPointer bool

	configured  bool
	canPresent  bool // configured, and the last frame callback fired
	haveAcquire bool
	cursor      neferclient.CursorShape

	frames, shown int // -frames limit and frames acknowledged so far
	done          bool
	err           error
}

func main() {
	frames := flag.Int("frames", 0, "redraw continuously and exit after this many frames (0: run until closed)")
	logPointer := flag.Bool("log-pointer", false, "print pointer enter and leave events on stdout")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *frames, *logPointer); err != nil {
		fmt.Fprintln(os.Stderr, "layer:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, frames int, logPointer bool) (err error) {
	conn, err := neferclient.Connect(ctx, "") // WAYLAND_DISPLAY
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	// region starts non-nil so that an empty NeferGUI region stays an empty
	// slice (click-through) and does not become nil (whole surface).
	a := &app{conn: conn, frames: frames, logPointer: logPointer, cursor: neferclient.CursorDefault,
		region: []neferclient.Rect{}}
	// Shutdown order: close the connection first, which drops its descriptor
	// watches and destroys every Wayland object, then free the renderer and
	// its GPU resources (including the release eventfds).
	defer func() {
		err = errors.Join(err, conn.Close())
		if a.r != nil {
			err = errors.Join(err, a.r.Close())
		}
	}()
	a.surf, err = conn.NewLayerSurface(neferclient.LayerConfig{
		Level:    neferclient.LayerTop,
		Anchors:  neferclient.AnchorTop | neferclient.AnchorLeft,
		Width:    surfaceW,
		Height:   surfaceH,
		Keyboard: neferclient.KeyboardOnDemand,
	})
	if err != nil {
		return fmt.Errorf("layer surface: %w", err)
	}
	a.seat = conn.Seat()

	var tick <-chan time.Time
	for !a.done {
		select {
		case <-ctx.Done():
			return nil // interrupt or SIGTERM: a clean exit
		case <-conn.Wake():
			if err = conn.Dispatch(a); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case <-a.rwake: // another goroutine asked for a frame
		case <-tick: // a built frame was waiting for the GPU
		}
		if a.err != nil {
			return a.err
		}
		if err = a.draw(); err != nil {
			return err
		}
		tick = nil
		if a.r != nil && a.r.Pending() {
			tick = time.After(2 * time.Millisecond)
		}
	}
	return nil
}

// FeedbackDone: dmabuf feedback → nefergui.RendererConfig. The renderer opens
// the compositor's main device and picks a format and modifier from the list.
//
// Only the first complete feedback is used. A later one (the compositor
// changing its preferred device or formats) is ignored: this demo assumes a
// single GPU, and rebuilding the renderer is application policy that would
// obscure the glue. A real application would create a new Renderer and
// re-import its buffers here.
func (a *app) FeedbackDone(neferclient.SurfaceID) { a.setup() }

func (a *app) Configure(neferclient.SurfaceID, int32, int32) {
	if !a.configured { // later configures must not lift the frame-callback gate
		a.canPresent = true
	}
	a.configured = true
	a.resize()
	a.setup()
}

func (a *app) Scale(neferclient.SurfaceID, float64) { a.resize() }

func (a *app) setup() {
	fb := a.surf.Feedback()
	if a.r != nil || !a.configured || fb == nil {
		return
	}
	cfg := nefergui.RendererConfig{MainDevice: fb.MainDevice, Formats: make([]nefergui.Format, len(fb.Formats))}
	for i, f := range fb.Formats {
		cfg.Formats[i] = nefergui.Format{FourCC: f.FourCC, Modifier: f.Modifier}
	}
	r, err := nefergui.NewRenderer(cfg)
	if err != nil {
		a.fail(fmt.Errorf("renderer: %w", err))
		return
	}
	a.r, a.rwake = r, r.Wake()
	a.resize()
}

// resize forwards the logical size and scale to the renderer.
func (a *app) resize() {
	if a.r == nil {
		return
	}
	w, h, scale := a.surf.Size()
	a.r.Resize(int(w), int(h), scale)
}

// Frame: the compositor showed the last commit; the next Present is allowed.
func (a *app) Frame(neferclient.SurfaceID) {
	a.canPresent = true
	a.shown++
	if a.frames > 0 {
		if a.shown >= a.frames {
			a.done = true
			return
		}
		a.r.Invalidate()
	}
}

func (a *app) Closed(neferclient.SurfaceID) { a.done = true }

func (a *app) Error(err error) { a.fail(err) }

func (a *app) fail(err error) {
	if a.err == nil {
		a.err = err
	}
}

// FDReady: the release eventfd of a buffer is readable (watched with
// Conn.WatchFD in present, id is the buffer): hand the buffer back.
func (a *app) FDReady(id uint64) {
	if err := a.r.Released(id); err != nil {
		a.fail(fmt.Errorf("release buffer %d: %w", id, err))
	}
}

// Pointer and Key: neferclient events → nefergui.Input, field by field.
func (a *app) Pointer(ev *neferclient.PointerEvent) {
	if a.r == nil {
		return
	}
	in := nefergui.Input{X: ev.X, Y: ev.Y}
	switch ev.Kind {
	case neferclient.PointerEnter:
		in.Kind = nefergui.InputPointerMotion
		if a.logPointer {
			fmt.Printf("pointer enter %.0f %.0f\n", ev.X, ev.Y)
		}
		// The cursor shape is only valid after the enter: set the shape of
		// the last frame again.
		if err := a.seat.SetCursor(a.cursor); err != nil {
			a.fail(fmt.Errorf("cursor: %w", err))
		}
	case neferclient.PointerMotion:
		in.Kind = nefergui.InputPointerMotion
	case neferclient.PointerLeave:
		in.Kind = nefergui.InputPointerLeave
		if a.logPointer {
			fmt.Println("pointer leave")
		}
	case neferclient.PointerButton:
		in.Kind, in.Button = nefergui.InputPointerRelease, ev.Button
		if ev.Pressed {
			in.Kind = nefergui.InputPointerPress
		}
	case neferclient.PointerAxis:
		in.Kind, in.DX, in.DY = nefergui.InputPointerAxis, ev.DX, ev.DY
	default:
		return
	}
	a.r.Input(&in)
}

func (a *app) Key(ev *neferclient.KeyEvent) {
	if a.r == nil {
		return
	}
	a.r.Input(&nefergui.Input{
		Kind:      nefergui.InputKey,
		Keysym:    ev.Keysym,
		Text:      ev.Text, // valid only during the call, as for Input
		Pressed:   ev.Pressed,
		Repeat:    ev.Repeat,
		Modifiers: nefergui.Modifiers(ev.Modifiers & guiMods),
	})
}

func (a *app) KeyboardFocus(_ neferclient.SurfaceID, focused bool) {
	if a.r == nil {
		return
	}
	kind := nefergui.InputFocusOut
	if focused {
		kind = nefergui.InputFocusIn
	}
	a.r.Input(&nefergui.Input{Kind: kind})
}

// draw renders a frame when something changed and presents it.
func (a *app) draw() error {
	if a.r == nil || !a.canPresent {
		return nil
	}
	ok, err := a.r.Render(&a.out, &a.model, view)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if !ok {
		return nil
	}
	if err = a.setInputRegion(); err != nil {
		return err
	}
	if err = a.present(); err != nil {
		return err
	}
	return a.setCursor()
}

// setInputRegion: Output.InputRects → Surface.SetInputRegion, only when the
// view changed it. Both use logical pixels in surface coordinates, so the
// rectangles are copied field by field without scaling (Output.Width and
// Height, the buffer size, are the physical ones). Nil means the whole surface
// and an empty slice means click-through, in both libraries. Before the first
// Present the region is applied with it; afterwards SetInputRegion commits it
// immediately.
func (a *app) setInputRegion() error {
	out := &a.out
	if !out.InputRectsChanged {
		return nil
	}
	var region []neferclient.Rect // nil: the whole surface
	if out.InputRects != nil {
		a.region = a.region[:0] // reused: no allocation once it has grown
		for _, r := range out.InputRects {
			a.region = append(a.region, neferclient.Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height})
		}
		region = a.region
	}
	if err := a.surf.SetInputRegion(region); err != nil {
		return fmt.Errorf("input region: %w", err)
	}
	return nil
}

// present: Renderer.Render output → ImportBuffer, ImportTimeline, Present.
func (a *app) present() error {
	out := &a.out
	// A resize retired buffers: stop watching and destroy them first.
	for _, rt := range out.Retired {
		if err := a.conn.UnwatchFD(rt.ReleaseFD); err != nil {
			return fmt.Errorf("unwatch retired buffer %d: %w", rt.Buffer, err)
		}
		if err := a.surf.DestroyBuffer(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired buffer %d: %w", rt.Buffer, err)
		}
		// The release timeline of a buffer is imported under the buffer's id.
		if err := a.surf.DestroyTimeline(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired timeline %d: %w", rt.Buffer, err)
		}
	}
	if out.NewBuffer {
		buf := neferclient.Buffer{
			Width: out.Width, Height: out.Height, FourCC: out.FourCC, Modifier: out.Modifier,
			PlaneCount: out.PlaneCount,
		}
		for i, p := range out.Planes {
			buf.Planes[i] = neferclient.Plane{FD: p.FD, Offset: p.Offset, Stride: p.Stride}
		}
		if err := a.surf.ImportBuffer(out.Buffer, &buf); err != nil {
			return fmt.Errorf("import buffer: %w", err)
		}
		// The release eventfd is stable per buffer: watch it once; FDReady
		// calls Renderer.Released.
		if err := a.conn.WatchFD(out.ReleaseFD, out.Buffer); err != nil {
			return fmt.Errorf("watch release fd: %w", err)
		}
	}
	if out.NewTimelines {
		if !a.haveAcquire { // the acquire timeline is shared by every buffer
			if err := a.surf.ImportTimeline(out.Acquire.ID, out.Acquire.FD); err != nil {
				return fmt.Errorf("import acquire timeline: %w", err)
			}
			a.haveAcquire = true
		}
		if err := a.surf.ImportTimeline(out.Release.ID, out.Release.FD); err != nil {
			return fmt.Errorf("import release timeline: %w", err)
		}
	}
	a.damage = a.damage[:0]
	for _, d := range out.Damage {
		a.damage = append(a.damage, neferclient.Rect{X: d.X, Y: d.Y, Width: d.Width, Height: d.Height})
	}
	err := a.surf.Present(&neferclient.Present{
		Buffer:          out.Buffer,
		AcquireTimeline: out.Acquire.ID,
		ReleaseTimeline: out.Release.ID,
		AcquirePoint:    out.AcquirePoint,
		ReleasePoint:    out.ReleasePoint,
		Damage:          a.damage,
		Opaque:          true, // XRGB8888: RendererConfig.Transparent is false
	})
	if err != nil {
		return fmt.Errorf("present: %w", err)
	}
	a.canPresent = false // until Frame
	return nil
}

// setCursor: the cursor the hovered element asked for → Seat.SetCursor.
func (a *app) setCursor() error {
	switch a.out.Cursor {
	case nefergui.CursorPointer:
		a.cursor = neferclient.CursorPointer
	case nefergui.CursorText:
		a.cursor = neferclient.CursorText
	case nefergui.CursorNotAllowed:
		a.cursor = neferclient.CursorNotAllowed
	default:
		a.cursor = neferclient.CursorDefault
	}
	if err := a.seat.SetCursor(a.cursor); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}
	return nil
}
