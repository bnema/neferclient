package examples_test

import (
	"bufio"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests run the example programs against a NeferWL compositor started
// headless in an isolated runtime directory, and are skipped unless
// NEFERCLIENT_HEADLESS names its binary. The lock example only ever talks to
// that nested compositor: the child environment is stripped of the caller's
// session variables, and the socket is checked to live in the temporary
// directory. Never point these tests at a real session.

var envDrop = []string{"DISPLAY=", "WAYLAND_DISPLAY=", "WAYLAND_SOCKET=", "NOTIFY_SOCKET=", "DBUS_SESSION_BUS_ADDRESS=",
	"XDG_RUNTIME_DIR=", "XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "XDG_STATE_HOME=", "XDG_SESSION_", "XDG_VTNR=", "XDG_SEAT=",
	"XDG_CURRENT_DESKTOP=", "NIRI_SOCKET=", "SWAYSOCK=", "HYPRLAND_INSTANCE_SIGNATURE=", "NEFERWL_"}

func cleanEnv() []string {
	var out []string
next:
	for _, e := range os.Environ() {
		for _, p := range envDrop {
			if strings.HasPrefix(e, p) {
				continue next
			}
		}
		out = append(out, e)
	}
	return out
}

func must(t *testing.T, err error, what ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%v: %v", fmt.Sprint(what...), err)
	}
}

type compositor struct {
	socket, shots string
	env           []string
	input         io.WriteCloser // NeferWL input script (stdin)
}

// startHeadless runs NeferWL and returns once its socket exists.
func startHeadless(t *testing.T) *compositor {
	t.Helper()
	bin := os.Getenv("NEFERCLIENT_HEADLESS")
	if bin == "" {
		t.Skip("set NEFERCLIENT_HEADLESS to a NeferWL binary")
	}
	bin, err := filepath.Abs(bin)
	must(t, err)
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"run", "config", "data", "state", "shots"} {
		dirs[name] = filepath.Join(root, name)
		must(t, os.Mkdir(dirs[name], 0o700))
	}
	logf, err := os.Create(filepath.Join(root, "neferwl.log"))
	must(t, err)
	t.Cleanup(func() { _ = logf.Close() })
	env := append(cleanEnv(), "XDG_RUNTIME_DIR="+dirs["run"], "XDG_CONFIG_HOME="+dirs["config"],
		"XDG_DATA_HOME="+dirs["data"], "XDG_STATE_HOME="+dirs["state"])
	cmd := exec.Command(bin, "--backend=headless", "--no-terminal", "--no-xwayland", "--size", "640x480",
		"--timeout", "60s", "--screenshot", dirs["shots"], "--input", "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	stdin, err := cmd.StdinPipe()
	must(t, err)
	must(t, cmd.Start())
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		paths, _ := filepath.Glob(filepath.Join(dirs["run"], "wayland-*"))
		for _, p := range paths {
			if info, err := os.Stat(p); err == nil && info.Mode()&os.ModeSocket != 0 {
				if !strings.HasPrefix(p, root) {
					t.Fatalf("socket %s is outside the test directory", p)
				}
				return &compositor{socket: p, shots: dirs["shots"], env: env, input: stdin}
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

// build compiles one example into a temporary directory.
func build(t *testing.T, pkg string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), filepath.Base(pkg))
	out, err := exec.Command("go", "build", "-o", exe, pkg).CombinedOutput()
	must(t, err, string(out))
	return exe
}

// command prepares an example to run against c, and only against c.
func (c *compositor) command(ctx context.Context, exe string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(c.env, "WAYLAND_DISPLAY="+c.socket)
	return cmd
}

func near(a, b uint32) bool {
	d := int(a>>8) - int(b>>8)
	return d >= -2 && d <= 2
}

// hasColor reports whether the PNG has the (r, g, b) colour, tolerance 2 per
// channel, at (x, y).
func hasColor(t *testing.T, path string, x, y int, r, g, b uint8) bool {
	t.Helper()
	f, err := os.Open(path)
	must(t, err)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil { // a frame being written when we look
		return false
	}
	if !image.Pt(x, y).In(img.Bounds()) {
		return false
	}
	pr, pg, pb, _ := img.At(x, y).RGBA()
	return near(pr, uint32(r)<<8) && near(pg, uint32(g)<<8) && near(pb, uint32(b)<<8)
}

func TestHeadlessLayer(t *testing.T) {
	c := startHeadless(t)
	exe := build(t, "./layer")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := c.command(ctx, exe, "-frames", "20")
	out, err := cmd.CombinedOutput()
	must(t, err, string(out))

	// The button is 200x40 logical pixels at (10, 40) of a surface placed at
	// the output's top-left corner, filled with #2060c0. Sample near its
	// bottom-right corner, away from the label. The program may already have
	// exited, so look through every screenshot taken while it was mapped.
	deadline := time.Now().Add(10 * time.Second)
	for {
		shots, _ := filepath.Glob(filepath.Join(c.shots, "*.png"))
		for _, p := range shots {
			if hasColor(t, p, 200, 75, 0x20, 0x60, 0xc0) {
				t.Logf("button colour found in %s", filepath.Base(p))
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no screenshot (of %d) has the button colour at (200, 75)", len(shots))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHeadlessLock(t *testing.T) {
	c := startHeadless(t)
	exe := build(t, "./lock")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := c.command(ctx, exe)
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	must(t, cmd.Start())
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case l := <-lines:
			if l != want {
				t.Fatalf("got %q, want %q: %s", l, want, stderr.String())
			}
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %q: %s", want, stderr.String())
		}
	}
	expect("locked")
	// Only the nested compositor receives this input; it types into the
	// password field (shown masked) and presses Enter.
	_, err = io.WriteString(c.input, "type abc\nsleep 200ms\nkey Return\n")
	must(t, err)
	expect("unlocked")
	must(t, cmd.Wait(), stderr.String())
}
