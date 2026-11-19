package s3_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	crossOrigin = "http://x.example"
	// exposeEdge is an ExposeHeader that ends in a space. The CORS headers a
	// cross-origin request to the bucket then carries include a value net/http
	// would trim, so that response is written through the connection.
	exposeEdge = "x-amz-meta-a "
)

// putExposeEdgeCORS gives /bucket a CORS rule whose ExposeHeader ends in a
// space, which sends every cross-origin response of the bucket down the path
// that writes the connection itself. The bucket must exist.
func putExposeEdgeCORS(t *testing.T, srv *httptest.Server) {
	t.Helper()

	config := []byte(`<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin>` +
		`<AllowedMethod>PUT</AllowedMethod><AllowedMethod>GET</AllowedMethod><AllowedMethod>DELETE</AllowedMethod>` +
		`<ExposeHeader>` + exposeEdge + `</ExposeHeader></CORSRule></CORSConfiguration>`)
	sum := md5.Sum(config)

	target := srv.URL + bucketPath + "?cors"

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, target, bytes.NewReader(config))
	require.NoError(t, err)

	req.Header.Set("Authorization", rawAuthorization)
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// putPlainObject stores body under /bucket/plain with no metadata, so a
// response for it takes the raw path only when the request makes it.
func putPlainObject(t *testing.T, srv *httptest.Server, body []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+bucketPath+"/plain", bytes.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("Authorization", rawAuthorization)

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// rawRequest sends one HTTP/1.1 request over a fresh connection, asking for it
// to be closed, and returns the head and the bytes after it.
func rawRequest(t *testing.T, srv *httptest.Server, method, target, headers string) (head string, body []byte) {
	t.Helper()

	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	request := method + " " + target + " HTTP/1.1\r\nHost: " + srv.Listener.Addr().String() +
		"\r\nAuthorization: " + rawAuthorization + "\r\nConnection: close\r\n" + headers + "\r\n"
	_, err = io.WriteString(conn, request)
	require.NoError(t, err)

	all, err := io.ReadAll(conn)
	require.NoError(t, err)

	head, rest, found := strings.Cut(string(all), headEnd)
	require.True(t, found, "no end of headers in %q", all)

	return head + "\r\n", []byte(rest)
}

// headerLine returns the bytes of the first header line called name, as sent.
func headerLine(head, name string) (string, bool) {
	_, after, found := strings.Cut(head, "\r\n"+name+": ")
	if !found {
		return "", false
	}

	line, _, _ := strings.Cut(after, "\r\n")

	return line, true
}

// TestAnEarlyErrorReachesAClientThatIsStillSending is the case of a response
// written through the connection to a request whose body is unread. Closing the
// connection with those bytes unread sends a reset, which can reach the client
// before the response does: the PUT below was lost 3 times in 20 without the
// linger.
func TestAnEarlyErrorReachesAClientThatIsStillSending(t *testing.T) {
	earlyErrors(t, startEdgeServer(t, nil))
}

// TestAnEarlyErrorReachesAClientOverTLS is the same over TLS, where the
// half-close is tls.Conn.CloseWrite.
func TestAnEarlyErrorReachesAClientOverTLS(t *testing.T) {
	earlyErrors(t, startEdgeTLS(t))
}

func earlyErrors(t *testing.T, srv *httptest.Server) {
	t.Helper()

	putEdgeObject(t, srv, []byte("hello"))
	putExposeEdgeCORS(t, srv)

	body := bytes.Repeat([]byte("a"), 8<<20)

	for try := range 20 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+bucketPath+"/big", bytes.NewReader(body))
		require.NoError(t, err)

		req.Header.Set("Authorization", rawAuthorization)
		req.Header.Set("Origin", crossOrigin)
		req.Header.Set("X-Amz-Storage-Class", "BOGUS")

		resp, err := srv.Client().Do(req)
		require.NoError(t, err, "try %d: the response is lost", try)

		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "try %d", try)
		assert.True(t, resp.Close, "try %d: the response took the connection", try)
	}
}

// controlValues are values a client can send in a response-* query parameter.
var controlValues = map[string]string{
	"plain":         "no-store",
	"SOH":           "c\x01d",
	"NUL":           "c\x00d",
	"US":            "c\x1fd",
	"DEL":           "c\x7fd",
	"vertical tab":  "c\x0bd",
	"form feed":     "c\x0cd",
	"inner tab":     "c\td",
	"inner space":   "c  d",
	"UTF-8":         "café",
	"CR":            "c\rd",
	"LF":            "c\nd",
	"CRLF injected": "c\r\nSet-Cookie: p=1\r\nd",
}

// TestControlBytesAreSentAsNetHTTPSendsThem compares the response written
// through the connection with the one net/http writes, byte for byte, for the
// same value. The first asks for a Content-Type that starts with a space, which
// net/http would trim, so only it takes the raw path.
func TestControlBytesAreSentAsNetHTTPSendsThem(t *testing.T) {
	srv := startEdgeServer(t, nil)
	putEdgeObject(t, srv, []byte("hello"))
	putPlainObject(t, srv, []byte("hello"))

	for name, value := range controlValues {
		t.Run(name, func(t *testing.T) {
			target := bucketPath + "/plain?response-cache-control=" + url.QueryEscape(value) + "&response-content-type="

			ordinary, _ := rawRequest(t, srv, http.MethodGet, target+"text%2Fplain", "")
			raw, _ := rawRequest(t, srv, http.MethodGet, target+"%20text%2Fplain", "")

			require.Contains(t, raw, "\r\nContent-Type:  text/plain\r\n", "the request took the raw path")
			require.NotContains(t, ordinary, "\r\nContent-Type:  ", "the request took net/http's own path")

			want, ok := headerLine(ordinary, "Cache-Control")
			require.True(t, ok, "net/http sends the value: %q", ordinary)

			got, ok := headerLine(raw, "Cache-Control")
			require.True(t, ok, "the raw path drops the value: %q", raw)

			assert.Equal(t, want, got)
			assert.NotContains(t, raw, "\r\nSet-Cookie:", "no line a value did not start")
		})
	}
}

// TestARawResponseOfKnownSizeSendsItsLength covers the three bodies the handler
// does not give a Content-Length: none at all (an empty 200), an error and an
// XML document. net/http sends the length of each; the raw path sent none, so
// the body ended only when the connection closed.
func TestARawResponseOfKnownSizeSendsItsLength(t *testing.T) {
	srv := startEdgeServer(t, nil)
	putEdgeObject(t, srv, []byte("hello"))
	putExposeEdgeCORS(t, srv)

	tests := []struct {
		name       string
		giveMethod string
		giveTarget string
		giveHeader string
		wantStatus string
		wantEmpty  bool
	}{
		{
			name: "an empty 200", giveMethod: http.MethodPut, giveTarget: bucketPath + "/empty",
			giveHeader: "Content-Length: 0\r\n", wantStatus: "200 OK", wantEmpty: true,
		},
		{name: "an error", giveMethod: http.MethodGet, giveTarget: bucketPath + "/missing", wantStatus: "404 Not Found"},
		{name: "an XML document", giveMethod: http.MethodGet, giveTarget: bucketPath + "?list-type=2", wantStatus: "200 OK"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, body := rawRequest(t, srv, tt.giveMethod, tt.giveTarget, "Origin: "+crossOrigin+"\r\n"+tt.giveHeader)

			require.True(t, strings.HasPrefix(head, "HTTP/1.1 "+tt.wantStatus+"\r\n"), head)
			require.Contains(t, head, "\r\nConnection: close\r\n", "the response took the raw path")
			require.Contains(t, head, exposeEdge, "the CORS headers carry the edge value")

			assert.Contains(t, head, "\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n")
			assert.NotContains(t, head, "Transfer-Encoding")
			assert.Equal(t, tt.wantEmpty, len(body) == 0)
		})
	}
}

// dirtyLog remembers what the gateway marks dirty.
type dirtyLog struct {
	mu    sync.Mutex
	names []string
}

func (d *dirtyLog) MarkDirty(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.names = append(d.names, name)
}

func (d *dirtyLog) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	names := d.names
	d.names = nil

	return names
}

func (d *dirtyLog) empty() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.names) == 0
}

// markWatch counts the bytes the server writes to a connection while it watches
// and nothing is marked dirty yet.
type markWatch struct {
	dirty    *dirtyLog
	watching atomic.Bool
	unmarked atomic.Int64
}

// watchedListener hands the server connections whose writes markWatch sees,
// the hijacked ones included: Hijack returns the connection Accept gave.
type watchedListener struct {
	net.Listener

	watch *markWatch
}

func (l watchedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err //nolint:wrapcheck // net.Listener: the error of the Accept it delegates to.
	}

	return &watchedConn{Conn: conn, watch: l.watch}, nil
}

type watchedConn struct {
	net.Conn

	watch *markWatch
}

func (c *watchedConn) Write(p []byte) (int, error) {
	if c.watch.watching.Load() && c.watch.dirty.empty() {
		c.watch.unmarked.Add(int64(len(p)))
	}

	return c.Conn.Write(p) //nolint:wrapcheck // io.Writer: the error of the Write it delegates to.
}

// CloseWrite keeps the half-close the raw path lingers with.
func (c *watchedConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite() //nolint:wrapcheck // the error of the CloseWrite it delegates to.
	}

	return nil
}

// TestARawWriteIsMarkedDirtyBeforeItsResponseIsSent watches the server's side of
// the connection: a successful write answered on the raw path is marked dirty
// before the first byte of its response is written, so a client that reads the
// success and asks for a save at once finds the change marked. The 200 holds
// its body back for its length and is sent at the end of the handler; the 204
// is complete once its head is written.
func TestARawWriteIsMarkedDirtyBeforeItsResponseIsSent(t *testing.T) {
	watch := &markWatch{dirty: new(dirtyLog)}
	srv, _ := newEdgeServer(t, nil, watch.dirty)
	srv.Listener = watchedListener{Listener: srv.Listener, watch: watch}
	srv.Start()
	t.Cleanup(srv.Close)

	putEdgeObject(t, srv, []byte("hello"))
	putExposeEdgeCORS(t, srv)

	cross := "Origin: " + crossOrigin + "\r\nContent-Length: 0\r\n"

	for _, tt := range []struct {
		method, status string
	}{
		{method: http.MethodPut, status: "200"},
		{method: http.MethodDelete, status: "204"},
	} {
		watch.dirty.take()
		watch.unmarked.Store(0)
		watch.watching.Store(true)

		head, _ := rawRequest(t, srv, tt.method, bucketPath+"/k", cross)

		watch.watching.Store(false)

		require.True(t, strings.HasPrefix(head, "HTTP/1.1 "+tt.status+" "), head)
		require.Contains(t, head, "\r\nConnection: close\r\n", "the response took the raw path")
		assert.Zero(t, watch.unmarked.Load(), "%s: bytes sent before the write was marked dirty", tt.method)
		assert.Equal(t, []string{"s3"}, watch.dirty.take(), "%s: marked once", tt.method)
	}
}

// TestARawErrorDoesNotMarkStateDirty shows the gateway learns the status of a
// response written through the connection: a 400 is not a success, so no save
// follows it, and a 200 is one.
func TestARawErrorDoesNotMarkStateDirty(t *testing.T) {
	dirty := new(dirtyLog)
	srv, _ := startEdgeProvider(t, nil, dirty)

	putEdgeObject(t, srv, []byte("hello"))
	putExposeEdgeCORS(t, srv)
	dirty.take()

	cross := "Origin: " + crossOrigin + "\r\nContent-Length: 0\r\n"

	head, _ := rawRequest(t, srv, http.MethodPut, bucketPath+"/k", cross+"X-Amz-Storage-Class: BOGUS\r\n")
	require.True(t, strings.HasPrefix(head, "HTTP/1.1 400 "), head)
	require.Contains(t, head, "\r\nConnection: close\r\n", "the response took the raw path")
	assert.Empty(t, dirty.take(), "a 400 changed nothing")

	head, _ = rawRequest(t, srv, http.MethodPut, bucketPath+"/k", cross)
	require.True(t, strings.HasPrefix(head, "HTTP/1.1 200 "), head)
	require.Contains(t, head, "\r\nConnection: close\r\n", "the response took the raw path")
	assert.Equal(t, []string{"s3"}, dirty.take(), "a 200 on a write")
}

// TestCloseWaitsForARawResponseStillStreaming starts a GetObject on the raw path
// that the client reads slowly, calls Close while it is half sent, and checks
// that Close waits and the client gets the whole object.
// http.Server.Shutdown does not wait for a hijacked connection.
func TestCloseWaitsForARawResponseStillStreaming(t *testing.T) {
	srv, provider := startEdgeProvider(t, nil, nil)
	body := bytes.Repeat([]byte("0123456789abcdef"), (48<<20)/16)
	putEdgeObject(t, srv, body)

	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(30*time.Second)))

	_, err = io.WriteString(conn, "GET "+bucketPath+"/key HTTP/1.1\r\nHost: "+srv.Listener.Addr().String()+
		"\r\nAuthorization: "+rawAuthorization+headEnd)
	require.NoError(t, err)

	first := make([]byte, 64<<10)
	_, err = io.ReadFull(conn, first)
	require.NoError(t, err)

	closed := make(chan struct{})

	go func() {
		provider.Close(t.Context())
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while a response was still being written")
	case <-time.After(300 * time.Millisecond):
	}

	rest, err := io.ReadAll(conn)
	require.NoError(t, err)

	head, got, found := strings.Cut(string(slices.Concat(first, rest)), headEnd)
	require.True(t, found)
	assert.Contains(t, head, "\r\nContent-Length: "+strconv.Itoa(len(body)))
	assert.Len(t, got, len(body))
	assert.True(t, bytes.Equal(body, []byte(got)), "the object arrives whole")

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the response was done")
	}
}
