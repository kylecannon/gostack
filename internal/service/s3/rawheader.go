package s3

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// net/http trims leading and trailing spaces and tabs from every header value it
// writes over HTTP/1.x (Header.writeSubset calls textproto.TrimString), and S3
// sends such a value as it stores it: a metadata value ending in a tab is the
// case LocalStack's test_post_object_with_unicode_metadata pins. HTTP/2 does not
// trim. A response that carries one on an HTTP/1.x connection is therefore
// written by hand, through the connection, byte for byte.
//
// Only that response takes the path. Every other response keeps net/http's own
// write, with its keep-alive and its chunked encoding.

const (
	headerContentLength = "Content-Length"

	// rawBufferLimit is how much of a body of unknown length a taken response
	// holds back, so that a body that ends within it is sent with its
	// Content-Length, as net/http does for a short body. A longer body is sent
	// chunked.
	rawBufferLimit = 64 << 10

	// lingerFor is how long a taken connection keeps reading after the response,
	// when the request body was not read to its end. It is net/http's
	// rstAvoidanceDelay: closing a connection with unread request bytes sends a
	// reset that can reach the client before the response does.
	lingerFor = 500 * time.Millisecond

	// drainLimit is the longest Provider.Close waits for the taken responses
	// still being written. The services' Close budget is what ends the wait in
	// a running process, because an overrunning Close is abandoned; this only
	// keeps a stuck client from holding Close for good.
	drainLimit = 10 * time.Second
)

// edgeSpace reports whether a header value starts or ends with a space or tab,
// which net/http would trim.
func edgeSpace(value string) bool {
	if value == "" {
		return false
	}

	first, last := value[0], value[len(value)-1]

	return first == ' ' || first == '\t' || last == ' ' || last == '\t'
}

// inflight counts the taken responses being written. http.Server.Shutdown does
// not track a hijacked connection, so the provider does, and Close waits for
// them before it lets go of the bodies they read. A nil *inflight tracks nothing.
type inflight struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// enter registers a response about to take its connection. It reports false once
// drain has begun, and the response is then written by net/http.
func (f *inflight) enter() bool {
	if f == nil {
		return true
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return false
	}

	f.wg.Add(1)

	return true
}

// leave ends what enter began.
func (f *inflight) leave() {
	if f != nil {
		f.wg.Done()
	}
}

// drain refuses new responses and waits for the registered ones, for at most
// limit.
func (f *inflight) drain(limit time.Duration) {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()

	done := make(chan struct{})

	go func() {
		f.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
	}
}

// statusNoter is implemented by the gateway's status writer. A taken response
// never reaches its WriteHeader, so the response tells it the status instead.
type statusNoter interface{ NoteStatus(code int) }

// noteStatus tells the gateway's writer, found through Unwrap, the status of a
// response that is about to take the connection.
func noteStatus(w http.ResponseWriter, code int) {
	for {
		if noter, ok := w.(statusNoter); ok {
			noter.NoteStatus(code)

			return
		}

		next, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}

		w = next.Unwrap()
	}
}

// trackedBody notes whether the request body was read to its end, which tells
// finish whether the client may still be sending. It lives inside the
// responseWriter, so tracking costs no allocation.
type trackedBody struct {
	io.ReadCloser

	left int64 // bytes still owed to a declared Content-Length, or -1.
	done bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.left > 0 {
		b.left -= int64(n)
	}

	if errors.Is(err, io.EOF) {
		b.done = true
	}

	return n, err
}

// unread reports whether request bytes may still be on their way.
func (b *trackedBody) unread() bool {
	return b.ReadCloser != nil && !b.done && b.left != 0
}

// trackBody has r's body noted in w, when r has one.
func (w *responseWriter) trackBody(r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}

	w.body = trackedBody{ReadCloser: r.Body, left: r.ContentLength}
	r.Body = &w.body
}

// takeConn tries to take the connection of an HTTP/1.x response whose headers
// net/http would alter, and writes the status line and the headers to it. It
// reports false, with nothing written, when the connection cannot be taken, and
// the caller then writes the response as it always did.
//
// The headers are complete when this runs: the gateway added x-localstack and
// x-gostack before the handler, and WriteHeader has applied the S3 spelling. The
// ListBuckets and CreateBucket CORS headers that defaultCORSWriter adds below
// this writer are not applied, and neither response carries a header value that
// leads or ends in white space.
//
// A body of unknown length keeps its head back until the length is known: the
// end of the body, or more of it than rawBufferLimit.
func (w *responseWriter) takeConn(code int) bool {
	if !w.inflight.enter() {
		return false
	}

	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		w.inflight.leave()

		return false
	}

	// The gateway's writer never sees a taken response, so it learns the status
	// here: a 4xx must not mark state dirty, and a panic after this point must
	// abort the connection instead of writing a 500 into it.
	noteStatus(w.ResponseWriter, code)

	header := w.Header()
	t := &takenConn{
		conn: conn, out: rw.Writer, length: -1, code: code,
		noBody: bodiless(code), discard: w.method == http.MethodHead, http11: w.http11,
	}

	n, parseErr := strconv.ParseInt(header.Get(headerContentLength), 10, 64)
	declared := parseErr == nil && n >= 0 && !t.noBody

	if declared {
		t.length = n
	}

	if declared || t.noBody || t.discard {
		t.err = writeRawHead(t.out, code, header, "")
		t.sent = true
	} else {
		t.header = header.Clone()
	}

	w.raw = t

	return true
}

// bodiless reports whether a response with this status has no body and no
// Content-Length (RFC 9110 6.4.1), which is how net/http treats it.
func bodiless(code int) bool {
	return code == http.StatusNoContent || code == http.StatusNotModified
}

// writeRawHead writes the status line and the headers of a response sent with
// Connection: close, then framing, the Content-Length or Transfer-Encoding line
// of a response whose handler did not declare a length. Names net/http would
// refuse are dropped. Values are sent as net/http sends them, except that it
// also trims white space, which this path exists to avoid: a CR or LF becomes a
// space, so a value cannot end its line and start another header, and every other
// byte goes out as it is.
func writeRawHead(out *bufio.Writer, code int, header http.Header, framing string) error {
	text := http.StatusText(code)
	if text == "" {
		text = "status code " + strconv.Itoa(code)
	}

	_, _ = out.WriteString("HTTP/1.1 " + strconv.Itoa(code) + " " + text + "\r\n")

	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}

	slices.Sort(names)

	if _, dated := header["Date"]; !dated {
		_, _ = out.WriteString("Date: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n")
	}

	for _, name := range names {
		if skipRawHeader(name) || (name == headerContentLength && bodiless(code)) {
			continue
		}

		for _, value := range header[name] {
			_, _ = out.WriteString(name + ": " + lineBreaksToSpace(value) + "\r\n")
		}
	}

	_, _ = out.WriteString(framing + "Connection: close\r\n\r\n")

	return out.Flush()
}

// skipRawHeader reports whether a header is written by writeRawHead itself or
// is one net/http would not send.
func skipRawHeader(name string) bool {
	if name == "" || name == "Connection" || name == "Transfer-Encoding" || name == "Trailer" {
		return true
	}

	for i := range len(name) {
		if !isTokenChar(name[i]) {
			return true
		}
	}

	return false
}

// isTokenChar reports whether c may appear in a header name (RFC 9110 token).
func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}

	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// takenConn is a hijacked connection that a response is written to by hand. It
// is allocated only when one is taken, so an ordinary response does not carry it.
type takenConn struct {
	conn    net.Conn
	out     *bufio.Writer
	err     error
	code    int
	length  int64 // bytes still owed to a declared Content-Length, or -1.
	noBody  bool  // a 204 or 304: no body, no Content-Length.
	discard bool  // a HEAD response: Content-Length, no body.
	http11  bool  // the client reads chunked bodies; an HTTP/1.0 client does not.

	// sent is set once the head is written. Until then header holds the head of
	// a response of unknown length, and buf the body held back for its length.
	sent    bool
	header  http.Header
	buf     []byte
	chunked bool
}

// write writes body bytes, with net/http's answers for a body the response may
// not have or one that outruns its Content-Length.
func (t *takenConn) write(p []byte) (int, error) {
	switch {
	case t.err != nil:
		return 0, t.err
	case t.noBody:
		return 0, http.ErrBodyNotAllowed
	case t.discard:
		return len(p), nil
	}

	if !t.sent {
		if len(t.buf)+len(p) <= rawBufferLimit {
			t.buf = append(t.buf, p...)

			return len(p), nil
		}

		t.commitStreaming()

		if t.err != nil {
			return 0, t.err
		}
	}

	if t.chunked {
		t.writeChunk(p)

		return len(p), t.err
	}

	var overrun error

	if t.length >= 0 && int64(len(p)) > t.length {
		p, overrun = p[:t.length], http.ErrContentLength
	}

	n, err := t.out.Write(p)
	if t.length >= 0 {
		t.length -= int64(n)
	}

	if err != nil {
		t.err = err

		return n, err
	}

	return n, overrun
}

// commitLength writes the head of a response of unknown length whose body ended
// within what was held back, with that length, and then the body.
func (t *takenConn) commitLength() {
	t.sent = true
	t.err = writeRawHead(t.out, t.code, t.header, "Content-Length: "+strconv.Itoa(len(t.buf))+"\r\n")

	if t.err == nil {
		_, t.err = t.out.Write(t.buf)
	}

	t.header, t.buf = nil, nil
}

// commitStreaming writes the head of a response of unknown length that must send
// before its end, and then what was held back. An HTTP/1.1 client gets it
// chunked. An HTTP/1.0 client cannot read chunks, so its body ends when the
// connection closes, as net/http answers it.
func (t *takenConn) commitStreaming() {
	t.sent = true
	t.chunked = t.http11

	framing := ""
	if t.chunked {
		framing = "Transfer-Encoding: chunked\r\n"
	}

	t.err = writeRawHead(t.out, t.code, t.header, framing)

	if t.chunked {
		t.writeChunk(t.buf)
	} else if t.err == nil {
		_, t.err = t.out.Write(t.buf)
	}

	t.header, t.buf = nil, nil
}

// writeChunk writes p as one chunk of a chunked body.
func (t *takenConn) writeChunk(p []byte) {
	if t.err != nil || len(p) == 0 {
		return
	}

	_, t.err = t.out.WriteString(strconv.FormatInt(int64(len(p)), 16) + "\r\n")
	if t.err == nil {
		_, t.err = t.out.Write(p)
	}

	if t.err == nil {
		_, t.err = t.out.WriteString("\r\n")
	}
}

// flush sends what is buffered. A response of unknown length that must send
// something before its end can only be chunked.
func (t *takenConn) flush() error {
	if !t.sent {
		t.commitStreaming()
	}

	if t.err != nil {
		return t.err
	}

	return t.out.Flush()
}

// finish is the end of the response: it completes a response that still holds
// its head, sends the last chunk of a chunked one and flushes.
func (t *takenConn) finish() {
	if !t.sent {
		t.commitLength()
	} else if t.chunked && t.err == nil {
		_, t.err = t.out.WriteString("0\r\n\r\n")
	}

	_ = t.out.Flush()
}

// linger half-closes the connection and reads what the client is still sending
// for a short while, as net/http does before it closes: a close with unread
// bytes sends a reset, and the client can lose the response it has not yet read.
// A connection that cannot be half-closed is left to close.
func (t *takenConn) linger() {
	closer, ok := t.conn.(interface{ CloseWrite() error })
	if !ok || closer.CloseWrite() != nil {
		return
	}

	_ = t.conn.SetReadDeadline(time.Now().Add(lingerFor))
	_, _ = io.Copy(io.Discard, t.conn)
}

// finish completes a taken response and closes its connection. serve calls it
// when the handler returns, so the client sees the end of the body. A client that
// may still be sending gets the linger before the close.
func (w *responseWriter) finish() {
	raw := w.raw
	if raw == nil {
		return
	}

	w.raw = nil

	defer w.inflight.leave()

	raw.finish()

	if w.body.unread() {
		raw.linger()
	}

	_ = raw.conn.Close()
}

// abort closes a taken response that serve left without finishing, because its
// handler panicked. Nothing more is sent, so the client sees a broken response,
// not a short one that looks complete. It does nothing once finish has run.
func (w *responseWriter) abort() {
	raw := w.raw
	if raw == nil {
		return
	}

	w.raw = nil

	_ = raw.conn.Close()

	w.inflight.leave()
}

// FlushError sends buffered data to the client. On a taken connection that is the
// buffer here; otherwise it is the underlying writer's.
func (w *responseWriter) FlushError() error {
	if w.raw != nil {
		return w.raw.flush()
	}

	return http.NewResponseController(w.ResponseWriter).Flush() //nolint:wrapcheck // the underlying writer's own error.
}

// lineBreaksToSpace replaces a CR or LF with a space, as net/http does, so a
// value cannot end its line and start another header.
func lineBreaksToSpace(value string) string {
	if !strings.ContainsAny(value, "\r\n") {
		return value
	}

	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}
