package gateway

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// unreadDrain is how long the server waits, after a handler that answered
// without reading all of its request's body returns, for the rest of that
// body before it closes the connection. A client that sends its whole body
// sends what is left within it, so the answer is not lost to a reset; one that
// stopped sending gives back its connection and goroutine.
const unreadDrain = time.Second

// unreadBody follows whether a request's body was read to its end, so an
// answer to a request whose body was not can close the connection
// (closeUnread, drainUnread). Without it net/http reads what is left of the
// body, up to 256 KiB, before it writes the answer and again after, with no
// deadline: an S3 PutObject to a bucket that does not exist, which is
// answered before its body is read, waited on a client that had stopped for
// as long as the client kept the connection (SQ-320). It is a field of
// call, so following a body allocates nothing.
//
// Read runs on whatever goroutine reads the body, and remains on the one that
// writes the answer, so its state is atomic.
type unreadBody struct {
	src    io.ReadCloser
	length int64 // the stated length, or -1
	read   atomic.Int64
	ended  atomic.Bool
}

func (b *unreadBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)

	read := b.read.Add(int64(n))
	if errors.Is(err, io.EOF) || (b.length >= 0 && read >= b.length) {
		b.ended.Store(true)
	}

	return n, err
}

func (b *unreadBody) Close() error {
	return b.src.Close()
}

// follow makes b follow r's body, for an HTTP/1.x request from a listener that
// has one. On HTTP/2 net/http turns a handler's Connection: close into a GOAWAY
// that ends every stream on the connection, and an unread HTTP/2 body ends
// with its stream, not the connection, so an HTTP/2 body is not followed. Nor is one of length zero,
// which HTTP/2 gives a request that has no body. A chunked body states no
// length (-1) and is followed.
func (b *unreadBody) follow(r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody || r.ProtoMajor >= 2 || r.ContentLength == 0 {
		return
	}

	b.src, b.length = r.Body, r.ContentLength
	r.Body = b
}

// remains reports whether the request has a body that was not read to its end.
func (b *unreadBody) remains() bool {
	return b != nil && b.src != nil && !b.ended.Load()
}

// closeUnread marks an answer that starts while its request's body remains
// unread to close the connection, so net/http does not read the rest of the
// body before it writes the answer.
func (w *statusWriter) closeUnread() {
	if w.body.remains() {
		w.Header().Set("Connection", "close")
	}
}

// drainUnread runs when the handler has returned. For a body that remains
// unread, it closes the connection after the answer (when nothing was written
// yet) and bounds the read net/http makes of the rest of the body by
// unreadDrain.
func (w *statusWriter) drainUnread(rw http.ResponseWriter) {
	if !w.body.remains() {
		return
	}

	if w.status == 0 {
		w.Header().Set("Connection", "close")
	}

	// A hijacked connection, or a writer that has no deadline, has nothing
	// for net/http to read.
	_ = http.NewResponseController(rw).SetReadDeadline(time.Now().Add(unreadDrain))
}
