# neferclient

A Wayland client toolkit library for Go: connection, outputs, surface roles, seat and DMA-BUF presentation. It does not render and holds no application policy. No cgo required.

> [!WARNING]
> **Early development.** No release yet. Expect bugs and breaking changes.

## Requirements

- Linux, kernel 6.6 or newer (DRM syncobj eventfd).
- A compositor advertising `zwp_linux_dmabuf_v1` (v4) and `wp_linux_drm_syncobj_manager_v1`.
- `libxkbcommon` at runtime for keyboard input.

## License

MIT
