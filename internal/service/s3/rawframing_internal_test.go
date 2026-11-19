package s3

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTakenConnFraming(t *testing.T) {
	limit := strings.Repeat("x", rawBufferLimit)

	tests := []struct {
		name        string
		give        func(t *testing.T, tc *takenConn)
		wantBody    string
		wantChunked bool
	}{
		{name: "no body at all", give: func(*testing.T, *takenConn) {}},
		{
			name: "a short body in two writes", wantBody: "abcd",
			give: func(t *testing.T, tc *takenConn) { writeAll(t, tc, "ab", "cd") },
		},
		{
			name: "a body at the limit", wantBody: limit,
			give: func(t *testing.T, tc *takenConn) { writeAll(t, tc, limit) },
		},
		{
			name: "a body over the limit", wantBody: limit + "x", wantChunked: true,
			give: func(t *testing.T, tc *takenConn) { writeAll(t, tc, limit+"x") },
		},
		{
			name: "two writes that pass the limit", wantBody: limit + "yz", wantChunked: true,
			give: func(t *testing.T, tc *takenConn) { writeAll(t, tc, limit, "y", "z") },
		},
		{
			name: "a flush before the end", wantBody: "abcd", wantChunked: true,
			give: func(t *testing.T, tc *takenConn) {
				writeAll(t, tc, "ab")
				require.NoError(t, tc.flush())
				writeAll(t, tc, "cd")
			},
		},
	}

	// An HTTP/1.0 client cannot read chunks: what would be chunked for HTTP/1.1
	// ends at the close for it, as net/http answers it.
	for _, http11 := range []bool{true, false} {
		for _, tt := range tests {
			t.Run(tt.name+map[bool]string{true: "", false: ", HTTP/1.0"}[http11], func(t *testing.T) {
				var out bytes.Buffer

				tc := &takenConn{
					out: bufio.NewWriter(&out), length: -1, code: http.StatusOK, http11: http11,
					header: http.Header{headerContentType: {contentTypeXML}},
				}

				tt.give(t, tc)
				tc.finish()
				require.NoError(t, tc.err)

				resp, err := http.ReadResponse(bufio.NewReader(&out), nil)
				require.NoError(t, err)

				got, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())

				assert.Equal(t, tt.wantBody, string(got))

				switch {
				case tt.wantChunked && http11:
					assert.Equal(t, []string{"chunked"}, resp.TransferEncoding)
					assert.Equal(t, int64(-1), resp.ContentLength)
				case tt.wantChunked:
					assert.Empty(t, resp.TransferEncoding, "no chunking for an HTTP/1.0 client")
					assert.Equal(t, int64(-1), resp.ContentLength, "the body ends at the close")
				default:
					assert.Empty(t, resp.TransferEncoding)
					assert.Equal(t, int64(len(tt.wantBody)), resp.ContentLength, "the length of a body that ends in time")
				}
			})
		}
	}
}

func writeAll(t *testing.T, tc *takenConn, parts ...string) {
	t.Helper()

	for _, part := range parts {
		n, err := tc.write([]byte(part))
		require.NoError(t, err)
		require.Equal(t, len(part), n)
	}
}

// TestAPanickedRawResponseIsBroken checks that a response whose handler
// panicked is not completed: it sends nothing, so the client sees a closed
// connection and not a short body that looks whole.
func TestAPanickedRawResponseIsBroken(t *testing.T) {
	server, client := net.Pipe()

	tc := &takenConn{
		conn: server, out: bufio.NewWriter(server), length: -1, code: http.StatusOK,
		header: http.Header{headerContentType: {contentTypeXML}},
	}
	w := &responseWriter{raw: tc, inflight: new(inflight)}

	require.True(t, w.inflight.enter())
	writeAll(t, tc, "part")

	go w.abort()

	got, err := io.ReadAll(client)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestTrackedBodyKnowsWhetherTheClientMayStillBeSending(t *testing.T) {
	tests := []struct {
		name       string
		giveLength int64
		giveRead   int
		wantUnread bool
	}{
		{name: "nothing read", giveLength: 5, giveRead: 0, wantUnread: true},
		{name: "part read", giveLength: 5, giveRead: 3, wantUnread: true},
		{name: "every byte read", giveLength: 5, giveRead: 5, wantUnread: false},
		{name: "chunked, not at its end", giveLength: -1, giveRead: 5, wantUnread: true},
		{name: "chunked, read to EOF", giveLength: -1, giveRead: 6, wantUnread: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/b/k", strings.NewReader("hello"))
			r.ContentLength = tt.giveLength

			w := &responseWriter{}
			w.trackBody(r)

			buf := make([]byte, tt.giveRead)

			for read := 0; read < tt.giveRead; {
				n, err := r.Body.Read(buf[read:])
				read += n

				if err != nil {
					break
				}
			}

			assert.Equal(t, tt.wantUnread, w.body.unread())
		})
	}

	empty := &responseWriter{}
	empty.trackBody(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/b/k", http.NoBody))
	assert.False(t, empty.body.unread(), "a request with no body has nothing unread")
}

type statusProbe struct {
	*httptest.ResponseRecorder

	noted int
}

func (p *statusProbe) NoteStatus(code int) { p.noted = code }

func TestNoteStatusFindsTheGatewayWriterThroughUnwrap(t *testing.T) {
	noter := &statusProbe{ResponseRecorder: httptest.NewRecorder()}

	noteStatus(wrapper{wrapper{noter}}, http.StatusBadRequest)
	assert.Equal(t, http.StatusBadRequest, noter.noted)

	assert.NotPanics(t, func() { noteStatus(httptest.NewRecorder(), http.StatusOK) }, "a writer with none to tell")
}

func TestInflightRefusesNewResponsesOnceDraining(t *testing.T) {
	var f inflight

	require.True(t, f.enter())

	released := make(chan struct{})

	go func() {
		f.drain(t.Context(), 5*time.Second)
		close(released)
	}()

	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.closed
	}, time.Second, time.Millisecond)

	assert.False(t, f.enter(), "a response that starts during the drain is written by net/http")

	select {
	case <-released:
		t.Fatal("drain returned with a response in flight")
	case <-time.After(50 * time.Millisecond):
	}

	f.leave()
	<-released
}
