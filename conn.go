package neferclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/wl"
	"golang.org/x/sys/unix"
)

// dialTimeout bounds the unix-socket dial; a local dial is normally instant.
const dialTimeout = 5 * time.Second

// maxOutputVersion is the wl_output version bound: 4 adds names.
const maxOutputVersion = 4

var (
	// ErrClosed is returned by calls on a closed Conn.
	ErrClosed = errors.New("neferclient: connection closed")
	// ErrGlobalNotFound is returned by [Conn.Bind] when the compositor does
	// not announce the interface. Test it with errors.Is.
	ErrGlobalNotFound = wlturbo.ErrGlobalNotFound
)

// Output describes one wl_output after its latest wl_output.done.
type Output struct {
	Global        uint32 // registry name; identifies the output for its lifetime
	Name          string // wl_output.name, empty before version 4
	Scale         int32  // integer scale, 1 until announced
	Width, Height int32  // current mode in physical pixels
}

// outputEntry holds the owner-side state of one bound wl_output. Properties
// accumulate in the pending fields and become visible at wl_output.done.
type outputEntry struct {
	proxy *wayland.Output
	done  bool // first wl_output.done applied; the output is public
	pub   Output

	nameBuf       [maxNameLen]byte
	nameLen       int
	scale         int32
	width, height int32
}

// Conn is one Wayland connection. Two goroutines run beside the owner: the
// reader, which only decodes wire events into a fixed queue, and an epoll
// waiter for [Conn.WatchFD]. Everything else — event handling, requests,
// Bind, Roundtrip, Outputs, Close — belongs to the owner goroutine, the one
// that calls [Conn.Dispatch].
type Conn struct {
	sock    net.Conn
	display *wl.Display
	wlctx   *wl.Context
	q       *queue

	readerStarted bool
	readerDone    chan struct{}
	pauseDepth    int

	epfd, efd  int
	epollStart bool
	epollDone  chan struct{}
	watched    map[int32]uint64 // fd -> caller id

	outputs []*outputEntry // sorted by Global
	view    []Output       // reused by Outputs

	ready       bool // Connect finished; announcements go to the Handler
	dispatching bool
	closed      bool
}

// Connect dials the compositor and completes the registry setup: every
// wl_output announced at that point is bound and its first description is
// available from [Conn.Outputs]. display names the socket relative to
// $XDG_RUNTIME_DIR, or is an absolute path; "" uses $WAYLAND_DISPLAY, then
// "wayland-0". Cancelling ctx abandons setup; afterwards ctx is not used.
func Connect(ctx context.Context, display string) (*Conn, error) {
	if ctx == nil {
		return nil, errors.New("neferclient: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := socketPath(display)
	if err != nil {
		return nil, err
	}
	sock, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Wayland: %w", err)
	}
	return connectConn(ctx, sock)
}

func socketPath(name string) (string, error) {
	if name == "" {
		name = os.Getenv("WAYLAND_DISPLAY")
		if name == "" {
			name = "wayland-0"
		}
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("neferclient: XDG_RUNTIME_DIR not set")
	}
	return filepath.Join(dir, name), nil
}

// connectConn runs the setup over an established connection and takes
// ownership of it.
func connectConn(ctx context.Context, sock net.Conn) (res *Conn, err error) {
	display, err := wlturbo.ConnectFromConn(sock)
	if err != nil {
		_ = sock.Close()
		return nil, err
	}
	c := &Conn{
		sock:    sock,
		display: display,
		wlctx:   display.Context(),
		q:       newQueue(true), // no reader yet: owner-side events are parked
		efd:     -1,
		epfd:    -1,
		watched: make(map[int32]uint64),
	}
	watch := context.AfterFunc(ctx, func() { _ = display.Close() })
	defer func() {
		if !watch() && err == nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = c.Close()
			res = nil
			if ctx.Err() != nil {
				err = errors.Join(ctx.Err(), err)
			}
		}
	}()
	if c.epfd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC); err != nil {
		return nil, fmt.Errorf("neferclient: epoll: %w", err)
	}
	if c.efd, err = unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK); err != nil {
		return nil, fmt.Errorf("neferclient: eventfd: %w", err)
	}
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(c.efd)}
	if err = unix.EpollCtl(c.epfd, unix.EPOLL_CTL_ADD, c.efd, &ev); err != nil {
		return nil, fmt.Errorf("neferclient: epoll: %w", err)
	}

	reg := display.Registry()
	reg.AddHandler(wayland.OutputInterface, func(_ *wl.Registry, name, version uint32) {
		e := event{kind: evGlobal, global: name, version: version}
		c.q.post(&e)
	})
	reg.AddGlobalRemoveHandler(removeHook{c.q})

	// With no reader running, the owner's own roundtrips park every event in
	// the queue's held list, in order; applying it here is the startup drain.
	if err = display.Roundtrip(); err != nil {
		return nil, err
	}
	if err = c.applyHeld(); err != nil {
		return nil, err
	}
	if err = display.Roundtrip(); err != nil { // wl_output properties follow the binds
		return nil, err
	}
	if err = c.applyHeld(); err != nil {
		return nil, err
	}

	c.ready = true
	c.readerDone = make(chan struct{})
	c.readerStarted = true
	c.epollDone = make(chan struct{})
	c.epollStart = true
	c.q.setPausing(false)
	go c.readLoop()
	go c.epollLoop()
	return c, nil
}

// removeHook forwards registry global_remove to the queue.
type removeHook struct{ q *queue }

func (h removeHook) HandleRegistryGlobalRemove(e wl.RegistryGlobalRemoveEvent) {
	ev := event{kind: evGlobalRemove, global: e.Name}
	h.q.post(&ev)
}

// setupHandler applies setup events without notifying anyone and keeps the
// first failure, which makes Connect fail.
type setupHandler struct {
	nopHandler
	err *error
}

func (h setupHandler) Error(err error) {
	if *h.err == nil {
		*h.err = err
	}
}

// applyHeld applies the events parked during setup.
func (c *Conn) applyHeld() error {
	var err error
	h := setupHandler{err: &err}
	held := c.q.takeHeld()
	for i := range held {
		c.apply(&held[i], h)
	}
	return err
}

// readLoop is the reader goroutine. It only calls Display.Dispatch; the
// generated handlers it triggers only push events into the queue.
func (c *Conn) readLoop() {
	defer close(c.readerDone)
	defer c.q.readerDone()
	for {
		if !c.q.gate() {
			return
		}
		err := c.display.Dispatch()
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			continue // a pause request interrupted the read
		}
		if !c.q.isStopped() {
			c.q.setFatal(err)
		}
		return
	}
}

// epollLoop is the second goroutine: it waits for watched descriptors and the
// shutdown eventfd.
func (c *Conn) epollLoop() {
	defer close(c.epollDone)
	var evs [8]unix.EpollEvent
	for {
		n, err := unix.EpollWait(c.epfd, evs[:], -1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			c.q.setFatal(fmt.Errorf("neferclient: epoll wait: %w", err))
			return
		}
		for i := range n {
			if evs[i].Fd == int32(c.efd) {
				return
			}
			e := event{kind: evFDReady, fd: evs[i].Fd}
			c.q.post(&e)
		}
	}
}

// pause parks the reader between two reads and returns once it is parked.
// Nested calls on the owner goroutine share the first pause. Before the reader
// starts, the queue already parks everything and pause does nothing.
func (c *Conn) pause() {
	if !c.readerStarted {
		return
	}
	c.pauseDepth++
	if c.pauseDepth > 1 {
		return
	}
	c.q.setPausing(true)
	_ = c.sock.SetReadDeadline(time.Unix(1, 0)) // wakes a blocked read
	c.q.awaitParked()
	_ = c.sock.SetReadDeadline(time.Time{})
}

func (c *Conn) resume() {
	if !c.readerStarted {
		return
	}
	c.pauseDepth--
	if c.pauseDepth == 0 {
		c.q.setPausing(false)
	}
}

// Wake returns a channel with capacity 1 that is signaled whenever events are
// queued: call [Conn.Dispatch] when it fires. A signal can be stale (the
// events were already drained), so Dispatch may find nothing to do.
func (c *Conn) Wake() <-chan struct{} { return c.q.wake }

// Context returns the wlturbo context that generated proxies are created on.
func (c *Conn) Context() *wl.Context { return c.wlctx }

// Display returns the underlying wlturbo display. Only the owner goroutine
// may call Dispatch or Roundtrip on it; use [Conn.Dispatch] and
// [Conn.Roundtrip] instead.
func (c *Conn) Display() *wl.Display { return c.display }

// Dispatch applies the events the reader queued and calls h for each one that
// matters to the application, on the calling goroutine. It handles at most one
// ring of events per call and signals [Conn.Wake] again if more are pending.
// A nil h discards the notifications but still applies the state changes. It
// returns the transport error once the queue is drained after the connection
// failed, and [ErrClosed] after Close.
func (c *Conn) Dispatch(h Handler) error {
	if c.closed {
		return ErrClosed
	}
	if c.dispatching {
		return errors.New("neferclient: Dispatch called from a Handler")
	}
	if h == nil {
		h = nopHandler{}
	}
	c.dispatching = true
	defer func() { c.dispatching = false }()
	var ev event
	for range ringSize {
		if !c.q.pop(&ev) {
			return c.q.fatalErr()
		}
		c.apply(&ev, h)
		if c.closed {
			return ErrClosed
		}
	}
	if c.q.pending() {
		c.q.signal()
		return nil
	}
	return c.q.fatalErr()
}

// apply runs on the owner goroutine.
func (c *Conn) apply(ev *event, h Handler) {
	switch ev.kind {
	case evGlobal:
		if err := c.bindOutput(ev.global, ev.version); err != nil {
			h.Error(err)
		}
	case evGlobalRemove:
		c.removeOutput(ev.global, h)
	case evOutputName:
		if o := c.entry(ev.global); o != nil {
			o.nameLen = copy(o.nameBuf[:], ev.nameBytes())
		}
	case evOutputScale:
		if o := c.entry(ev.global); o != nil && ev.a > 0 {
			o.scale = ev.a
		}
	case evOutputMode:
		if o := c.entry(ev.global); o != nil && ev.flags&1 != 0 { // current mode
			o.width, o.height = ev.a, ev.b
		}
	case evOutputDone:
		if o := c.entry(ev.global); o != nil {
			c.commitOutput(o, h)
		}
	case evFDReady:
		id, ok := c.watched[ev.fd]
		if !ok {
			return // unwatched while the event was queued
		}
		h.FDReady(id)
		if _, ok := c.watched[ev.fd]; ok { // the handler may have unwatched it
			if err := c.armFD(int(ev.fd), unix.EPOLL_CTL_MOD); err != nil {
				h.Error(err)
			}
		}
	}
}

func (c *Conn) entry(global uint32) *outputEntry {
	for _, o := range c.outputs {
		if o.pub.Global == global {
			return o
		}
	}
	return nil
}

func (c *Conn) bindOutput(global, version uint32) error {
	if version == 0 || c.entry(global) != nil {
		return nil
	}
	o := &outputEntry{pub: Output{Global: global, Scale: 1}, scale: 1}
	out := wayland.NewOutput(c.wlctx)
	q := c.q
	// Handlers go on before the bind request is sent, so no early event of the
	// new object is lost. They run on the reader goroutine and only queue.
	out.OnName(func(name string) {
		e := event{kind: evOutputName, global: global}
		e.setName(name)
		q.post(&e)
	})
	out.OnScale(func(factor int32) {
		e := event{kind: evOutputScale, global: global, a: factor}
		q.post(&e)
	})
	out.OnMode(func(flags uint32, width, height, _ int32) {
		e := event{kind: evOutputMode, global: global, flags: flags, a: width, b: height}
		q.post(&e)
	})
	out.OnDone(func() {
		e := event{kind: evOutputDone, global: global}
		q.post(&e)
	})
	if err := c.display.Registry().Bind(global, wayland.OutputInterface, min(version, maxOutputVersion), out); err != nil {
		c.wlctx.Unregister(out)
		return fmt.Errorf("neferclient: bind wl_output %d: %w", global, err)
	}
	o.proxy = out
	i := len(c.outputs)
	c.outputs = append(c.outputs, o)
	for ; i > 0 && c.outputs[i-1].pub.Global > global; i-- {
		c.outputs[i], c.outputs[i-1] = c.outputs[i-1], c.outputs[i]
	}
	return nil
}

func (c *Conn) commitOutput(o *outputEntry, h Handler) {
	if string(o.nameBuf[:o.nameLen]) != o.pub.Name {
		o.pub.Name = string(o.nameBuf[:o.nameLen])
	}
	o.pub.Scale, o.pub.Width, o.pub.Height = o.scale, o.width, o.height
	if !o.done {
		o.done = true
		if c.ready {
			h.OutputAdded(&o.pub)
		}
	}
}

func (c *Conn) removeOutput(global uint32, h Handler) {
	for i, o := range c.outputs {
		if o.pub.Global != global {
			continue
		}
		if err := o.proxy.Release(); err != nil {
			c.wlctx.Unregister(o.proxy) // version 1 and 2 have no release
		}
		c.outputs = append(c.outputs[:i], c.outputs[i+1:]...)
		if o.done {
			h.OutputRemoved(global)
		}
		return
	}
}

// Outputs returns the outputs that finished their first description, ordered
// by Global. The slice is reused by the next call; owner goroutine only.
func (c *Conn) Outputs() []Output {
	c.view = c.view[:0]
	for _, o := range c.outputs {
		if o.done {
			c.view = append(c.view, o.pub)
		}
	}
	return c.view
}

// Roundtrip sends wl_display.sync and dispatches until it returns. The reader
// is paused meanwhile; the events it handled are queued for [Conn.Dispatch].
func (c *Conn) Roundtrip() error {
	if c.closed {
		return ErrClosed
	}
	c.pause()
	defer c.resume()
	return c.display.Roundtrip()
}

// Bind binds the global named iface at min(announced, version) and returns the
// proxy and the negotiated version. newProxy creates the proxy on the given
// context; register its event handlers inside newProxy: the reader is paused
// until Bind returns, but the bound object's first events can arrive right
// after, so a handler added later may miss them. A missing global yields an
// error matching [ErrGlobalNotFound].
func (c *Conn) Bind[P wl.Proxy](iface string, version uint32, newProxy func(*wl.Context) P) (P, uint32, error) {
	var zero P
	if c.closed {
		return zero, 0, ErrClosed
	}
	c.pause()
	defer c.resume()
	p := newProxy(c.wlctx)
	v, err := c.display.Registry().BindNegotiated(iface, version, p)
	if err != nil {
		c.wlctx.Unregister(p)
		return zero, 0, err
	}
	return p, v, nil
}

// WatchFD reports readability of fd through Handler.FDReady(id). The watch is
// one-shot: it is re-armed after each FDReady call returns. fd stays owned by
// the caller and must be unwatched before it is closed.
func (c *Conn) WatchFD(fd int, id uint64) error {
	if c.closed {
		return ErrClosed
	}
	if _, ok := c.watched[int32(fd)]; ok {
		return fmt.Errorf("neferclient: fd %d already watched", fd)
	}
	if err := c.armFD(fd, unix.EPOLL_CTL_ADD); err != nil {
		return err
	}
	c.watched[int32(fd)] = id
	return nil
}

// UnwatchFD stops watching fd. A notification already queued is dropped.
func (c *Conn) UnwatchFD(fd int) error {
	if c.closed {
		return ErrClosed
	}
	if _, ok := c.watched[int32(fd)]; !ok {
		return fmt.Errorf("neferclient: fd %d not watched", fd)
	}
	delete(c.watched, int32(fd))
	if err := unix.EpollCtl(c.epfd, unix.EPOLL_CTL_DEL, fd, nil); err != nil {
		return fmt.Errorf("neferclient: unwatch fd %d: %w", fd, err)
	}
	return nil
}

func (c *Conn) armFD(fd, op int) error {
	ev := unix.EpollEvent{Events: unix.EPOLLIN | unix.EPOLLONESHOT, Fd: int32(fd)}
	if err := unix.EpollCtl(c.epfd, op, fd, &ev); err != nil {
		return fmt.Errorf("neferclient: watch fd %d: %w", fd, err)
	}
	return nil
}

// Close closes the connection and stops both goroutines. It never requests an
// unlock or any other protocol shutdown; those are explicit calls. It is safe
// to call more than once.
func (c *Conn) Close() error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	c.q.stop()
	err := c.display.Close()
	if c.readerStarted {
		<-c.readerDone
	}
	if c.epollStart {
		var one = [8]byte{1}
		_, _ = unix.Write(c.efd, one[:])
		<-c.epollDone
	}
	if c.epfd >= 0 {
		_ = unix.Close(c.epfd)
	}
	if c.efd >= 0 {
		_ = unix.Close(c.efd)
	}
	return err
}
