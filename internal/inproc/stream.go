package inproc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
)

// StreamResponse is what the handler answered to Stream. Body yields the
// response body as the handler writes it, and the caller must close it.
type StreamResponse struct {
	Status int
	// Header is the response head as the handler sent it with its status.
	Header http.Header
	Body   io.ReadCloser
}

// OK reports whether the status is a 2xx.
func (r *StreamResponse) OK() bool {
	return ok(r.Status)
}

// Stream serves call through the handler with the request body read from body,
// size bytes long or of unknown length when size is negative, and returns as
// soon as the handler has fixed the response head. A nil body sends call.Body
// instead. Neither body is held whole: the handler reads the request body as
// it goes, and each write of the response waits for the caller to read it.
//
// The handler runs on a goroutine of its own until it returns. Closing Body, or
// the end of ctx, cancels the request and fails the handler's next write, and
// Close returns only once the handler has returned. A body read to its end
// means the handler has returned already. A handler that panics or calls
// runtime.Goexit after the head is fixed ends Body with ErrAborted; one that
// does before is the error of Stream.
func (c *Client) Stream(ctx context.Context, call Call, body io.Reader, size int64) (*StreamResponse, error) {
	if !c.Bound() {
		return nil, ErrNotBound
	}

	if body == nil {
		body, size = bytes.NewReader(call.Body), int64(len(call.Body))
	}

	own, cancel := context.WithCancel(context.Background())

	req, err := newRequest(own, call, body, size) //nolint:contextcheck // own deliberately inherits none of ctx's values.
	if err != nil {
		cancel()

		return nil, err
	}

	handler := *c.handler.Load()
	pr, pw := io.Pipe()
	ready := make(chan struct{})
	s := &stream{pr: pr, cancel: cancel, done: make(chan struct{})}
	w := &writer{header: make(http.Header), body: pw, onCommit: func() { close(ready) }}

	// The end of ctx fails the handler's next write, and the caller's next read
	// with ctx's error. A body already written whole keeps its io.EOF: the first
	// close of the write end is the one that counts.
	s.stop = context.AfterFunc(ctx, func() {
		cancel()
		pw.CloseWithError(ctx.Err())
	})

	// failed is written before the head is fixed and read after it, so the
	// close of ready orders the two.
	var failed error

	go func() {
		defer close(s.done)

		// A handler that calls runtime.Goexit never returns, and leaves err as
		// errExited for the deferred tail, which runs either way.
		err := errExited

		defer func() {
			if !w.committed {
				failed = err
				w.finish()
			}

			_ = pw.CloseWithError(err)
		}()

		err = serve(handler, w, req)
	}()

	<-ready

	if failed != nil {
		_ = s.Close()

		return nil, failed
	}

	return &StreamResponse{Status: w.status, Header: w.sent, Body: s}, nil
}

// stream is the body of a StreamResponse: the read end of the pipe the handler
// writes into.
type stream struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
	stop   func() bool
	done   chan struct{}
	once   sync.Once
}

func (s *stream) Read(p []byte) (int, error) {
	return s.pr.Read(p)
}

// Close cancels the request, fails the handler's next write and waits for it to
// return. It is safe to call more than once.
func (s *stream) Close() error {
	s.once.Do(func() {
		s.stop()
		s.cancel()
		_ = s.pr.Close()
		<-s.done
	})

	return nil
}
