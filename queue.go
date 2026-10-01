package neferclient

import (
	"sync"

	"golang.org/x/sys/unix"
)

const (
	ringSize   = 256 // events queued between the reader and Dispatch
	maxNameLen = 64  // longest output name kept; longer names are truncated
)

type eventKind uint8

const (
	evGlobal       eventKind = iota + 1 // registry global (wl_output only): global, version
	evGlobalRemove                      // registry global_remove: global
	evOutputName                        // name, global
	evOutputScale                       // a = factor, global
	evOutputMode                        // flags, a = width, b = height, global
	evOutputDone                        // global
	evFDReady                           // fd (epoll data)

	// Surface events. id is the wl_surface object id (SurfaceID).
	evPing               // serial (xdg_wm_base)
	evXdgToplevel        // id, a = width, b = height (before the matching evXdgConfigure)
	evXdgConfigure       // id, serial
	evLayerConfigure     // id, serial, a = width, b = height
	evLockConfigure      // id, serial, a = width, b = height
	evClosed             // id
	evFrame              // id
	evPreferredScale     // id, a = fractional scale in 120ths
	evBufferScale        // id, a = integer factor
	evFeedbackTable      // id, fd = owned descriptor, flags = size
	evFeedbackMainDevice // id, dev
	evFeedbackFormats    // id, name = one chunk of tranche format indices
	evFeedbackDone       // id
	evLocked             // id = lock generation
	evLockFinished       // id = lock generation
	evBadEvent           // id, a = badEvent code: the reader met a malformed event
)

// event is the only thing the reader and the epoll goroutine hand to the
// owner. It holds no pointers except through fixed storage, so queuing one
// never allocates.
type event struct {
	kind    eventKind
	nameLen uint8
	global  uint32
	version uint32
	flags   uint32
	id      uint32 // wl_surface object id for surface events
	serial  uint32
	fd      int32 // epoll data, or a received descriptor owned by the event
	a, b    int32
	dev     uint64
	name    [maxNameLen]byte
}

// hasFD reports whether the event owns a received descriptor that must be
// closed if the event is dropped.
func (e *event) hasFD() bool { return e.kind == evFeedbackTable && e.fd >= 0 }

func (e *event) closeFD() {
	if e.hasFD() {
		_ = unix.Close(int(e.fd))
		e.fd = -1
	}
}

func (e *event) setBytes(b []byte) {
	e.nameLen = uint8(copy(e.name[:], b))
}

func (e *event) setName(s string) {
	e.nameLen = uint8(copy(e.name[:], s))
}

func (e *event) nameBytes() []byte { return e.name[:e.nameLen] }

// queue is the bounded hand-off between producers (the reader goroutine and
// the epoll goroutine) and the owner goroutine, plus the reader pause gate.
//
// A producer facing a full ring waits on cond; Dispatch releases it as it
// drains. A pause request also releases it: while pausing, producers park
// events in held instead of waiting, so neither side can block on the other.
// Parked events are re-posted, in order, by the reader once it resumes.
type queue struct {
	mu   sync.Mutex
	cond sync.Cond // L = &mu; one cond for every state change, all are rare

	ring [ringSize]event
	head int
	n    int

	pausing      bool
	readerParked bool
	readerExit   bool
	stopped      bool
	held         []event
	spare        []event // capacity reused by the next held generation
	fatal        error

	wake chan struct{}
}

func newQueue(pausing bool) *queue {
	q := &queue{pausing: pausing, wake: make(chan struct{}, 1)}
	q.cond.L = &q.mu
	return q
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// post queues ev. It blocks while the ring is full and not pausing.
func (q *queue) post(ev *event) {
	q.mu.Lock()
	for {
		switch {
		case q.stopped:
			q.mu.Unlock()
			ev.closeFD()
			return
		case q.pausing:
			q.held = append(q.held, *ev)
			q.mu.Unlock()
			return
		case q.n < ringSize:
			q.ring[(q.head+q.n)%ringSize] = *ev
			q.n++
			q.mu.Unlock()
			q.signal()
			return
		}
		q.cond.Wait()
	}
}

// pop moves the oldest queued event into ev.
func (q *queue) pop(ev *event) bool {
	q.mu.Lock()
	if q.n == 0 {
		q.mu.Unlock()
		return false
	}
	*ev = q.ring[q.head]
	q.ring[q.head].kind = 0 // ownership of a carried descriptor moved to ev
	q.head = (q.head + 1) % ringSize
	full := q.n == ringSize
	q.n--
	if full {
		q.cond.Broadcast()
	}
	q.mu.Unlock()
	return true
}

// setFatal records the first transport failure; Dispatch returns it once the
// events queued before it are drained.
func (q *queue) setFatal(err error) {
	q.mu.Lock()
	if q.fatal == nil && !q.stopped {
		q.fatal = err
	}
	q.mu.Unlock()
	q.signal()
}

func (q *queue) fatalErr() error {
	q.mu.Lock()
	err := q.fatal
	q.mu.Unlock()
	return err
}

func (q *queue) isStopped() bool {
	q.mu.Lock()
	s := q.stopped
	q.mu.Unlock()
	return s
}

func (q *queue) stop() {
	q.mu.Lock()
	q.stopped = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// setPausing starts or ends a pause.
func (q *queue) setPausing(v bool) {
	q.mu.Lock()
	q.pausing = v
	q.cond.Broadcast()
	q.mu.Unlock()
}

// endPause ends a pause started by setPausing(true). Events producers parked
// meanwhile are moved into the ring, in order, as far as it has room, so that
// everything handled during the pause is visible to the next Dispatch. The
// remainder stays at the front of held; the reader re-posts it when it resumes.
func (q *queue) endPause() {
	q.mu.Lock()
	m := min(len(q.held), ringSize-q.n)
	for i := range m {
		q.ring[(q.head+q.n)%ringSize] = q.held[i]
		q.n++
	}
	if m > 0 {
		q.held = q.held[:copy(q.held, q.held[m:])]
	}
	q.pausing = false
	q.cond.Broadcast()
	q.mu.Unlock()
	if m > 0 {
		q.signal()
	}
}

// awaitParked blocks until the reader is parked or gone.
func (q *queue) awaitParked() {
	q.mu.Lock()
	for !q.readerParked && !q.readerExit {
		q.cond.Wait()
	}
	q.mu.Unlock()
}

// gate runs on the reader goroutine before every read. It parks the reader
// while a pause is requested and re-posts what producers parked meanwhile.
// It reports false when the connection is closing.
func (q *queue) gate() bool {
	q.mu.Lock()
	for q.pausing && !q.stopped {
		if !q.readerParked {
			q.readerParked = true
			q.cond.Broadcast()
		}
		q.cond.Wait()
	}
	wasParked := q.readerParked
	q.readerParked = false
	stopped := q.stopped
	q.mu.Unlock()
	if stopped {
		return false
	}
	if wasParked {
		q.flushHeld()
	}
	return true
}

func (q *queue) flushHeld() {
	q.mu.Lock()
	held := q.held
	q.held = q.spare[:0]
	q.mu.Unlock()
	for i := range held {
		q.post(&held[i])
	}
	q.mu.Lock()
	q.spare = held[:0]
	q.mu.Unlock()
}

// takeHeld returns and clears the parked events. Only valid while no reader
// runs (during setup).
func (q *queue) takeHeld() []event {
	q.mu.Lock()
	held := q.held
	q.held = nil
	q.mu.Unlock()
	return held
}

// closeFDs closes the descriptors owned by events nobody will drain. Only
// valid once both producers have stopped.
func (q *queue) closeFDs() {
	q.mu.Lock()
	for i := range q.n {
		q.ring[(q.head+i)%ringSize].closeFD()
	}
	for i := range q.held {
		q.held[i].closeFD()
	}
	q.mu.Unlock()
}

// length reports how many events are queued.
func (q *queue) length() int {
	q.mu.Lock()
	n := q.n
	q.mu.Unlock()
	return n
}

// pending reports whether events are queued.
func (q *queue) pending() bool {
	q.mu.Lock()
	n := q.n
	q.mu.Unlock()
	return n > 0
}

func (q *queue) readerDone() {
	q.mu.Lock()
	q.readerExit = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
