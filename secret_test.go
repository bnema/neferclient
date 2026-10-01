package neferclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretBufferEditing(t *testing.T) {
	b := NewSecretBuffer(4)
	storage := b.buf
	require.True(t, b.appendText([]byte("a")))
	require.True(t, b.appendText([]byte("é€")))
	require.Equal(t, 3, b.Len())
	require.Equal(t, []byte("aé€"), b.Bytes())

	require.True(t, b.backspace()) // one code point, not one byte
	require.Equal(t, 2, b.Len())
	require.Equal(t, []byte("aé"), b.Bytes())
	require.Equal(t, make([]byte, 3), storage[3:6], "removed bytes are wiped")

	require.False(t, b.appendText([]byte("xyz")), "does not fit: nothing changes")
	require.Equal(t, []byte("aé"), b.Bytes())
	require.True(t, b.appendText([]byte("xy")))
	require.False(t, b.appendText([]byte("z")), "full")

	b.Wipe()
	require.Zero(t, b.Len())
	require.Empty(t, b.Bytes())
	require.Equal(t, make([]byte, len(storage)), storage, "whole storage zeroed")
	require.False(t, b.backspace())
}

func TestSecretBufferCapacityClampAndNil(t *testing.T) {
	require.Equal(t, 1, NewSecretBuffer(-5).max)
	require.Equal(t, 1, NewSecretBuffer(0).max)
	require.Equal(t, maxSecretCapacity, NewSecretBuffer(1<<20).max)
	var b *SecretBuffer
	require.Zero(t, b.Len())
	require.Nil(t, b.Bytes())
	b.Wipe()
}

func TestClassifySecretText(t *testing.T) {
	for _, ok := range []string{"a", "é", "€", " ", "日本"} {
		require.True(t, classifySecretText([]byte(ok)), ok)
	}
	for _, bad := range [][]byte{nil, {}, {'\r'}, {0x1b}, {0x7f}, {'a', '\n'}, {0xff}, {0xc3}} {
		require.False(t, classifySecretText(bad), "%q", bad)
	}
}
