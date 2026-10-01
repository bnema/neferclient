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
}

// nopHandler discards every event. Connect uses it to apply the events queued
// during setup, and Dispatch uses it for a nil Handler.
type nopHandler struct{}

func (nopHandler) OutputAdded(*Output)  {}
func (nopHandler) OutputRemoved(uint32) {}
func (nopHandler) FDReady(uint64)       {}
func (nopHandler) Error(error)          {}
