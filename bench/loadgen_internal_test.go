package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closingBody behaves as the SDK's request body does: once closed, Read and
// WriteTo return io.EOF. Its reads past the end wait for Close, which puts the
// SDK's Close ahead of the check net/http makes for extra body bytes, the order
// a fast server produces.
type closingBody struct {
	data   *bytes.Reader
	closed chan struct{}
	once   sync.Once
}

func newClosingBody(data string) *closingBody {
	return &closingBody{data: bytes.NewReader([]byte(data)), closed: make(chan struct{})}
}

func (b *closingBody) Read(p []byte) (int, error) {
	if b.data.Len() == 0 {
		<-b.closed

		return 0, io.EOF
	}

	return b.data.Read(p) //nolint:wrapcheck // io.Reader: the error of the Read it delegates to.
}

func (b *closingBody) WriteTo(io.Writer) (int64, error) {
	<-b.closed

	return 0, io.EOF
}

func (b *closingBody) Close() error { //nolint:unparam // io.Closer: net/http calls it through the interface.
	b.once.Do(func() { close(b.closed) })

	return nil
}

// A request body closed while net/http still checks it for extra bytes must not
// fail the request write: a failed write closes the connection under the
// response body. Without readOnlyBodies, WriteTo's io.EOF fails the write.
func Test_readOnlyBodies_survives_a_body_closed_on_response(t *testing.T) {
	const reqBody = `{"QueueUrl":"q"}`

	answer := strings.Repeat("x", 16<<10) // larger than the transport's 4 KiB read buffer

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()

	client := &http.Client{Transport: readOnlyBodies{newTransport(1)}}

	for range 2 {
		body := newClosingBody(reqBody)
		wrote := make(chan error, 1)
		trace := &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) { wrote <- info.Err },
		}

		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace),
			http.MethodPost, server.URL, body)
		require.NoError(t, err)

		req.ContentLength = int64(len(reqBody))

		resp, err := client.Do(req)
		require.NoError(t, err)

		// The SDK closes its request body as soon as Do returns.
		require.NoError(t, body.Close())
		require.NoError(t, <-wrote, "the request write failed after the body was closed")

		got, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		assert.Equal(t, answer, string(got))
	}
}
