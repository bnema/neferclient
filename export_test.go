package neferclient

import (
	"context"
	"net"
)

// Test access for the external neferclient_test package, which imports the
// generated mocks (they import this package, so in-package tests cannot).

const RingSize = ringSize

func ConnectConn(ctx context.Context, sock net.Conn) (*Conn, error) { return connectConn(ctx, sock) }

func SocketPath(name string) (string, error) { return socketPath(name) }

func (c *Conn) QueueLen() int { return c.q.length() }

// MarkPresentable stands in for the configure and feedback rounds, which the
// allocation test does not drive over the wire.
func (s *Surface) MarkPresentable(w, h int32) {
	s.width, s.height = w, h
	s.configured, s.frameReady, s.fbDone = true, true, true
}

func (s *Surface) SurfaceObjectID() uint32 { return s.surf.ID() }

// SkipFrameRequests makes Present omit wl_surface.frame.
func (s *Surface) SkipFrameRequests() { s.req = wireRequests{s: s, skipFrame: true} }

// ResetFrame re-arms Present when frame requests are skipped.
func (s *Surface) ResetFrame() { s.frameReady = true }

// UseWireRequests restores the full request path.
func (s *Surface) UseWireRequests() { s.req = wireRequests{s: s} }
