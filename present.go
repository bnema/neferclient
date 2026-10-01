package neferclient

import (
	"errors"
	"fmt"

	"github.com/bnema/wlturbo/wl"
	"golang.org/x/sys/unix"
)

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
func (s *Surface) ImportBuffer(id uint64, b *Buffer) error {
	if s.closed {
		return ErrClosed
	}
	if _, ok := s.buffers[id]; ok {
		return fmt.Errorf("neferclient: buffer %d already imported", id)
	}
	if b == nil || b.Width <= 0 || b.Height <= 0 || b.PlaneCount < 1 || b.PlaneCount > len(b.Planes) {
		return errors.New("neferclient: invalid buffer description")
	}
	var dups [4]int
	sent := 0
	defer func() {
		for _, fd := range dups[sent:b.PlaneCount] { // not consumed by a request
			if fd > 0 {
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
	defer params.Destroy()
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
	s.buffers[id] = buf
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
	return buf.Destroy()
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
	acq, ok := s.timelines[p.AcquireTimeline]
	if !ok {
		return fmt.Errorf("neferclient: unknown timeline %d", p.AcquireTimeline)
	}
	rel, ok := s.timelines[p.ReleaseTimeline]
	if !ok {
		return fmt.Errorf("neferclient: unknown timeline %d", p.ReleaseTimeline)
	}
	pw, ph, err := s.PhysicalSize()
	if err != nil {
		return err
	}
	if s.viewport != nil {
		if s.sentDestW != s.width || s.sentDestH != s.height {
			if err = s.viewport.SetDestination(s.width, s.height); err != nil {
				return err
			}
			s.sentDestW, s.sentDestH = s.width, s.height
		}
	} else if s.scale > 1 && s.sentBufScale != int32(s.scale) {
		if err = s.surf.SetBufferScale(int32(s.scale)); err != nil {
			return err
		}
		s.sentBufScale = int32(s.scale)
	}
	if err = s.setOpaque(p.Opaque); err != nil {
		return err
	}
	if err = s.sync.SetAcquirePoint(acq, uint32(p.AcquirePoint>>32), uint32(p.AcquirePoint)); err != nil {
		return err
	}
	if err = s.sync.SetReleasePoint(rel, uint32(p.ReleasePoint>>32), uint32(p.ReleasePoint)); err != nil {
		return err
	}
	if err = s.surf.Attach(buf, 0, 0); err != nil {
		return err
	}
	if len(p.Damage) == 0 {
		err = s.surf.DamageBuffer(0, 0, pw, ph)
	}
	for _, r := range p.Damage {
		if err != nil {
			break
		}
		err = s.surf.DamageBuffer(r.X, r.Y, r.Width, r.Height)
	}
	if err != nil {
		return err
	}
	// The one frame callback object is reused: only one is ever pending.
	if err = s.c.wlctx.RequestArgs(wl.Request{Proxy: s.surf, Opcode: 3, Name: "wl_surface.frame", Child: s.cb}, wl.ArgObject(s.cb)); err != nil {
		return err
	}
	if err = s.surf.Commit(); err != nil {
		return err
	}
	s.frameReady = false
	return nil
}

// setOpaque keeps the opaque region equal to the full logical surface when
// opaque, and empty otherwise, sending requests only on change.
func (s *Surface) setOpaque(opaque bool) error {
	if !opaque {
		if s.opaqueSet {
			s.opaqueSet = false
			return s.surf.SetOpaqueRegion(nil)
		}
		return nil
	}
	if s.opaqueSet && s.opaqueW == s.width && s.opaqueH == s.height {
		return nil
	}
	region, err := s.c.g.compositor.CreateRegion()
	if err != nil {
		return err
	}
	if err = region.Add(0, 0, s.width, s.height); err == nil {
		err = s.surf.SetOpaqueRegion(region)
	}
	_ = region.Destroy()
	if err != nil {
		return err
	}
	s.opaqueSet, s.opaqueW, s.opaqueH = true, s.width, s.height
	return nil
}
