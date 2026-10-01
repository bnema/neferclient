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
conn, _ := neferclient.Connect(ctx, "")
surf, _ := conn.NewLayerSurface(neferclient.LayerConfig{Level: neferclient.LayerTop, Width: 240, Height: 90})
for {
	select {
	case <-conn.Wake():
		conn.Dispatch(handler) // Configure, FeedbackDone, Frame, Pointer, Key, FDReady...
	}
	// render with NeferGUI, then:
	// surf.ImportBuffer / surf.ImportTimeline / surf.Present
}
```

- [`examples/layer`](examples/layer): a layer surface with a counter button.
- [`examples/lock`](examples/lock): a session lock with a masked password field. It is a demo and authenticates nothing; read its header before running it.

Run them against a compositor with `cd examples && go run ./layer`. `make examples-check` builds and vets them; the headless test needs a NeferWL binary: `NEFERCLIENT_HEADLESS=/usr/sbin/neferwl go test ./... -run Headless`.

## License

MIT
