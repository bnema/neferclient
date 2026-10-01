// Package neferclient is a Wayland client toolkit library for Go: connection,
// outputs, surface roles, seat and DMA-BUF presentation. It does not render
// and holds no application policy.
//
// # Rebuilding after a dmabuf feedback change
//
// The compositor may change its linux-dmabuf preference at any time (another
// main device or format table, for example when a surface moves to a GPU on
// another output). Every complete round calls [Handler.FeedbackDone], also when
// nothing changed. A renderer built from the first feedback must then be
// rebuilt from the new one. On the owner goroutine:
//
//  1. Keep a [Feedback.Clone] of the feedback each renderer was built from. In
//     FeedbackDone, return when [Feedback.Equal] says the preference is the
//     one in use.
//  2. Stop watching the renderer's descriptors, call [Surface.DestroyImports]
//     (a frame may be pending: that is allowed), then close the old renderer.
//  3. Build the new renderer from [Surface.Feedback] and import its buffers and
//     timelines on demand, as for the first one. Ids may be reused.
//  4. Present again when [Handler.Frame] reports the pending frame; Present
//     refuses until then, with the old renderer or the new one.
//
// Destroying the wl_buffer and timeline objects never withdraws a commit the
// compositor already holds, and the duplicated descriptors are owned by the
// requests, so rebuilding cannot touch a destroyed object or close a
// descriptor twice. The compositor holds its own references to the DMA-BUFs
// and timelines it received, so closing the old renderer at once is safe; what
// the explicit-sync rule forbids is writing to or recycling storage before its
// release point signals.
package neferclient
