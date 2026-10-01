package neferclient

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

// Rect is an integer rectangle: surface-local logical pixels for input
// regions, buffer pixels for damage.
type Rect struct{ X, Y, Width, Height int32 }
