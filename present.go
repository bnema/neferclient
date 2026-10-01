package neferclient

import (
	"errors"
	"fmt"

	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/wlturbo/wl"
	"golang.org/x/sys/unix"
)

// importedBuffer is a wl_buffer with the size it was imported at.
type importedBuffer struct {
	buf  *wayland.Buffer
	w, h int32
}

// Plane is one DMA-BUF plane of a [Buffer]. FD stays owned by the caller.
type Plane struct {
	FD     int
	Offset uint32
	Stride uint32
}

// Buffer describes a DMA-BUF to import as a wl_buffer.
type Buffer struct {
	Width, Height int32 // physical pixels
	FourCC        uint32
	Modifier      uint64
	Planes        [4]Plane
	PlaneCount    int
}

// Present describes one commit: which imported buffer, the explicit-sync
// timelines and points, and the damage in buffer pixels.
type Present struct {
	Buffer          uint64
	AcquireTimeline uint64
	ReleaseTimeline uint64
	AcquirePoint    uint64
	ReleasePoint    uint64
	Damage          []Rect // nil or empty damages the whole buffer
	Opaque          bool   // the whole surface is opaque
}

// ImportBuffer creates a wl_buffer from b under the caller's id. The plane
// descriptors are duplicated internally; the caller keeps ownership of its own.
func (s *Surface) ImportBuffer(id uint64, b *Buffer) (err error) {
	if s.closed {
		return ErrClosed
	}
	if _, ok := s.buffers[id]; ok {
		return fmt.Errorf("neferclient: buffer %d already imported", id)
	}
	if b == nil || b.Width <= 0 || b.Height <= 0 || b.PlaneCount < 1 || b.PlaneCount > len(b.Planes) {
		return errors.New("neferclient: invalid buffer description")
	}
	dups := [4]int{-1, -1, -1, -1}
	sent := 0
	defer func() {
		for _, fd := range dups[sent:b.PlaneCount] { // not consumed by a request
			if fd >= 0 {
				_ = unix.Close(fd)
			}
		}
	}()
	for i := range b.PlaneCount {
		fd, err := unix.FcntlInt(uintptr(b.Planes[i].FD), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("neferclient: dup plane %d: %w", i, err)
		}
		dups[i] = fd
	}
	params, err := s.c.g.dmabuf.CreateParams()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, params.Destroy()) }()
	hi, lo := uint32(b.Modifier>>32), uint32(b.Modifier)
	for i := range b.PlaneCount {
		// On success the request closes the duplicate; on failure it stays ours.
		if err = params.Add(dups[i], uint32(i), b.Planes[i].Offset, b.Planes[i].Stride, hi, lo); err != nil {
			return err
		}
		sent++
	}
	buf, err := params.CreateImmed(b.Width, b.Height, b.FourCC, 0)
	if err != nil {
		return err
	}
	s.buffers[id] = importedBuffer{buf: buf, w: b.Width, h: b.Height}
	return nil
}

// DestroyBuffer destroys an imported wl_buffer. The compositor may still read
// it until its release point signals; see the explicit-sync rules.
func (s *Surface) DestroyBuffer(id uint64) error {
	buf, ok := s.buffers[id]
	if !ok {
		return fmt.Errorf("neferclient: unknown buffer %d", id)
	}
	delete(s.buffers, id)
	return buf.buf.Destroy()
}

// ImportTimeline imports a DRM syncobj timeline. fd is duplicated internally.
func (s *Surface) ImportTimeline(id uint64, fd int) error {
	if s.closed {
		return ErrClosed
	}
	if _, ok := s.timelines[id]; ok {
		return fmt.Errorf("neferclient: timeline %d already imported", id)
	}
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("neferclient: dup timeline: %w", err)
	}
	t, err := s.c.g.syncobj.ImportTimeline(dup)
	if err != nil {
		_ = unix.Close(dup) // not consumed on error
		return err
	}
	s.timelines[id] = t
	return nil
}

// Present commits an imported buffer. It refuses before the first configure,
// before the first complete dmabuf feedback and while the previous frame
// callback is pending.
func (s *Surface) Present(p *Present) error {
	if s.closed {
		return ErrClosed
	}
	if p == nil {
		return errors.New("neferclient: nil present")
	}
	if !s.configured {
		return errors.New("neferclient: present before configure")
	}
	if !s.fbDone {
		return errors.New("neferclient: dmabuf feedback incomplete")
	}
	if !s.frameReady {
		return errors.New("neferclient: frame callback pending")
	}
	buf, ok := s.buffers[p.Buffer]
	if !ok {
		return fmt.Errorf("neferclient: unknown buffer %d", p.Buffer)
	}
	if p.AcquireTimeline == p.ReleaseTimeline && p.AcquirePoint >= p.ReleasePoint {
		return fmt.Errorf("neferclient: acquire point %d must precede release point %d on one timeline", p.AcquirePoint, p.ReleasePoint)
	}
	for _, id := range [...]uint64{p.AcquireTimeline, p.ReleaseTimeline} {
		if _, ok := s.timelines[id]; !ok {
			return fmt.Errorf("neferclient: unknown timeline %d", id)
		}
	}
	pw, ph, err := s.PhysicalSize()
	if err != nil {
		return err
	}
	// Without a viewport (and always on lock surfaces) the compositor
	// requires the buffer to match the surface exactly.
	if (s.viewport == nil || s.role == roleLock) && (buf.w != pw || buf.h != ph) {
		return fmt.Errorf("neferclient: buffer %dx%d does not match surface %dx%d", buf.w, buf.h, pw, ph)
	}
	r := s.req
	if s.viewport != nil {
		if s.sentDestW != s.width || s.sentDestH != s.height {
			if err = r.setDestination(s.width, s.height); err != nil {
				return err
			}
			s.sentDestW, s.sentDestH = s.width, s.height
		}
	} else if s.sentBufScale != int32(s.scale) {
		if err = r.setBufferScale(int32(s.scale)); err != nil {
			return err
		}
		s.sentBufScale = int32(s.scale)
	}
	if p.Opaque {
		if !s.opaqueSet || s.opaqueW != s.width || s.opaqueH != s.height {
			if err = r.setOpaqueRegion(s.width, s.height, true); err != nil {
				return err
			}
			s.opaqueSet, s.opaqueW, s.opaqueH = true, s.width, s.height
		}
	} else if s.opaqueSet {
		if err = r.setOpaqueRegion(0, 0, false); err != nil {
			return err
		}
		s.opaqueSet = false
	}
	if err = r.setAcquire(p.AcquireTimeline, p.AcquirePoint); err != nil {
		return err
	}
	if err = r.setRelease(p.ReleaseTimeline, p.ReleasePoint); err != nil {
		return err
	}
	if err = r.attach(p.Buffer); err != nil {
		return err
	}
	if len(p.Damage) == 0 {
		err = r.damageBuffer(Rect{0, 0, pw, ph})
	}
	for _, d := range p.Damage {
		if err != nil {
			break
		}
		err = r.damageBuffer(d)
	}
	if err != nil {
		return err
	}
	if err = r.frame(); err != nil {
		return err
	}
	if err = r.commit(); err != nil {
		return err
	}
	s.frameReady, s.presented = false, true
	return nil
}

// surfaceRequests is the seam between the Present ordering rules and the
// wire: the production implementation sends the protocol requests.
type surfaceRequests interface {
	setDestination(w, h int32) error
	setBufferScale(n int32) error
	setOpaqueRegion(w, h int32, opaque bool) error
	setAcquire(timeline, point uint64) error
	setRelease(timeline, point uint64) error
	attach(buffer uint64) error
	damageBuffer(r Rect) error
	frame() error
	commit() error
}

// wireRequests sends the requests. skipFrame is set by tests that isolate the
// request path from the frame callback registration.
type wireRequests struct {
	s         *Surface
	skipFrame bool
}

func (r wireRequests) setDestination(w, h int32) error { return r.s.viewport.SetDestination(w, h) }
func (r wireRequests) setBufferScale(n int32) error    { return r.s.surf.SetBufferScale(n) }

func (r wireRequests) setOpaqueRegion(w, h int32, opaque bool) error {
	s := r.s
	if !opaque {
		return s.surf.SetOpaqueRegion(nil)
	}
	region, err := s.c.g.compositor.CreateRegion()
	if err != nil {
		return err
	}
	if err = region.Add(0, 0, w, h); err == nil {
		err = s.surf.SetOpaqueRegion(region)
	}
	_ = region.Destroy()
	return err
}

func (r wireRequests) setAcquire(timeline, point uint64) error {
	return r.s.sync.SetAcquirePoint(r.s.timelines[timeline], uint32(point>>32), uint32(point))
}

func (r wireRequests) setRelease(timeline, point uint64) error {
	return r.s.sync.SetReleasePoint(r.s.timelines[timeline], uint32(point>>32), uint32(point))
}

func (r wireRequests) attach(buffer uint64) error {
	return r.s.surf.Attach(r.s.buffers[buffer].buf, 0, 0)
}

func (r wireRequests) damageBuffer(d Rect) error {
	return r.s.surf.DamageBuffer(d.X, d.Y, d.Width, d.Height)
}

// frame requests the frame callback on the surface's single reusable
// callback object: only one is ever pending.
func (r wireRequests) frame() error {
	s := r.s
	if r.skipFrame {
		return nil
	}
	return s.c.wlctx.RequestArgs(wl.Request{Proxy: s.surf, Opcode: 3, Name: "wl_surface.frame", Child: s.cb}, wl.ArgObject(s.cb))
}

func (r wireRequests) commit() error { return r.s.surf.Commit() }
