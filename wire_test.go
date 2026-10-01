package neferclient_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/neferclient"
	"golang.org/x/sys/unix"
)

// wireServer speaks just enough of the compositor side of the Wayland wire
// protocol over a socketpair to drive Conn: registry globals, sync callbacks,
// wl_output binds and caller-scripted events. It is the protocol peer, not a
// double of one of our own interfaces.
type wireServer struct {
	t    *testing.T
	conn *net.UnixConn

	mu       sync.Mutex
	cond     *sync.Cond
	registry uint32
	globals  []wireGlobal
	bound    map[uint32]boundObject // global name -> bound object
	outputs  map[uint32]outputSpec
	hook     func(obj uint32, op uint16, body []byte) // other requests, if set
}

type wireGlobal struct {
	name    uint32
	iface   string
	version uint32
}

type boundObject struct {
	id      uint32
	version uint32
}

// outputSpec is what the server sends right after an output bind.
type outputSpec struct {
	name          string
	scale         int32
	width, height int32
}

func appendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

func appendStr(b []byte, s string) []byte {
	n := len(s) + 1
	b = appendU32(b, uint32(n))
	b = append(b, s...)
	b = append(b, 0)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func frame(obj uint32, op uint16, body []byte) []byte {
	b := appendU32(nil, obj)
	b = appendU32(b, uint32(8+len(body))<<16|uint32(op))
	return append(b, body...)
}

// socketPair returns the client end as a net.Conn and the server end.
func socketPair(t *testing.T) (net.Conn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	client, server := mk(fds[0]), mk(fds[1])
	t.Cleanup(func() { _ = server.Close() })
	return client, server.(*net.UnixConn)
}

// startWireServer serves a socketpair. outputs maps an announced wl_output
// global to the description sent when it is bound.
func startWireServer(t *testing.T, globals []wireGlobal, outputs map[uint32]outputSpec) (net.Conn, *wireServer) {
	t.Helper()
	client, srvConn := socketPair(t)
	s := &wireServer{t: t, conn: srvConn, globals: globals, outputs: outputs,
		bound: map[uint32]boundObject{}}
	s.cond = sync.NewCond(&s.mu)
	go s.serve()
	return client, s
}

func (s *wireServer) write(b []byte) {
	if _, err := s.conn.Write(b); err != nil && !isClosed(err) {
		s.t.Errorf("wire server write: %v", err)
	}
}

func isClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, unix.EPIPE) || errors.Is(err, unix.ECONNRESET)
}

func (s *wireServer) serve() {
	var hdr [8]byte
	var buf []byte // reused: the peer must not allocate while draining
	for {
		if _, err := io.ReadFull(s.conn, hdr[:]); err != nil {
			return
		}
		obj := binary.LittleEndian.Uint32(hdr[0:4])
		w := binary.LittleEndian.Uint32(hdr[4:8])
		size, op := int(w>>16), uint16(w)
		if cap(buf) < size-8 {
			buf = make([]byte, size-8)
		}
		body := buf[:size-8]
		if _, err := io.ReadFull(s.conn, body); err != nil {
			return
		}
		s.handle(obj, op, body)
	}
}

func (s *wireServer) handle(obj uint32, op uint16, body []byte) {
	s.mu.Lock()
	reg := s.registry
	hook := s.hook
	s.mu.Unlock()
	if hook != nil && obj != 1 && obj != reg {
		hook(obj, op, body) // every request on a client-created or bound object
	}
	switch {
	case obj == 1 && op == 1: // get_registry
		id := binary.LittleEndian.Uint32(body)
		s.mu.Lock()
		s.registry = id
		globals := s.globals
		s.mu.Unlock()
		for _, g := range globals {
			s.announce(g)
		}
	case obj == 1 && op == 0: // sync
		id := binary.LittleEndian.Uint32(body)
		s.write(frame(id, 0, appendU32(nil, 0)))
		s.write(frame(1, 1, appendU32(nil, id)))
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	case obj == reg && op == 0: // bind
		name := binary.LittleEndian.Uint32(body)
		n := int(binary.LittleEndian.Uint32(body[4:]))
		off := 8 + (n+3)&^3
		version := binary.LittleEndian.Uint32(body[off:])
		id := binary.LittleEndian.Uint32(body[off+4:])
		s.mu.Lock()
		s.bound[name] = boundObject{id: id, version: version}
		spec, isOutput := s.outputs[name]
		s.cond.Broadcast()
		s.mu.Unlock()
		if isOutput {
			s.describe(id, version, spec)
		}
	case op == 0 && len(body) == 0: // wl_output.release (destructor, no args)
		s.write(frame(1, 1, appendU32(nil, obj)))
	}
}

func (s *wireServer) announce(g wireGlobal) {
	b := appendU32(nil, g.name)
	b = appendStr(b, g.iface)
	b = appendU32(b, g.version)
	s.mu.Lock()
	reg := s.registry
	s.mu.Unlock()
	s.write(frame(reg, 0, b))
}

func (s *wireServer) remove(name uint32) {
	s.mu.Lock()
	reg := s.registry
	s.mu.Unlock()
	s.write(frame(reg, 1, appendU32(nil, name)))
}

// describe sends mode, scale, name (version >= 4) and done for an output.
func (s *wireServer) describe(id, version uint32, o outputSpec) {
	mode := appendU32(nil, 3) // current|preferred
	mode = appendU32(mode, uint32(o.width))
	mode = appendU32(mode, uint32(o.height))
	mode = appendU32(mode, 60000)
	s.write(frame(id, 1, mode))
	s.write(frame(id, 3, appendU32(nil, uint32(o.scale))))
	if version >= 4 {
		s.write(frame(id, 4, appendStr(nil, o.name)))
	}
	s.write(frame(id, 2, nil))
}

// waitBound waits until the client bound global and returns the object.
func (s *wireServer) waitBound(global uint32) boundObject {
	s.t.Helper()
	deadline := time.AfterFunc(5*time.Second, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer deadline.Stop()
	start := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if b, ok := s.bound[global]; ok {
			return b
		}
		if time.Since(start) > 5*time.Second {
			s.t.Fatalf("global %d never bound", global)
		}
		s.cond.Wait()
	}
}

func (s *wireServer) closeConn() { _ = s.conn.Close() }

// connectWire connects a Conn to the wire server.
func connectWire(t *testing.T, globals []wireGlobal, outputs map[uint32]outputSpec) (*neferclient.Conn, *wireServer) {
	t.Helper()
	client, srv := startWireServer(t, globals, outputs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := neferclient.ConnectConn(ctx, client)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

const outputIface = wayland.OutputInterface
