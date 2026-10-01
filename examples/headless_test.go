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
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The tests run the example programs against a NeferWL compositor started
// headless in an isolated runtime directory, and are skipped unless
// NEFERCLIENT_HEADLESS names its binary. The lock example only ever talks to
// that nested compositor: the child environment is stripped of the caller's
// session variables, and the socket is checked to be absolute (an absolute
// WAYLAND_DISPLAY is how the example finds it). Never point these tests at a
// real session.

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

// syncBuffer is a bytes buffer safe for the copying goroutine of exec and the
// test goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
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
				if !filepath.IsAbs(p) {
					t.Fatalf("socket %q is not absolute", p)
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
	// slices.Concat copies: c.env is never aliased or modified.
	cmd.Env = slices.Concat(c.env, []string{"WAYLAND_DISPLAY=" + c.socket})
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
	exe := build(t, "./layer")
	c := startHeadless(t)
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

// TestHeadlessLayerInputRegion checks that the layer example forwards the
// view's input rectangle to the compositor: NeferWL only delivers pointer
// events to the surface inside the button, not over the label above it.
func TestHeadlessLayerInputRegion(t *testing.T) {
	exe := build(t, "./layer")
	c := startHeadless(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := c.command(ctx, exe, "-log-pointer") // runs until killed
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	must(t, cmd.Start())
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
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
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for %q: %s", want, stderr.String())
		}
	}

	// The region is committed with the first buffer, so once the button is
	// visible in a screenshot the compositor knows it. The surface is at the
	// output's top-left corner (see TestHeadlessLayer).
	deadline := time.Now().Add(10 * time.Second)
	for visible := false; !visible; time.Sleep(50 * time.Millisecond) {
		shots, _ := filepath.Glob(filepath.Join(c.shots, "*.png"))
		for _, p := range shots {
			visible = visible || hasColor(t, p, 200, 75, 0x20, 0x60, 0xc0)
		}
		if !visible && time.Now().After(deadline) {
			t.Fatalf("the button never appeared: %s", stderr.String())
		}
	}

	// (50, 20) is over the label, outside the input region: the surface sees
	// nothing. (100, 60) is over the button: the first event the program
	// prints is its enter, so the label position did not enter earlier.
	// Back on the label, the pointer leaves the surface again.
	_, err = io.WriteString(c.input, "move 50 20\nsleep 300ms\nmove 100 60\nsleep 300ms\nmove 50 20\n")
	must(t, err)
	expect("pointer enter 100 60")
	expect("pointer leave")
}

func TestHeadlessLock(t *testing.T) {
	exe := build(t, "./lock")
	c := startHeadless(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := c.command(ctx, exe)
	stdout, err := cmd.StdoutPipe()
	must(t, err)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	must(t, cmd.Start())
	// Wait runs once: here, after the program is done, or from the cleanup if
	// the test fails first (the context kill ends the process).
	var waitOnce sync.Once
	var waitErr error
	wait := func() error {
		waitOnce.Do(func() { waitErr = cmd.Wait() })
		return waitErr
	}
	t.Cleanup(func() { cancel(); _ = wait() })
	var all syncBuffer // everything the program printed on stdout
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			_, _ = all.Write(append(sc.Bytes(), '\n'))
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
	expect("focused") // the keyboard reached a lock surface: typing is safe
	// Only the nested compositor receives this input; it types into the
	// password field (shown masked) and presses Enter.
	_, err = io.WriteString(c.input, "type abc\nsleep 200ms\nkey Return\n")
	must(t, err)
	expect("unlocked")
	for range lines { // drain until the program closes stdout
	}
	must(t, wait(), stderr.String())
	// The typed text must not leak to the program's output.
	if strings.Contains(all.String(), "abc") || strings.Contains(stderr.String(), "abc") {
		t.Fatal("the typed text appears in the program output")
	}
}
