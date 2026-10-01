package neferclient_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/stretchr/testify/require"

	"github.com/bnema/neferclient"
)

// headlessEnvDrop lists the environment prefixes the child compositor must not
// inherit: anything that could attach it to, or make it act on, the caller's
// session.
var headlessEnvDrop = []string{"DISPLAY=", "WAYLAND_DISPLAY=", "WAYLAND_SOCKET=", "NOTIFY_SOCKET=", "DBUS_SESSION_BUS_ADDRESS=",
	"XDG_RUNTIME_DIR=", "XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "XDG_STATE_HOME=", "XDG_SESSION_", "XDG_VTNR=", "XDG_SEAT=",
	"XDG_CURRENT_DESKTOP=", "NIRI_SOCKET=", "SWAYSOCK=", "HYPRLAND_INSTANCE_SIGNATURE=", "NEFERWL_"}

func cleanEnv(in []string) []string {
	out := make([]string, 0, len(in))
next:
	for _, e := range in {
		for _, p := range headlessEnvDrop {
			if strings.HasPrefix(e, p) {
				continue next
			}
		}
		out = append(out, e)
	}
	return out
}

// startHeadless runs NeferWL (NEFERCLIENT_HEADLESS) in an isolated runtime
// directory and returns its socket path. The test is skipped when the variable
// is unset.
func startHeadless(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("NEFERCLIENT_HEADLESS")
	if bin == "" {
		t.Skip("set NEFERCLIENT_HEADLESS to a NeferWL binary")
	}
	bin, err := filepath.Abs(bin)
	require.NoError(t, err)
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"run", "config", "data", "state"} {
		dirs[name] = filepath.Join(root, name)
		require.NoError(t, os.Mkdir(dirs[name], 0o700))
	}
	logf, err := os.Create(filepath.Join(root, "neferwl.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = logf.Close() })
	cmd := exec.Command(bin, "--backend=headless", "--no-terminal", "--no-xwayland", "--size", "640x480", "--timeout", "30s")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(cleanEnv(os.Environ()),
		"XDG_RUNTIME_DIR="+dirs["run"], "XDG_CONFIG_HOME="+dirs["config"],
		"XDG_DATA_HOME="+dirs["data"], "XDG_STATE_HOME="+dirs["state"])
	cmd.Stdout, cmd.Stderr = logf, logf
	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		paths, _ := filepath.Glob(filepath.Join(dirs["run"], "wayland-*"))
		for _, p := range paths {
			if info, err := os.Stat(p); err == nil && info.Mode()&os.ModeSocket != 0 {
				return p
			}
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf("neferwl exited before serving:\n%s", b)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf("headless socket timeout:\n%s", b)
		}
	}
}

func TestHeadlessConnect(t *testing.T) {
	socket := startHeadless(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var c *neferclient.Conn
	var err error
	for { // the socket file exists before listen(); a refused dial is transient
		if c, err = neferclient.Connect(ctx, socket); err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.NoError(t, err)
	defer c.Close()

	// The compositor creates its virtual output just after the socket appears,
	// so it may arrive through OutputAdded instead of being there at Connect.
	h := &countingHandler{}
	for len(c.Outputs()) == 0 {
		select {
		case <-c.Wake():
			require.NoError(t, c.Dispatch(h))
		case <-ctx.Done():
			t.Fatal("no output announced")
		}
	}
	outs := c.Outputs()
	for _, o := range outs {
		t.Logf("output %d name=%q scale=%d %dx%d", o.Global, o.Name, o.Scale, o.Width, o.Height)
		require.Positive(t, o.Width)
		require.Positive(t, o.Height)
		require.NotZero(t, o.Scale)
	}
	require.NoError(t, c.Roundtrip())

	comp, v, err := c.Bind[*wayland.Compositor](wayland.CompositorInterface, 4, wayland.NewCompositor)
	require.NoError(t, err)
	require.NotZero(t, v)
	surface, err := comp.CreateSurface()
	require.NoError(t, err)
	require.NoError(t, surface.Destroy())
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(nil))
	require.NoError(t, c.Close())
}
