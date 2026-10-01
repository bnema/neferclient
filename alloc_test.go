package neferclient_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/bnema/neferclient"
)

// countingHandler is the measured system's input, not a double of a port: it
// only counts, so the test sees the cost of the library alone.
type countingHandler struct{ fdReady, added, removed, errs int }

func (h *countingHandler) OutputAdded(*neferclient.Output) { h.added++ }
func (h *countingHandler) OutputRemoved(uint32)            { h.removed++ }
func (h *countingHandler) FDReady(uint64)                  { h.fdReady++ }
func (h *countingHandler) Error(error)                     { h.errs++ }

// TestAllocDispatch measures the whole steady-state pipeline over a real
// socketpair: wire frames in, reader decode, queue, Dispatch (output state
// updates and a watched-fd notification with epoll re-arm). testing's
// AllocsPerRun counts every goroutine, so reader and epoll allocations are
// included.
func TestAllocDispatch(t *testing.T) {
	skipUnderRace(t)
	c, srv := connectWire(t, []wireGlobal{{7, outputIface, 4}}, map[uint32]outputSpec{7: {"DP-1", 1, 100, 100}})
	obj := srv.waitBound(7)

	const scaleEvents = 200
	var batch []byte
	for i := range scaleEvents {
		batch = append(batch, frame(obj.id, 3, appendU32(nil, uint32(1+i%2)))...)
	}
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	require.NoError(t, err)
	defer unix.Close(efd)
	require.NoError(t, c.WatchFD(efd, 1))
	one := [8]byte{1}
	var drain [8]byte

	h := &countingHandler{}
	step := func() {
		if _, err := srv.conn.Write(batch); err != nil {
			t.Fatal(err)
		}
		_, _ = unix.Write(efd, one[:])
		for c.QueueLen() < scaleEvents+1 {
			runtime.Gosched()
		}
		_, _ = unix.Read(efd, drain[:])
		if err := c.Dispatch(h); err != nil {
			t.Fatal(err)
		}
	}
	step() // warm-up beyond AllocsPerRun's own: grow pools and buffers
	step()
	allocs := testing.AllocsPerRun(50, step)
	t.Logf("allocs per Dispatch of %d events: %v", scaleEvents+1, allocs)
	require.Zero(t, allocs)
	require.Positive(t, h.fdReady)
	require.Zero(t, h.errs)
}
