package neferclient

import "unicode/utf8"

// maxSecretCapacity bounds [NewSecretBuffer].
const maxSecretCapacity = 4096

// SecretBuffer collects text typed while it is installed with
// [Seat.SetSecret], for example a password. Its storage is allocated once at
// full size, so it never reallocates and leaves no stale copies behind; the
// text is never turned into a string by the library.
//
// Only the owner goroutine (the one calling [Conn.Dispatch]) may use it.
type SecretBuffer struct {
	buf    []byte // capacity*utf8.UTFMax bytes, fixed
	n      int    // bytes in use
	points int    // code points in use
	max    int    // capacity in code points
}

// NewSecretBuffer returns a buffer holding up to capacity code points.
// capacity is clamped to 1..4096.
func NewSecretBuffer(capacity int) *SecretBuffer {
	capacity = min(max(capacity, 1), maxSecretCapacity)
	return &SecretBuffer{buf: make([]byte, capacity*utf8.UTFMax), max: capacity}
}

// Len returns the number of code points held.
func (b *SecretBuffer) Len() int {
	if b == nil {
		return 0
	}
	return b.points
}

// Bytes returns the UTF-8 text held. The slice aliases the buffer: do not
// retain it, convert it to a string or log it; it changes on the next edit.
func (b *SecretBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf[:b.n:b.n]
}

// Wipe zeroes the whole storage and empties the buffer.
func (b *SecretBuffer) Wipe() {
	if b == nil {
		return
	}
	clear(b.buf)
	b.n, b.points = 0, 0
}

// appendText adds text, which must be valid UTF-8. It reports false and
// changes nothing when the text does not fit.
func (b *SecretBuffer) appendText(text []byte) bool {
	cp := utf8.RuneCount(text)
	if cp == 0 || b.points+cp > b.max {
		return false
	}
	b.n += copy(b.buf[b.n:], text)
	b.points += cp
	return true
}

// backspace removes the last code point and wipes its bytes.
func (b *SecretBuffer) backspace() bool {
	if b.points == 0 {
		return false
	}
	_, size := utf8.DecodeLastRune(b.buf[:b.n])
	clear(b.buf[b.n-size : b.n])
	b.n -= size
	b.points--
	return true
}

// classifySecretText reports whether text is printable input for a secret:
// non-empty, valid UTF-8 and free of C0 controls and DEL.
func classifySecretText(text []byte) bool {
	if len(text) == 0 || !utf8.Valid(text) {
		return false
	}
	for rest := text; len(rest) > 0; {
		r, size := utf8.DecodeRune(rest)
		if r < 0x20 || r == 0x7f {
			return false
		}
		rest = rest[size:]
	}
	return true
}
