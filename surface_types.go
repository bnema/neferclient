package neferclient

import "slices"

// SurfaceID identifies a [Surface] in Handler callbacks. IDs are never
// reused within a connection.
type SurfaceID uint32

// Format is one DRM format and modifier pair from linux-dmabuf feedback.
type Format struct {
	FourCC   uint32
	Modifier uint64
}

// Feedback is the latest complete linux-dmabuf feedback of a surface.
type Feedback struct {
	MainDevice uint64   // dev_t of the compositor's main device
	Formats    []Format // every tranche's formats, flattened in tranche order
}

// Equal reports whether f and o have the same main device and the same formats
// in the same order. Feedback flattens tranches, so a change limited to a
// tranche's target device or scanout flag is not visible here. A nil Feedback
// equals only nil. The compositor sends a complete round again whenever it
// re-evaluates its preference, often unchanged, so compare against the
// feedback a renderer was built from before rebuilding it.
func (f *Feedback) Equal(o *Feedback) bool {
	if f == nil || o == nil {
		return f == o
	}
	return f.MainDevice == o.MainDevice && slices.Equal(f.Formats, o.Formats)
}

// Clone returns a deep copy that stays valid after later rounds. The value from
// [Surface.Feedback] shares memory the library reuses, so keep a Clone of the
// feedback a renderer was built from. A nil Feedback clones to nil.
func (f *Feedback) Clone() *Feedback {
	if f == nil {
		return nil
	}
	return &Feedback{MainDevice: f.MainDevice, Formats: slices.Clone(f.Formats)}
}

// Rect is an integer rectangle: surface-local logical pixels for input
// regions, buffer pixels for damage.
type Rect struct{ X, Y, Width, Height int32 }
