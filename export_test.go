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
