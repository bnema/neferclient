# neferclient

A Wayland client toolkit library for Go: connection, outputs, surface roles, seat and DMA-BUF presentation. It does not render and holds no application policy. No cgo required.

> [!WARNING]
> **Early development.** No release yet. Expect bugs and breaking changes.

## Requirements

- Linux, kernel 6.6 or newer (DRM syncobj eventfd).
- A compositor advertising `zwp_linux_dmabuf_v1` (v4) and `wp_linux_drm_syncobj_manager_v1`.
- `libxkbcommon` at runtime for keyboard input.

## Usage

The library does not render. The `examples` module pairs it with [NeferGUI](https://github.com/bnema/nefergui) (a pure graphics library with no Wayland code); the two never import each other, and the examples copy plain fields between them. The loop runs on one owner goroutine:

```go
conn, err := neferclient.Connect(ctx, "")
if err != nil {
	return fmt.Errorf("connect: %w", err)
}
defer conn.Close()
surf, err := conn.NewLayerSurface(neferclient.LayerConfig{Level: neferclient.LayerTop, Width: 240, Height: 90})
if err != nil {
	return fmt.Errorf("layer surface: %w", err)
}
for {
	select {
	case <-conn.Wake():
		if err := conn.Dispatch(handler); err != nil { // Configure, FeedbackDone, Frame, Pointer, Key, FDReady...
			return fmt.Errorf("dispatch: %w", err)
		}
	case <-ctx.Done():
		return nil
	}
	// render with NeferGUI, then surf.ImportBuffer / surf.ImportTimeline / surf.Present
}
```

### Popups

`Conn.NewPopup` creates an `xdg_popup` on a layer surface, a toplevel or another popup: a menu or tooltip placed relative to an anchor rectangle of its parent. With `Grab` the popup takes the keyboard and the compositor dismisses it on a click outside; the grab needs the `PointerEvent.Serial` of the button press that opened it. The compositor can dismiss a popup at any time, so a handler must implement `PopupDone` (embedded `NopHandler` ignores it) and close the popup; close popups before their parent.

```go
func (h *handler) Pointer(ev *neferclient.PointerEvent) {
	if ev.Kind != neferclient.PointerButton || !ev.Pressed {
		return
	}
	popup, err := h.conn.NewPopup(neferclient.PopupConfig{
		Parent: h.bar, // a layer surface
		Place: neferclient.PopupPlacement{
			Anchor: neferclient.Rect{X: 100, Y: 0, Width: 24, Height: 30}, // on the parent, logical px
			Edge:   neferclient.EdgeBottom, Gravity: neferclient.EdgeBottomRight,
			Adjust: neferclient.AdjustSlideX | neferclient.AdjustFlipY,
			Width:  200, Height: 120,
		},
		Grab: true, Serial: ev.Serial,
	})
	if err != nil {
		h.err = err
		return
	}
	h.popup = popup // presentable after Configure, like any surface
}

func (h *handler) PopupDone(id neferclient.SurfaceID) {
	if h.popup != nil && h.popup.ID() == id {
		h.err = h.popup.Close()
		h.popup = nil
	}
}
```

`Surface.Reposition` moves a mapped popup (`xdg_wm_base` version 3).

- [`examples/layer`](examples/layer): a layer surface with a counter button.
- [`examples/lock`](examples/lock): a session lock with a masked password field. It is a demo and authenticates nothing; read its header before running it.

Run them against a compositor with `cd examples && go run ./layer`. `make examples-check` builds, vets and lints them; the headless test needs a NeferWL binary: `cd examples && NEFERCLIENT_HEADLESS=/usr/sbin/neferwl go test -run Headless ./...`.

## License

MIT
