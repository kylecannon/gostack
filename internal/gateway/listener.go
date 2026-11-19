package gateway

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	// tlsHandshakeRecord is the first byte of every TLS connection: the record
	// type of a handshake.
	tlsHandshakeRecord = 0x16
	// firstByteTimeout is how long a connection may stay silent before its first
	// byte. net/http applies ReadHeaderTimeout from accept as well, so a silent
	// connection costs the same on either path.
	firstByteTimeout = readHeaderTimeout
)

// muxListener serves TLS and cleartext on one listener, as LocalStack's
// TLSMultiplexer does. It reads the first byte of each connection: a TLS
// handshake record gets wrapped in tls.Server, anything else is handed over
// as it is. net/http then speaks HTTP/1.1 or h2 on the TLS side, and HTTP/1.1
// or h2c with prior knowledge on the cleartext side.
//
// The first byte is read in a goroutine per connection, never in Accept, so
// one client that connects and stays silent does not hold up the others.
type muxListener struct {
	inner net.Listener
	tls   *tls.Config

	conns chan net.Conn
	errs  chan error
	done  chan struct{}

	mu      sync.Mutex
	pending map[net.Conn]struct{}
	wg      sync.WaitGroup
	once    sync.Once

	// handedOff, when set, runs in a connection's goroutine right after Accept
	// has taken the connection. Tests use it to hold that goroutine there.
	handedOff func()
}

func newMuxListener(inner net.Listener, cfg *tls.Config) *muxListener {
	l := muxListenerFor(inner, cfg)
	l.wg.Go(l.acceptLoop)

	return l
}

// muxListenerFor builds a muxListener that does not accept yet.
func muxListenerFor(inner net.Listener, cfg *tls.Config) *muxListener {
	return &muxListener{
		inner:   inner,
		tls:     cfg,
		conns:   make(chan net.Conn),
		errs:    make(chan error),
		done:    make(chan struct{}),
		pending: make(map[net.Conn]struct{}),
	}
}

// Accept returns the next connection, already wrapped in tls.Server when the
// client opened with a TLS handshake. The handshake itself runs in net/http's
// connection goroutine.
func (l *muxListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting, closes connections that have not sent their first
// byte and waits for the goroutines that served them.
func (l *muxListener) Close() error {
	var err error

	l.once.Do(func() {
		close(l.done)

		closeErr := l.inner.Close()
		if closeErr != nil {
			err = fmt.Errorf("close listener: %w", closeErr)
		}

		l.closePending()
		l.wg.Wait()
	})

	return err
}

// Addr returns the address of the underlying listener.
func (l *muxListener) Addr() net.Addr {
	return l.inner.Addr()
}

// closePending closes the connections still waiting for their first byte.
func (l *muxListener) closePending() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for conn := range l.pending {
		_ = conn.Close()
	}
}

func (l *muxListener) acceptLoop() {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			select {
			case l.errs <- err:
			case <-l.done:
				return
			}

			if errors.Is(err, net.ErrClosed) {
				return
			}

			// net/http backs off on a temporary error and gives up on the rest.
			continue
		}

		if !l.add(conn) {
			continue
		}

		l.wg.Go(func() { l.classify(conn) })
	}
}

func (l *muxListener) classify(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(firstByteTimeout))

	reader := bufio.NewReader(conn)

	first, err := reader.Peek(1)

	// conn leaves pending before it can reach Accept. Once net/http serves it,
	// a Close of this listener (net/http's own, when Accept fails) must not cut
	// the request in flight; only the server's drain may.
	l.remove(conn)

	if err != nil {
		_ = conn.Close()

		return
	}

	_ = conn.SetReadDeadline(time.Time{})

	var out net.Conn = &peekConn{Conn: conn, reader: reader}
	if first[0] == tlsHandshakeRecord {
		out = tls.Server(out, l.tls)
	}

	select {
	case l.conns <- out:
		if l.handedOff != nil {
			l.handedOff()
		}
	case <-l.done:
		_ = out.Close()
	}
}

// add records conn as waiting for its first byte, so Close can end the wait. It
// closes conn and reports false when Close has already run.
func (l *muxListener) add(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	select {
	case <-l.done:
		_ = conn.Close()

		return false
	default:
		l.pending[conn] = struct{}{}

		return true
	}
}

func (l *muxListener) remove(conn net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.pending, conn)
}

// peekConn is a connection whose first byte has been read into reader.
type peekConn struct {
	net.Conn

	reader *bufio.Reader
}

func (c *peekConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// CloseWrite lets net/http half-close the connection as it would a *net.TCPConn.
func (c *peekConn) CloseWrite() error {
	closer, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return nil
	}

	return closer.CloseWrite() //nolint:wrapcheck // net/http reads the raw error of the connection it half-closes.
}
