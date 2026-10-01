package neferclient

import (
	"errors"
	"fmt"

	"github.com/bnema/go-wayland-bindings/client/extsessionlock"
	"github.com/bnema/wlturbo/wl"
)

// Lock is an ext-session-lock-v1 session lock. It reports [Handler.Locked]
// once every output is protected and [Handler.LockFinished] if the compositor
// refuses or ends it. Closing the connection or destroying surfaces never
// unlocks; only [Lock.Unlock] after Locked does. Owner goroutine only.
type Lock struct {
	c        *Conn
	gen      uint32
	obj      *extsessionlock.ExtSessionLock
	locked   bool
	finished bool
	done     bool
}

// Lock requests a session lock. At most one lock is live per connection.
// The compositor answers through Handler.Locked or Handler.LockFinished.
func (c *Conn) Lock() (*Lock, error) {
	if c.closed {
		return nil, ErrClosed
	}
	if c.curLock != nil {
		return nil, errors.New("neferclient: session lock already acquired")
	}
	if err := c.ensureCore(); err != nil {
		return nil, err
	}
	if err := c.ensureLockManager(); err != nil {
		return nil, err
	}
	c.pause() // locked/finished may follow the request immediately
	defer c.resume()
	c.nextLock++
	l := &Lock{c: c, gen: c.nextLock}
	q, gen := c.q, l.gen
	obj, err := c.g.lockMgr.Lock()
	if err != nil {
		return nil, err
	}
	obj.OnLocked(func() {
		e := event{kind: evLocked, id: gen}
		q.post(&e)
	})
	obj.OnFinished(func() {
		e := event{kind: evLockFinished, id: gen}
		q.post(&e)
	})
	l.obj = obj
	c.curLock = l
	return l, nil
}

// Locked reports whether the compositor confirmed the lock (applied by
// Dispatch) and it has not finished.
func (l *Lock) Locked() bool { return l.locked && !l.finished && !l.done }

// NewSurface creates the lock surface covering the output with the given
// registry name (see [Output.Global]). It becomes presentable after the first
// Handler.Configure; a lock surface is always opaque and takes all input.
func (l *Lock) NewSurface(output uint32) (*Surface, error) {
	c := l.c
	if c.closed {
		return nil, ErrClosed
	}
	if l.done || l.finished {
		return nil, errors.New("neferclient: lock ended")
	}
	e := c.entry(output)
	if e == nil || !e.done {
		return nil, fmt.Errorf("neferclient: lock output %d unavailable", output)
	}
	for _, other := range c.surfaces {
		if other.lock == l && other.output == output {
			return nil, fmt.Errorf("neferclient: output %d already has a lock surface", output)
		}
	}
	c.pause()
	defer c.resume()
	s, err := c.newSurface(roleLock, 0, 0)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.destroy()
		}
	}()
	if s.lockSurf, err = l.obj.GetLockSurface(s.surf, e.proxy); err != nil {
		return nil, err
	}
	s.lock, s.output = l, output
	q, sid := c.q, uint32(s.id)
	s.lockSurf.OnConfigure(func(serial, w, h uint32) {
		ev := configureEvent(evLockConfigure, sid, serial, w, h)
		q.post(&ev)
	})
	// ext-session-lock forbids the initial empty commit: the configure comes
	// with get_lock_surface and the first commit carries a buffer.
	if err = s.finishSurface(); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// Unlock sends unlock_and_destroy and roundtrips so the compositor processed
// it before the caller closes anything. It is only valid after Handler.Locked.
// The lock surfaces must be closed afterwards (or before: both are legal).
func (l *Lock) Unlock() error {
	if l.done {
		return errors.New("neferclient: lock already destroyed")
	}
	if !l.Locked() {
		return errors.New("neferclient: unlock before the locked event")
	}
	if err := l.obj.UnlockAndDestroy(); err != nil {
		return err
	}
	l.done = true
	l.c.curLock = nil
	return l.c.Roundtrip()
}

// Close destroys a lock that never reached Locked (refused or abandoned). A
// locked session is only ended by Unlock, so Close fails once Locked was
// applied. Because the compositor may have sent locked before it processes the
// destroy request (which would then be a protocol error), Close first
// roundtrips and looks through the queued events: if a locked event for this
// lock is already queued it is treated as applied (Handler.Locked is still
// delivered by the next Dispatch) and Close fails with the same error.
func (l *Lock) Close() error {
	if l.done {
		return nil
	}
	if !l.locked && !l.finished {
		if err := l.c.Roundtrip(); err != nil {
			return err
		}
		l.locked = l.locked || l.c.q.hasLocked(l.gen)
	}
	if l.locked && !l.finished {
		return errors.New("neferclient: lock is locked: use Unlock")
	}
	l.done = true
	l.c.curLock = nil
	var err error
	if l.obj != nil {
		err = l.obj.Destroy()
	}
	return err
}

var _ wl.Proxy = (*frameCallback)(nil)
