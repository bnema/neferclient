package neferclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/neferclient"
	neferclientmocks "github.com/bnema/neferclient/mocks"
)

func connectHeadless(t *testing.T) *neferclient.Conn {
	t.Helper()
	socket := startHeadless(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		c, err := neferclient.Connect(ctx, socket)
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			for len(c.Outputs()) == 0 { // the virtual output may arrive late
				select {
				case <-c.Wake():
					require.NoError(t, c.Dispatch(nil))
				case <-ctx.Done():
					t.Fatal("no output announced")
				}
			}
			return c
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHeadlessLayerSurface(t *testing.T) {
	c := connectHeadless(t)
	s, err := c.NewLayerSurface(neferclient.LayerConfig{
		Level: neferclient.LayerTop, Anchors: neferclient.AnchorTop | neferclient.AnchorLeft,
		Width: 200, Height: 50,
	})
	require.NoError(t, err)

	var configured, feedback bool
	var cw, ch int32
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Configure(s.ID(), mock.Anything, mock.Anything).Run(func(_ neferclient.SurfaceID, w, hh int32) {
		configured, cw, ch = true, w, hh
	}).Return().Maybe()
	h.EXPECT().FeedbackDone(s.ID()).Run(func(neferclient.SurfaceID) { feedback = true }).Return().Maybe()
	h.EXPECT().Scale(s.ID(), mock.Anything).Return().Maybe()
	dispatchUntil(t, c, h, func() bool { return configured && feedback })

	require.Equal(t, int32(200), cw)
	require.Equal(t, int32(50), ch)
	fb := s.Feedback()
	require.NotNil(t, fb)
	require.NotZero(t, fb.MainDevice)
	require.NotEmpty(t, fb.Formats)
	t.Logf("main device %#x, %d formats", fb.MainDevice, len(fb.Formats))
	pw, ph, err := s.PhysicalSize()
	require.NoError(t, err)
	require.Positive(t, pw)
	require.Positive(t, ph)
	require.NoError(t, s.SetInputRegion([]neferclient.Rect{{X: 0, Y: 0, Width: 10, Height: 10}}))
	require.NoError(t, s.Close())
	require.NoError(t, c.Roundtrip())
}

func TestHeadlessLockUnlock(t *testing.T) {
	c := connectHeadless(t)
	l, err := c.Lock()
	require.NoError(t, err)
	require.Error(t, l.Unlock(), "unlock before locked must be refused")

	var locked bool
	var surfaces []*neferclient.Surface
	for _, o := range c.Outputs() {
		s, err := l.NewSurface(o.Global)
		require.NoError(t, err)
		surfaces = append(surfaces, s)
	}
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Locked().Run(func() { locked = true }).Return().Maybe()
	h.EXPECT().Configure(mock.Anything, mock.Anything, mock.Anything).Return().Maybe()
	h.EXPECT().FeedbackDone(mock.Anything).Return().Maybe()
	h.EXPECT().Scale(mock.Anything, mock.Anything).Return().Maybe()
	dispatchUntil(t, c, h, func() bool { return locked })
	require.True(t, l.Locked())
	for _, s := range surfaces {
		w, hh, _ := s.Size()
		t.Logf("lock surface %d: %dx%d", s.ID(), w, hh)
	}
	require.Error(t, l.Close(), "a locked session is not closed, only unlocked")
	require.NoError(t, l.Unlock())
	require.False(t, l.Locked())
	for _, s := range surfaces {
		require.NoError(t, s.Close())
	}
	require.NoError(t, c.Roundtrip())
}

// TestHeadlessSeat binds the seat against a real compositor and drives it
// through a surface: the seat bind, capability handling and cursor request
// must not fail, whatever devices the headless backend offers.
func TestHeadlessSeat(t *testing.T) {
	c := connectHeadless(t)
	s, err := c.NewLayerSurface(neferclient.LayerConfig{
		Level: neferclient.LayerTop, Anchors: neferclient.AnchorTop | neferclient.AnchorLeft,
		Width: 100, Height: 40, Keyboard: neferclient.KeyboardOnDemand,
	})
	require.NoError(t, err)
	seat := c.Seat()
	require.NotNil(t, seat)
	seat.SetSecret(neferclient.NewSecretBuffer(16))
	seat.SetSecret(nil)

	var configured bool
	h := neferclientmocks.NewMockHandler(t)
	h.EXPECT().Configure(s.ID(), mock.Anything, mock.Anything).Run(func(neferclient.SurfaceID, int32, int32) { configured = true }).Return().Maybe()
	h.EXPECT().FeedbackDone(mock.Anything).Return().Maybe()
	h.EXPECT().Scale(mock.Anything, mock.Anything).Return().Maybe()
	h.EXPECT().Pointer(mock.Anything).Return().Maybe()
	h.EXPECT().Key(mock.Anything).Return().Maybe()
	h.EXPECT().KeyboardFocus(mock.Anything, mock.Anything).Return().Maybe()
	h.EXPECT().SecretChanged(mock.Anything).Return().Maybe()
	dispatchUntil(t, c, h, func() bool { return configured })
	require.NoError(t, c.Roundtrip())
	require.NoError(t, c.Dispatch(h))
	require.NoError(t, seat.SetCursor(neferclient.CursorPointer))
	require.NoError(t, s.Close())
}
