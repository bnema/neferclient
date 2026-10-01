package neferclient

import (
	"errors"
	"fmt"

	"github.com/bnema/go-wayland-bindings/client/extsessionlock"
	"github.com/bnema/go-wayland-bindings/client/fractionalscale"
	"github.com/bnema/go-wayland-bindings/client/linuxdmabuf"
	"github.com/bnema/go-wayland-bindings/client/linuxdrmsyncobj"
	"github.com/bnema/go-wayland-bindings/client/viewporter"
	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/go-wayland-bindings/client/wlrlayershell"
	"github.com/bnema/go-wayland-bindings/client/xdgshell"
	"github.com/bnema/wlturbo/wl"
)

// CapabilityError reports a missing or too old required protocol global.
type CapabilityError struct {
	Name  string
	Cause error
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("neferclient: capability %s: %v", e.Name, e.Cause)
}
func (e *CapabilityError) Unwrap() error { return e.Cause }

// globals holds the protocol globals bound on first use. Owner goroutine only.
type globals struct {
	compositor *wayland.Compositor
	dmabuf     *linuxdmabuf.LinuxDmabuf
	syncobj    *linuxdrmsyncobj.WpLinuxDrmSyncobjManager
	xdg        *xdgshell.XdgWmBase
	layer      *wlrlayershell.LayerShell
	lockMgr    *extsessionlock.ExtSessionLockManager
	viewporter *viewporter.WpViewporter
	fscale     *fractionalscale.WpFractionalScaleManager

	layerVersion  uint32
	triedViewport bool
	triedFScale   bool
}

// require binds iface at min(announced, supported) and fails with a
// *CapabilityError when it is absent or older than minimum.
func (c *Conn) require[P wl.Proxy](iface string, supported, minimum uint32, newProxy func(*wl.Context) P) (P, uint32, error) {
	p, v, err := c.Bind(iface, supported, newProxy)
	if err != nil {
		var zero P
		return zero, 0, &CapabilityError{Name: iface, Cause: err}
	}
	if v < minimum {
		_ = c.wlctx // the proxy stays bound; it is unusable below minimum
		var zero P
		return zero, 0, &CapabilityError{Name: iface, Cause: fmt.Errorf("requires version %d, negotiated %d", minimum, v)}
	}
	return p, v, nil
}

// ensureCore binds wl_compositor, linux-dmabuf (v4) and drm-syncobj, and the
// optional viewporter and fractional-scale managers.
func (c *Conn) ensureCore() error {
	g := &c.g
	var err error
	if g.compositor == nil {
		if g.compositor, _, err = c.require(wayland.CompositorInterface, 6, 4, wayland.NewCompositor); err != nil {
			return err
		}
	}
	if g.dmabuf == nil {
		if g.dmabuf, _, err = c.require(linuxdmabuf.LinuxDmabufInterface, 4, 4, linuxdmabuf.NewLinuxDmabuf); err != nil {
			return err
		}
	}
	if g.syncobj == nil {
		if g.syncobj, _, err = c.require(linuxdrmsyncobj.WpLinuxDrmSyncobjManagerInterface, 1, 1, linuxdrmsyncobj.NewWpLinuxDrmSyncobjManager); err != nil {
			return err
		}
	}
	if !g.triedViewport {
		g.triedViewport = true
		v, _, e := c.Bind(viewporter.WpViewporterInterface, 1, viewporter.NewWpViewporter)
		if e != nil && !errors.Is(e, ErrGlobalNotFound) {
			return e
		}
		g.viewporter = v
	}
	if !g.triedFScale {
		g.triedFScale = true
		v, _, e := c.Bind(fractionalscale.WpFractionalScaleManagerInterface, 1, fractionalscale.NewWpFractionalScaleManager)
		if e != nil && !errors.Is(e, ErrGlobalNotFound) {
			return e
		}
		g.fscale = v
	}
	return nil
}

func (c *Conn) ensureXdg() error {
	if c.g.xdg != nil {
		return nil
	}
	q := c.q
	x, _, err := c.require(xdgshell.XdgWmBaseInterface, 6, 1, func(ctx *wl.Context) *xdgshell.XdgWmBase {
		x := xdgshell.NewXdgWmBase(ctx)
		x.OnPing(func(serial uint32) {
			e := event{kind: evPing, serial: serial}
			q.post(&e)
		})
		return x
	})
	c.g.xdg = x
	return err
}

func (c *Conn) ensureLayerShell() error {
	if c.g.layer != nil {
		return nil
	}
	l, v, err := c.require(wlrlayershell.LayerShellInterface, 4, 1, wlrlayershell.NewLayerShell)
	c.g.layer, c.g.layerVersion = l, v
	return err
}

func (c *Conn) ensureLockManager() error {
	if c.g.lockMgr != nil {
		return nil
	}
	m, _, err := c.require(extsessionlock.ExtSessionLockManagerInterface, 1, 1, extsessionlock.NewExtSessionLockManager)
	c.g.lockMgr = m
	return err
}
