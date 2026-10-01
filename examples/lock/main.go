// Command lock shows an ext-session-lock-v1 session lock with a password
// field, drawn by NeferGUI.
//
// THIS IS A DEMO, NOT A SCREEN LOCKER. It performs no authentication: pressing
// Enter unlocks the session whatever was typed, so anyone at the keyboard can
// open it. Do not use it, or code copied from it, to protect a real session. A
// real locker must verify the secret (PAM, for example) before it calls
// Lock.Unlock. Run it on a real session only if you accept that.
//
// The typed text goes into a neferclient.SecretBuffer, never into the view or
// the renderer: the field shows Node.Masked with the count from
// SecretBuffer.Len(). The program never converts the secret to a string and
// never prints it; the buffer is wiped on unlock and on exit.
//
// The program prints "locked" and "unlocked" on standard output as progress
// markers (the headless test waits for them).
//
// All logic runs on the owner goroutine, inside or right after Conn.Dispatch.
// The lock needs one surface per output; each gets its own Renderer.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
)

const keysymReturn, keysymKPEnter = 0xff0d, 0xff8d // xkb keysyms

// model is what the view reads. The secret stays in its buffer; the view only
// asks for its length.
type model struct{ secret *neferclient.SecretBuffer }

func view(f *nefergui.Frame, m *model) {
	root := f.Root().Column(nefergui.Inline("padding:40px;gap:12px;color:#ffffff;background:#101820"))
	root.Heading("Session locked (demo: no authentication)")
	root.Text("Type anything, then press Enter to unlock.")
	field := root.Row(nefergui.Inline("width:320px;height:36px;padding:6px;background:#203040;border:1px solid #6080a0"))
	field.Masked(m.secret.Len()) // bullets only: the view never sees the text
}

// screen is the lock surface of one output and its renderer.
type screen struct {
	conn   *neferclient.Conn
	model  *model
	surf   *neferclient.Surface
	r      *nefergui.Renderer
	out    nefergui.Output
	damage []neferclient.Rect

	configured, canPresent, haveAcquire bool
}

type app struct {
	neferclient.NopHandler

	conn    *neferclient.Conn
	lock    *neferclient.Lock
	seat    *neferclient.Seat
	secret  *neferclient.SecretBuffer
	model   model
	screens map[neferclient.SurfaceID]*screen

	cursor           neferclient.CursorShape
	locked, unlocked bool
	err              error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "lock:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) (err error) {
	conn, err := neferclient.Connect(ctx, "") // WAYLAND_DISPLAY
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	a := &app{conn: conn, screens: map[neferclient.SurfaceID]*screen{}, cursor: neferclient.CursorDefault}
	a.secret = neferclient.NewSecretBuffer(256)
	a.model.secret = a.secret
	defer func() {
		a.secret.Wipe()
		for _, s := range a.screens {
			err = errors.Join(err, s.close())
		}
		err = errors.Join(err, conn.Close())
	}()

	// The compositor may announce its first output just after Connect.
	for len(conn.Outputs()) == 0 {
		select {
		case <-conn.Wake():
			if err = conn.Dispatch(a); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.lock, err = conn.Lock(); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	for _, o := range conn.Outputs() {
		if err = a.addScreen(o.Global); err != nil {
			return err
		}
	}
	// Typed text goes into the secret buffer, not into Handler.Key.
	a.seat = conn.Seat()
	a.seat.SetSecret(a.secret)
	defer a.seat.SetSecret(nil)

	var tick <-chan time.Time
	for !a.unlocked {
		select {
		case <-ctx.Done():
			return ctx.Err() // the session stays locked: only Unlock ends it
		case <-conn.Wake():
			if err = conn.Dispatch(a); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case <-tick:
		}
		if a.err != nil {
			return a.err
		}
		if a.unlocked {
			break
		}
		pending := false
		for _, s := range a.screens {
			shown, err := s.draw()
			if err != nil {
				return err
			}
			if shown {
				// The cursor the hovered element asked for → Seat.SetCursor.
				a.cursor = cursorShape(s.out.Cursor)
				if err = a.seat.SetCursor(a.cursor); err != nil {
					return fmt.Errorf("cursor: %w", err)
				}
			}
			pending = pending || (s.r != nil && s.r.Pending())
		}
		tick = nil
		if pending {
			tick = time.After(2 * time.Millisecond)
		}
	}
	return nil
}

func (a *app) addScreen(output uint32) error {
	surf, err := a.lock.NewSurface(output)
	if err != nil {
		return fmt.Errorf("lock surface: %w", err)
	}
	a.screens[surf.ID()] = &screen{conn: a.conn, model: &a.model, surf: surf}
	return nil
}

func (a *app) fail(err error) {
	if a.err == nil {
		a.err = err
	}
}

func (a *app) Error(err error) { a.fail(err) }

// OutputAdded: a new output must be covered too while the session is locked.
func (a *app) OutputAdded(out *neferclient.Output) {
	if a.lock == nil || a.unlocked {
		return
	}
	if err := a.addScreen(out.Global); err != nil {
		a.fail(err)
	}
}

func (a *app) Locked() {
	a.locked = true
	fmt.Println("locked")
}

func (a *app) LockFinished() {
	a.fail(errors.Join(errors.New("the compositor refused or ended the lock"), a.lock.Close()))
}

// FeedbackDone: dmabuf feedback → nefergui.RendererConfig.
func (a *app) FeedbackDone(id neferclient.SurfaceID) {
	if s := a.screens[id]; s != nil {
		a.fail(s.setup())
	}
}

func (a *app) Configure(id neferclient.SurfaceID, _, _ int32) {
	if s := a.screens[id]; s != nil {
		s.configured, s.canPresent = true, true
		s.resize()
		a.fail(s.setup())
	}
}

func (a *app) Scale(id neferclient.SurfaceID, _ float64) {
	if s := a.screens[id]; s != nil {
		s.resize()
	}
}

func (a *app) Frame(id neferclient.SurfaceID) {
	if s := a.screens[id]; s != nil {
		s.canPresent = true
	}
}

// FDReady: the release eventfd of a buffer is readable (watched in present;
// the id packs the surface and the buffer): hand the buffer back.
func (a *app) FDReady(id uint64) {
	s := a.screens[neferclient.SurfaceID(id>>32)]
	if s == nil || s.r == nil {
		return
	}
	if err := s.r.Released(id & 0xffffffff); err != nil {
		a.fail(fmt.Errorf("release buffer: %w", err))
	}
}

// Pointer: neferclient events → nefergui.Input, field by field.
func (a *app) Pointer(ev *neferclient.PointerEvent) {
	s := a.screens[ev.Surface]
	if s == nil || s.r == nil {
		return
	}
	in := nefergui.Input{X: ev.X, Y: ev.Y, Kind: nefergui.InputPointerMotion}
	switch ev.Kind {
	case neferclient.PointerEnter:
		// The shape only applies after an enter: send the last one again.
		a.fail(a.seat.SetCursor(a.cursor))
	case neferclient.PointerLeave:
		in.Kind = nefergui.InputPointerLeave
	case neferclient.PointerButton:
		in.Kind, in.Button = nefergui.InputPointerRelease, ev.Button
		if ev.Pressed {
			in.Kind = nefergui.InputPointerPress
		}
	case neferclient.PointerAxis:
		in.Kind, in.DX, in.DY = nefergui.InputPointerAxis, ev.DX, ev.DY
	}
	s.r.Input(&in)
}

// Key: Enter unlocks (DEMO: no authentication). Keys that produced text were
// already stored in the SecretBuffer and carry no keysym or text: they are not
// forwarded, so the renderer never sees the secret.
func (a *app) Key(ev *neferclient.KeyEvent) {
	if ev.Secret {
		return
	}
	if ev.Pressed && !ev.Repeat && (ev.Keysym == keysymReturn || ev.Keysym == keysymKPEnter) {
		a.unlock()
		return
	}
	if s := a.screens[ev.Surface]; s != nil && s.r != nil {
		s.r.Input(&nefergui.Input{
			Kind: nefergui.InputKey, Keysym: ev.Keysym, Pressed: ev.Pressed, Repeat: ev.Repeat,
			Modifiers: nefergui.Modifiers(ev.Modifiers & 0x0f), // shift, ctrl, alt, super
		})
	}
}

func (a *app) KeyboardFocus(id neferclient.SurfaceID, focused bool) {
	if s := a.screens[id]; s != nil && s.r != nil {
		kind := nefergui.InputFocusOut
		if focused {
			kind = nefergui.InputFocusIn
		}
		s.r.Input(&nefergui.Input{Kind: kind})
	}
}

// SecretChanged: the buffer changed; only its length is shown, on every screen.
func (a *app) SecretChanged(int) {
	for _, s := range a.screens {
		if s.r != nil {
			s.r.Invalidate()
		}
	}
}

// unlock ends the session lock. DEMO ONLY: nothing is verified here.
func (a *app) unlock() {
	if !a.locked || a.unlocked {
		return
	}
	a.secret.Wipe()
	if err := a.lock.Unlock(); err != nil {
		a.fail(fmt.Errorf("unlock: %w", err))
		return
	}
	a.unlocked = true
	fmt.Println("unlocked")
}

// setup creates the renderer once the surface is configured and its dmabuf
// feedback is complete.
func (s *screen) setup() error {
	fb := s.surf.Feedback()
	if s.r != nil || !s.configured || fb == nil {
		return nil
	}
	cfg := nefergui.RendererConfig{MainDevice: fb.MainDevice, Formats: make([]nefergui.Format, len(fb.Formats))}
	for i, f := range fb.Formats {
		cfg.Formats[i] = nefergui.Format{FourCC: f.FourCC, Modifier: f.Modifier}
	}
	r, err := nefergui.NewRenderer(cfg)
	if err != nil {
		return fmt.Errorf("renderer: %w", err)
	}
	s.r = r
	s.resize()
	return nil
}

func (s *screen) resize() {
	if s.r != nil {
		w, h, scale := s.surf.Size()
		s.r.Resize(int(w), int(h), scale)
	}
}

// draw renders when something changed and presents the frame.
func (s *screen) draw() (bool, error) {
	if s.r == nil || !s.canPresent {
		return false, nil
	}
	ok, err := s.r.Render(&s.out, s.model, view)
	if err != nil {
		return false, fmt.Errorf("render: %w", err)
	}
	if !ok {
		return false, nil
	}
	return true, s.present()
}

func cursorShape(c nefergui.Cursor) neferclient.CursorShape {
	switch c {
	case nefergui.CursorPointer:
		return neferclient.CursorPointer
	case nefergui.CursorText:
		return neferclient.CursorText
	case nefergui.CursorNotAllowed:
		return neferclient.CursorNotAllowed
	}
	return neferclient.CursorDefault
}

// watchID packs a surface and a buffer into the Conn.WatchFD id.
func (s *screen) watchID(buffer uint64) uint64 { return uint64(s.surf.ID())<<32 | buffer }

// present: Renderer.Render output → ImportBuffer, ImportTimeline, Present.
func (s *screen) present() error {
	out := &s.out
	for _, rt := range out.Retired { // buffers of an older size
		if err := s.conn.UnwatchFD(rt.ReleaseFD); err != nil {
			return fmt.Errorf("unwatch retired buffer %d: %w", rt.Buffer, err)
		}
		if err := s.surf.DestroyBuffer(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired buffer %d: %w", rt.Buffer, err)
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
		if err := s.surf.ImportBuffer(out.Buffer, &buf); err != nil {
			return fmt.Errorf("import buffer: %w", err)
		}
		// The release eventfd is stable per buffer: watch it once; FDReady
		// calls Renderer.Released.
		if err := s.conn.WatchFD(out.ReleaseFD, s.watchID(out.Buffer)); err != nil {
			return fmt.Errorf("watch release fd: %w", err)
		}
	}
	if out.NewTimelines {
		if !s.haveAcquire { // the acquire timeline is shared by every buffer
			if err := s.surf.ImportTimeline(out.Acquire.ID, out.Acquire.FD); err != nil {
				return fmt.Errorf("import acquire timeline: %w", err)
			}
			s.haveAcquire = true
		}
		if err := s.surf.ImportTimeline(out.Release.ID, out.Release.FD); err != nil {
			return fmt.Errorf("import release timeline: %w", err)
		}
	}
	s.damage = s.damage[:0]
	for _, d := range out.Damage {
		s.damage = append(s.damage, neferclient.Rect{X: d.X, Y: d.Y, Width: d.Width, Height: d.Height})
	}
	err := s.surf.Present(&neferclient.Present{
		Buffer:          out.Buffer,
		AcquireTimeline: out.Acquire.ID,
		ReleaseTimeline: out.Release.ID,
		AcquirePoint:    out.AcquirePoint,
		ReleasePoint:    out.ReleasePoint,
		Damage:          s.damage,
		Opaque:          true, // XRGB8888: RendererConfig.Transparent is false
	})
	if err != nil {
		return fmt.Errorf("present: %w", err)
	}
	s.canPresent = false // until Handler.Frame
	return nil
}

// close frees the renderer and the surface. The session lock itself is only
// ended by Lock.Unlock.
func (s *screen) close() error {
	var err error
	if s.r != nil {
		err = s.r.Close()
	}
	return errors.Join(err, s.surf.Close())
}
