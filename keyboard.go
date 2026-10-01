package neferclient

import (
	"errors"
	"io"
	"os"
	"time"

	xkb "github.com/bnema/purego-xkbcommon"
	"github.com/bnema/purego-xkbcommon/raw"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient/internal/secret"
)

// Modifiers is the set of active keyboard modifiers.
type Modifiers uint8

const (
	ModShift Modifiers = 1 << iota
	ModCtrl
	ModAlt
	ModSuper
	ModCapsLock
	ModNumLock
)

// modNames are the xkb modifier names behind each Modifiers bit, in order.
var modNames = [...]string{"Shift", "Control", "Mod1", "Mod4", "Lock", "Mod2"}

const (
	maxHeldKeys = 32
	scratchSize = 64 // longest key or compose text kept; longer yields no text
	keymapMax   = 16 << 20
)

type heldKey struct{ evdev, sym uint32 }

type textKind uint8

const (
	kindNone textKind = iota
	kindText
	kindBackspace
)

// keyboard interprets wl_keyboard events with xkbcommon. It runs only on the
// owner goroutine. It never builds a string from key text: translations go
// through the fixed scratch array, which is wiped after every use.
type keyboard struct {
	ctx     *xkb.Context
	keymap  *xkb.Keymap
	state   *xkb.State
	compose *xkb.ComposeState

	modBits  [len(modNames)]uint32 // xkb mask per Modifiers bit; 0 when absent
	lastMask [6]uint32
	haveMask bool

	held           []heldKey
	focus          bool
	rate           int32 // repeats per second, 0 disables
	delay          int32 // milliseconds before the first repeat
	repeating      uint32
	composePending bool

	tfd  int // repeat timerfd
	tbuf [8]byte

	secret  *SecretBuffer
	scratch [scratchSize]byte

	// Work area of doSecret, which secret.Do runs without arguments.
	secretFn func()
	w        struct {
		evdev, sym uint32
		mods       Modifiers
		repeat     bool
		kind       textKind
		changed    bool
		err        error
	}
	changed bool // the last translation edited the SecretBuffer
}

func newKeyboard(locale string) (*keyboard, error) {
	c, err := xkb.NewContext()
	if err != nil {
		return nil, err
	}
	k := &keyboard{ctx: c, held: make([]heldKey, 0, maxHeldKeys), tfd: -1}
	k.secretFn = k.doSecret
	if t, e := c.NewComposeTable(locale); e == nil {
		k.compose, _ = t.NewState() // compose is optional: without it, dead keys still come from xkb
		_ = t.Close()
	}
	if k.tfd, err = unix.TimerfdCreate(unix.CLOCK_MONOTONIC, unix.TFD_NONBLOCK|unix.TFD_CLOEXEC); err != nil {
		_ = c.Close()
		return nil, err
	}
	return k, nil
}

// composeLocale picks the locale for the compose table the way libc does.
func composeLocale() string {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return "C"
}

func (k *keyboard) close() {
	k.disarm()
	if k.tfd >= 0 {
		_ = unix.Close(k.tfd)
		k.tfd = -1
	}
	if k.compose != nil {
		_ = k.compose.Close()
		k.compose = nil
	}
	if k.state != nil {
		_ = k.state.Close()
		k.state = nil
	}
	if k.keymap != nil {
		_ = k.keymap.Close()
		k.keymap = nil
	}
	if k.ctx != nil {
		_ = k.ctx.Close()
		k.ctx = nil
	}
}

// replaceFD installs the keymap in fd. It consumes fd even on failure; the
// previous keymap stays valid on failure.
func (k *keyboard) replaceFD(fd, size int) error {
	f := os.NewFile(uintptr(fd), "keymap")
	if f == nil {
		return errors.New("neferclient: invalid keymap descriptor")
	}
	defer f.Close()
	km, err := k.ctx.NewKeymapFD(fd, size)
	if err != nil {
		return err
	}
	return k.replace(km)
}

func (k *keyboard) replace(km *xkb.Keymap) error {
	s, err := km.NewState()
	if err != nil {
		_ = km.Close()
		return err
	}
	k.reset()
	if k.state != nil {
		_ = k.state.Close()
	}
	if k.keymap != nil {
		_ = k.keymap.Close()
	}
	k.keymap, k.state = km, s
	for i, name := range modNames {
		k.modBits[i] = 0
		if idx, e := km.ModIndex(name); e == nil && idx < 32 {
			k.modBits[i] = 1 << idx
		}
	}
	if k.haveMask { // the compositor's modifiers survive a keymap change
		_, _ = s.UpdateMask(k.lastMask[0], k.lastMask[1], k.lastMask[2], k.lastMask[3], k.lastMask[4], k.lastMask[5])
	}
	return nil
}

func (k *keyboard) setMask(depressed, latched, locked, group uint32) error {
	k.lastMask = [6]uint32{depressed, latched, locked, 0, 0, group}
	k.haveMask = true
	if k.state == nil {
		return nil
	}
	_, err := k.state.UpdateMask(depressed, latched, locked, 0, 0, group)
	return err
}

// reset drops held keys, repeat and compose state. It keeps the keymap, the
// modifier mask and the focus flag.
func (k *keyboard) reset() {
	k.held = k.held[:0]
	k.repeating = 0
	k.disarm()
	k.composePending = false
	if k.compose != nil {
		_ = k.compose.Reset()
	}
}

func (k *keyboard) setFocus(on bool) {
	k.focus = on
	k.reset()
}

func (k *keyboard) setSecret(b *SecretBuffer) {
	k.secret = b
	k.reset() // switching paths cancels held keys, repeat and compose
}

func (k *keyboard) setRepeat(rate, delay int32) {
	k.rate, k.delay = max(rate, 0), max(delay, 0)
	if k.rate == 0 {
		k.repeating = 0
		k.disarm()
	}
}

// arm starts the repeat timer: first expiry after the delay, then at the rate.
func (k *keyboard) arm() {
	spec := unix.ItimerSpec{
		Interval: unix.NsecToTimespec(int64(time.Second) / int64(k.rate)),
		Value:    unix.NsecToTimespec(max(int64(k.delay)*int64(time.Millisecond), 1)), // zero would disarm
	}
	_ = unix.TimerfdSettime(k.tfd, 0, &spec, nil)
}

func (k *keyboard) disarm() {
	if k.tfd >= 0 {
		var spec unix.ItimerSpec
		_ = unix.TimerfdSettime(k.tfd, 0, &spec, nil)
	}
}

// expired consumes a timer expiration. Missed ticks are not accumulated: one
// call yields at most one repeat.
func (k *keyboard) expired() bool {
	n, err := unix.Read(k.tfd, k.tbuf[:])
	return err == nil && n == 8
}

func (k *keyboard) modifiers() Modifiers {
	mods, err := k.state.Mods()
	if err != nil {
		return 0
	}
	var m Modifiers
	for i, bit := range k.modBits {
		if bit != 0 && mods&bit != 0 {
			m |= 1 << i
		}
	}
	return m
}

func (k *keyboard) find(evdev uint32) int {
	for i := range k.held {
		if k.held[i].evdev == evdev {
			return i
		}
	}
	return -1
}

func (k *keyboard) drop(i int) { k.held = append(k.held[:i], k.held[i+1:]...) }

// press translates a key press into ev. It reports false when the press must
// not be delivered (no focus or keymap).
func (k *keyboard) press(evdev uint32, ev *KeyEvent) (bool, error) {
	if !k.focus || k.state == nil {
		return false, nil
	}
	if i := k.find(evdev); i >= 0 {
		k.drop(i)
	}
	if len(k.held) == cap(k.held) {
		return false, nil
	}
	sym, err := k.state.KeySym(xkb.WaylandKeycode(evdev))
	if err != nil {
		return false, err
	}
	kind, err := k.translate(evdev, sym, false, ev)
	if err != nil {
		return false, err
	}
	k.held = append(k.held, heldKey{evdev, sym})
	if k.secret == nil && repeatable(sym) || k.secret != nil && (kind == kindBackspace || kind == kindText && repeatable(sym)) {
		if k.rate > 0 {
			k.repeating = evdev
			k.arm()
		}
	} else if k.secret != nil && k.repeating != 0 && repeatable(sym) {
		// A control or rejected key stops a previous printable repeat;
		// a modifier does not.
		k.repeating = 0
		k.disarm()
	}
	return true, nil
}

// release fills ev for a key released after a delivered press.
func (k *keyboard) release(evdev uint32, ev *KeyEvent) bool {
	i := k.find(evdev)
	if i < 0 {
		return false
	}
	h := k.held[i]
	k.drop(i)
	if k.repeating == evdev {
		k.repeating = 0
		k.disarm()
	}
	ev.Keycode, ev.Keysym, ev.Pressed = evdev, h.sym, false
	if k.state != nil {
		ev.Modifiers = k.modifiers()
		ev.Group, _ = k.state.Layout()
	}
	return true
}

// repeat produces one repeat of the held key whose timer fired.
func (k *keyboard) repeat(ev *KeyEvent) (bool, error) {
	if !k.focus || k.state == nil || k.rate <= 0 || k.repeating == 0 || k.find(k.repeating) < 0 {
		return false, nil
	}
	evdev := k.repeating
	stop := func() (bool, error) {
		k.repeating = 0
		k.disarm()
		return false, nil
	}
	sym, err := k.state.KeySym(xkb.WaylandKeycode(evdev))
	if err != nil {
		_, _ = stop()
		return false, err
	}
	if k.secret == nil && !repeatable(sym) {
		return stop()
	}
	kind, err := k.translate(evdev, sym, true, ev)
	if err != nil {
		_, _ = stop()
		return false, err
	}
	if k.secret != nil && kind != kindBackspace && !(kind == kindText && repeatable(sym)) {
		return stop()
	}
	return true, nil
}

// translate fills ev for a press or repeat and returns what the key meant to
// the SecretBuffer, if one is set. In secret mode ev.Text stays nil.
func (k *keyboard) translate(evdev, sym uint32, repeat bool, ev *KeyEvent) (textKind, error) {
	ev.Keycode, ev.Keysym, ev.Pressed, ev.Repeat = evdev, sym, true, repeat
	ev.Modifiers = k.modifiers()
	ev.Group, _ = k.state.Layout()
	k.changed = false
	if k.secret != nil {
		w := &k.w
		w.evdev, w.sym, w.mods, w.repeat = evdev, sym, ev.Modifiers, repeat
		secret.Do(k.secretFn)
		k.changed = w.changed
		return w.kind, w.err
	}
	n, err := k.text(evdev, sym, repeat)
	if err != nil {
		return kindNone, err
	}
	if n > 0 {
		ev.Text = k.scratch[:n:n]
	}
	return kindNone, nil
}

// doSecret translates one key into the SecretBuffer. It is the only code that
// handles secret text, so it runs under runtime/secret when available.
func (k *keyboard) doSecret() {
	w := &k.w
	w.kind, w.changed, w.err = kindNone, false, nil
	defer clear(k.scratch[:])
	switch {
	case w.sym == raw.XKB_KEY_BackSpace:
		w.kind = kindBackspace
		k.resetCompose()
		w.changed = k.secret.backspace()
	case w.sym == raw.XKB_KEY_Return || w.sym == raw.XKB_KEY_KP_Enter || w.sym == raw.XKB_KEY_Escape:
		k.resetCompose() // controls are delivered as keys only
	case w.mods&ModCtrl != 0:
		// Other Control combinations are neither secret text nor ours to handle.
	default:
		n, err := k.text(w.evdev, w.sym, w.repeat)
		if err != nil {
			w.err = err
			return
		}
		if classifySecretText(k.scratch[:n]) {
			w.kind = kindText
			w.changed = k.secret.appendText(k.scratch[:n])
		}
	}
}

func (k *keyboard) resetCompose() {
	if k.compose != nil {
		_ = k.compose.Reset()
	}
	k.composePending = false
}

// text writes the text of a key into scratch and returns its length. Compose
// is fed only by physical presses: a repeat neither advances a sequence nor
// replays composed text.
func (k *keyboard) text(evdev, sym uint32, repeat bool) (int, error) {
	if k.compose != nil && !repeat {
		if _, err := k.compose.Feed(sym); err != nil {
			return 0, err
		}
		status, err := k.compose.Status()
		if err != nil {
			return 0, err
		}
		switch status {
		case raw.XKB_COMPOSE_COMPOSING:
			k.composePending = true
			return 0, nil
		case raw.XKB_COMPOSE_CANCELLED:
			k.resetCompose()
			return 0, nil
		case raw.XKB_COMPOSE_COMPOSED:
			n, err := k.compose.UTF8Into(k.scratch[:])
			k.resetCompose()
			return shortAsEmpty(n, err)
		default:
			k.composePending = false
		}
	}
	if k.composePending {
		return 0, nil
	}
	n, err := k.state.UTF8Into(xkb.WaylandKeycode(evdev), k.scratch[:])
	return shortAsEmpty(n, err)
}

// shortAsEmpty maps a too-long text to no text; xkb already wiped the buffer.
func shortAsEmpty(n int, err error) (int, error) {
	if errors.Is(err, io.ErrShortBuffer) {
		return 0, nil
	}
	return n, err
}

// Modifier, dead and compose keys never repeat. The compositor provides rate
// and delay; its keymap is authoritative for text and layout.
func repeatable(sym uint32) bool {
	switch sym {
	case raw.XKB_KEY_Shift_L, raw.XKB_KEY_Shift_R, raw.XKB_KEY_Control_L, raw.XKB_KEY_Control_R,
		raw.XKB_KEY_Alt_L, raw.XKB_KEY_Alt_R, raw.XKB_KEY_Super_L, raw.XKB_KEY_Super_R,
		raw.XKB_KEY_Caps_Lock, raw.XKB_KEY_Num_Lock, raw.XKB_KEY_Multi_key:
		return false
	}
	return sym < raw.XKB_KEY_dead_grave || sym > raw.XKB_KEY_dead_currency
}
