package s3

import (
	"bufio"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
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

const headerContentLength = "Content-Length"

// edgeSpace reports whether a header value starts or ends with a space or tab,
// which net/http would trim.
func edgeSpace(value string) bool {
	if value == "" {
		return false
	}

	first, last := value[0], value[len(value)-1]

	return first == ' ' || first == '\t' || last == ' ' || last == '\t'
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
func (w *responseWriter) takeConn(code int) bool {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return false
	}

	header := w.Header()
	t := &takenConn{conn: conn, out: rw.Writer, length: -1, noBody: bodiless(code), discard: w.method == http.MethodHead}

	n, parseErr := strconv.ParseInt(header.Get(headerContentLength), 10, 64)
	if parseErr == nil && n >= 0 && !t.noBody {
		t.length = n
	}

	t.err = writeRawHead(t.out, code, header)
	w.raw = t

	return true
}

// bodiless reports whether a response with this status has no body and no
// Content-Length (RFC 9110 6.4.1), which is how net/http treats it.
func bodiless(code int) bool {
	return code == http.StatusNoContent || code == http.StatusNotModified
}

// writeRawHead writes the status line and the headers of a response sent with
// Connection: close. A body whose length the handler did not declare ends when
// the connection closes. Names net/http would refuse, and values with control
// characters, are dropped as net/http drops them; a line break in a value
// becomes a space.
func writeRawHead(out *bufio.Writer, code int, header http.Header) error {
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
			if validFieldValue(value) {
				_, _ = out.WriteString(name + ": " + lineBreaksToSpace(value) + "\r\n")
			}
		}
	}

	_, _ = out.WriteString("Connection: close\r\n\r\n")

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

// validFieldValue reports whether net/http would send value: no control
// character but a tab, and no DEL. A CR or LF is handled by the caller.
func validFieldValue(value string) bool {
	for i := range len(value) {
		c := value[i]
		if (c < ' ' && c != '\t' && c != '\r' && c != '\n') || c == 0x7f {
			return false
		}
	}

	return true
}

// takenConn is a hijacked connection that a response is written to by hand. It
// is allocated only when one is taken, so an ordinary response does not carry it.
type takenConn struct {
	conn    net.Conn
	out     *bufio.Writer
	err     error
	length  int64 // bytes still owed to a declared Content-Length, or -1.
	noBody  bool  // a 204 or 304: no body, no Content-Length.
	discard bool  // a HEAD response: Content-Length, no body.
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

// finish flushes what is left and closes a taken connection. serve calls it when
// the handler returns, so the client sees the end of the body.
func (w *responseWriter) finish() {
	if w.raw == nil {
		return
	}

	_ = w.raw.out.Flush()
	_ = w.raw.conn.Close()
	w.raw = nil
}

// FlushError sends buffered data to the client. On a taken connection that is the
// buffer here; otherwise it is the underlying writer's.
func (w *responseWriter) FlushError() error {
	if w.raw != nil {
		return w.raw.out.Flush()
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
