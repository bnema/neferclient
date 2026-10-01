package neferclient

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/purego-xkbcommon/raw"
)

// Evdev codes used below (US QWERTY positions).
const (
	keyQ         = 16
	keyE         = 18
	keyBackspace = 14
	keyEnter     = 28
	keyU         = 22
	keyLeftShift = 42
	keyCircum    = 41 // de: dead_circumflex
)

// testKeyboard is a keyboard on a real xkb keymap compiled from rules.
func testKeyboard(t testing.TB, locale, layout string) *keyboard {
	t.Helper()
	k, err := newKeyboard(locale)
	require.NoError(t, err)
	t.Cleanup(k.close)
	km, err := k.ctx.NewKeymapRules("evdev", "pc105", layout, "", "")
	require.NoError(t, err)
	require.NoError(t, k.replace(km))
	k.setFocus(true)
	return k
}

func pressKey(t testing.TB, k *keyboard, code uint32) (KeyEvent, bool) {
	t.Helper()
	var ev KeyEvent
	ok, err := k.press(code, &ev)
	require.NoError(t, err)
	return ev, ok
}

func releaseKey(k *keyboard, code uint32) (KeyEvent, bool) {
	var ev KeyEvent
	ok := k.release(code, &ev)
	return ev, ok
}

func TestKeyboardLayoutsAndModifiers(t *testing.T) {
	for _, tc := range []struct {
		layout string
		code   uint32
		want   string
	}{{"us", keyQ, "q"}, {"fr", keyQ, "a"}, {"de", 21, "z"}} {
		k := testKeyboard(t, "en_US.UTF-8", tc.layout)
		ev, ok := pressKey(t, k, tc.code)
		require.True(t, ok)
		require.Equal(t, tc.want, string(ev.Text), tc.layout)
		require.True(t, ev.Pressed)
		require.NotZero(t, ev.Keysym)
		require.Equal(t, tc.code, ev.Keycode)
		rel, ok := releaseKey(k, tc.code)
		require.True(t, ok)
		require.False(t, rel.Pressed)
		require.Nil(t, rel.Text)
		require.Equal(t, ev.Keysym, rel.Keysym)
	}

	k := testKeyboard(t, "en_US.UTF-8", "us")
	idx, err := k.keymap.ModIndex("Shift")
	require.NoError(t, err)
	require.NoError(t, k.setMask(1<<idx, 0, 0, 0))
	ev, _ := pressKey(t, k, keyQ)
	require.Equal(t, "Q", string(ev.Text))
	require.Equal(t, ModShift, ev.Modifiers)

	// The compositor's mask survives a keymap change.
	km, err := k.ctx.NewKeymapRules("evdev", "pc105", "fr", "", "")
	require.NoError(t, err)
	require.NoError(t, k.replace(km))
	k.setFocus(true)
	ev, _ = pressKey(t, k, keyQ)
	require.Equal(t, "A", string(ev.Text))
}

func TestKeyboardKeymapFD(t *testing.T) {
	data, err := os.ReadFile("testdata/fr.xkb")
	require.NoError(t, err)
	f, err := os.CreateTemp(t.TempDir(), "keymap")
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(append(data, 0))
	require.NoError(t, err)

	k := testKeyboard(t, "", "us")
	fd, err := unix.Dup(int(f.Fd()))
	require.NoError(t, err)
	require.NoError(t, k.replaceFD(fd, len(data)+1))
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.ErrorIs(t, err, unix.EBADF, "replaceFD consumes the descriptor")
	k.setFocus(true)
	ev, _ := pressKey(t, k, keyQ)
	require.Equal(t, "a", string(ev.Text))

	// A failing map keeps the previous one and still consumes its fd.
	bad, err := unix.Dup(int(f.Fd()))
	require.NoError(t, err)
	require.Error(t, k.replaceFD(bad, 5))
	_, err = unix.FcntlInt(uintptr(bad), unix.F_GETFD, 0)
	require.ErrorIs(t, err, unix.EBADF)
	ev, _ = pressKey(t, k, keyQ)
	require.Equal(t, "a", string(ev.Text))
}

func TestKeyboardComposeAndFocus(t *testing.T) {
	k := testKeyboard(t, "en_US.UTF-8", "de")
	dead, _ := pressKey(t, k, keyCircum)
	require.Equal(t, uint32(raw.XKB_KEY_dead_circumflex), dead.Keysym)
	require.Nil(t, dead.Text)
	releaseKey(k, keyCircum)
	composed, _ := pressKey(t, k, keyE)
	require.Equal(t, "ê", string(composed.Text))
	releaseKey(k, keyE)

	pressKey(t, k, keyCircum)
	k.setFocus(false)
	_, ok := pressKey2(k, keyE)
	require.False(t, ok, "no key without focus")
	k.setFocus(true)
	ev, _ := pressKey(t, k, keyE)
	require.Equal(t, "e", string(ev.Text), "focus loss resets compose")
}

func pressKey2(k *keyboard, code uint32) (KeyEvent, bool) {
	var ev KeyEvent
	ok, _ := k.press(code, &ev)
	return ev, ok
}

func TestKeyboardRepeatTimer(t *testing.T) {
	k := testKeyboard(t, "", "us")
	k.setRepeat(20, 50)
	_, ok := pressKey(t, k, keyQ)
	require.True(t, ok)
	require.Equal(t, uint32(keyQ), k.repeating)
	var ev KeyEvent
	require.Eventually(t, k.expired, 2e9, 5e6)
	ok, err := k.repeat(&ev)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, ev.Repeat)
	require.Equal(t, "q", string(ev.Text))

	releaseKey(k, keyQ)
	require.Zero(t, k.repeating)
	require.False(t, k.expired(), "timer disarmed on release")

	// Modifiers never repeat.
	pressKey(t, k, keyLeftShift)
	require.Zero(t, k.repeating)
	// A rate of 0 disables repeat.
	k.setRepeat(0, 0)
	pressKey(t, k, keyQ)
	require.Zero(t, k.repeating)
}

func TestKeyboardRepeatTracksCurrentMask(t *testing.T) {
	k := testKeyboard(t, "", "us")
	k.setRepeat(100, 1)
	pressKey(t, k, keyQ)
	idx, err := k.keymap.ModIndex("Shift")
	require.NoError(t, err)
	require.NoError(t, k.setMask(1<<idx, 0, 0, 0))
	require.Eventually(t, k.expired, 2e9, 1e6)
	var ev KeyEvent
	ok, err := k.repeat(&ev)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Q", string(ev.Text))
}

// TestSecretKeysNeverReachText covers the secret rules end to end on the
// keyboard: printable text only into the buffer, BackSpace by code point,
// every other key delivered without text.
func TestSecretKeysNeverReachText(t *testing.T) {
	k := testKeyboard(t, "en_US.UTF-8", "de")
	buf := NewSecretBuffer(8)
	k.setSecret(buf)

	press := func(code uint32) KeyEvent {
		ev, ok := pressKey(t, k, code)
		require.True(t, ok)
		require.Nil(t, ev.Text, "secret mode never fills KeyEvent.Text")
		releaseKey(k, code)
		return ev
	}
	press(keyQ)
	press(keyCircum) // dead key
	press(keyE)      // composes ê
	require.Equal(t, "qê", string(buf.Bytes()))
	require.Equal(t, 2, buf.Len())

	press(keyBackspace)
	require.Equal(t, 1, buf.Len(), "backspace removes one code point")
	press(keyBackspace)
	press(keyBackspace) // empty: nothing to remove
	require.Zero(t, buf.Len())

	press(keyQ)
	enter := press(keyEnter)
	require.Equal(t, uint32(raw.XKB_KEY_Return), enter.Keysym)
	require.Equal(t, 1, buf.Len(), "Enter is delivered, not typed")

	// Control combinations are keys, not text.
	idx, err := k.keymap.ModIndex("Control")
	require.NoError(t, err)
	require.NoError(t, k.setMask(1<<idx, 0, 0, 0))
	ev := press(keyU)
	require.Equal(t, ModCtrl, ev.Modifiers)
	require.Equal(t, 1, buf.Len())
	require.NoError(t, k.setMask(0, 0, 0, 0))

	// Capacity: extra text is dropped, the key is still delivered.
	for range 10 {
		press(keyQ)
	}
	require.Equal(t, 8, buf.Len())

	// Scratch is wiped after use.
	require.Equal(t, make([]byte, scratchSize), k.scratch[:])

	k.setSecret(nil)
	ev, _ = pressKey(t, k, keyQ)
	require.Equal(t, "q", string(ev.Text), "ordinary text again once the buffer is removed")
}

func TestSecretChangedFlag(t *testing.T) {
	k := testKeyboard(t, "", "us")
	buf := NewSecretBuffer(2)
	k.setSecret(buf)
	pressKey(t, k, keyQ)
	require.True(t, k.changed)
	pressKey(t, k, keyEnter)
	require.False(t, k.changed)
	pressKey(t, k, keyBackspace)
	require.True(t, k.changed)
	pressKey(t, k, keyBackspace)
	require.False(t, k.changed, "nothing left to remove")
}

func TestSecretRepeat(t *testing.T) {
	k := testKeyboard(t, "", "us")
	buf := NewSecretBuffer(16)
	k.setSecret(buf)
	k.setRepeat(100, 1)
	var ev KeyEvent
	pressKey(t, k, keyQ)
	for range 2 {
		require.Eventually(t, k.expired, 2e9, 1e6)
		ok, err := k.repeat(&ev)
		require.NoError(t, err)
		require.True(t, ok)
		require.Nil(t, ev.Text)
	}
	require.Equal(t, "qqq", string(buf.Bytes()))

	// Enter cancels a previous printable repeat.
	pressKey(t, k, keyEnter)
	require.Zero(t, k.repeating)
	releaseKey(k, keyQ)
	releaseKey(k, keyEnter)

	// Held backspace repeats.
	pressKey(t, k, keyBackspace)
	require.Equal(t, uint32(keyBackspace), k.repeating)
	require.Eventually(t, k.expired, 2e9, 1e6)
	ok, err := k.repeat(&ev)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, buf.Len())
}
