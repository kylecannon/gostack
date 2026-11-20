package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// memListener is a net.Listener with no socket: each connection is a net.Pipe.
// A goroutine blocked in Accept or on such a connection is durably blocked, so
// a test that serves over it can run in a testing/synctest bubble and have the
// shutdown's deadlines on the fake clock. A real socket would stop that clock.
type memListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMemListener() *memListener {
	return &memListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.closed) })

	return nil
}

func (*memListener) Addr() net.Addr { return memAddr{} }

// dial returns the client end of a new connection to the listener.
func (l *memListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()

	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		_ = client.Close()
		_ = server.Close()

		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()

		return nil, fmt.Errorf("dial: %w", context.Cause(ctx))
	}
}

// client is an HTTP client whose every connection goes to the listener, and
// that keeps none open after a response, so a bubble can end.
func (l *memListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return l.dial(ctx)
		},
		DisableKeepAlives: true,
	}}
}

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }
