package neferclient

// Handler receives the events [Conn.Dispatch] drains. Every method is called
// on the goroutine that called Dispatch, never on the reader goroutine, so an
// implementation may send requests and call [Conn.Bind], [Conn.Roundtrip],
// [Conn.WatchFD] and [Conn.UnwatchFD] from inside any method. It must not call
// Dispatch.
type Handler interface {
	// OutputAdded reports an output that became complete after [Connect]
	// returned: the compositor sent its first wl_output.done. Outputs that
	// existed during Connect are not reported; read them with [Conn.Outputs].
	// Later property changes are applied to [Conn.Outputs] silently. out is
	// valid only during the call.
	OutputAdded(out *Output)
	// OutputRemoved reports that the wl_output global went away. The output is
	// already gone from [Conn.Outputs].
	OutputRemoved(global uint32)
	// FDReady reports that a descriptor registered with [Conn.WatchFD] is
	// readable. The watch is re-armed when the call returns. It can be
	// spurious: drain the descriptor without blocking.
	FDReady(id uint64)
	// Error reports a non-fatal failure while handling an event, for example
	// a wl_output that could not be bound. Fatal transport errors are returned
	// by Dispatch instead.
	Error(err error)

	// Configure reports the logical size of a surface after the compositor's
	// configure was acknowledged (the library acks internally). The first
	// call makes the surface presentable. For layer surfaces an axis the
	// compositor leaves unchanged keeps its previous size.
	Configure(id SurfaceID, width, height int32)
	// Scale reports a changed preferred scale: fractional (n/120) when the
	// compositor supports wp_fractional_scale, else the integer buffer scale.
	// Re-read [Surface.PhysicalSize] and render again.
	Scale(id SurfaceID, scale float64)
	// Frame reports that the frame callback requested by the last
	// [Surface.Present] fired; the next Present is allowed.
	Frame(id SurfaceID)
	// Closed reports that the compositor closed the surface (toplevel close,
	// layer surface closed). Call [Surface.Close]; nothing else happens
	// automatically.
	Closed(id SurfaceID)
	// FeedbackDone reports that a complete linux-dmabuf feedback round was
	// applied; [Surface.Feedback] holds the new main device and formats. It
	// is sent again whenever the compositor changes its preference.
	FeedbackDone(id SurfaceID)
	// Locked reports ext_session_lock_v1.locked for the live [Lock]: every
	// output is protected. Only now may [Lock.Unlock] be called.
	Locked()
	// LockFinished reports ext_session_lock_v1.finished: the compositor
	// refused or ended the lock. Destroy it with [Lock.Close].
	LockFinished()
}

// NopHandler implements every Handler method as a no-op. Embed it in a handler
// and override only the methods you need; methods added to Handler in later
// versions get their no-op here, so embedding keeps the type compiling.
type NopHandler struct{}

func (NopHandler) OutputAdded(*Output)  {}
func (NopHandler) OutputRemoved(uint32) {}
func (NopHandler) FDReady(uint64)       {}
func (NopHandler) Error(error)          {}

func (NopHandler) Configure(SurfaceID, int32, int32) {}
func (NopHandler) Scale(SurfaceID, float64)          {}
func (NopHandler) Frame(SurfaceID)                   {}
func (NopHandler) Closed(SurfaceID)                  {}
func (NopHandler) FeedbackDone(SurfaceID)            {}
func (NopHandler) Locked()                           {}
func (NopHandler) LockFinished()                     {}
