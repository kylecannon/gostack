package s3_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// edgeValue starts with a space and ends with a tab, which net/http trims
	// from a header value it writes over HTTP/1.x.
	edgeValue = " v\t"
	// edgeEncoded is edgeValue as an RFC 2047 word, the way a client sends
	// metadata whose value a request header would trim.
	edgeEncoded = "=?UTF-8?Q?_v=09?="
	edgeHeader  = "x-amz-meta-edge"
	// edgeCanonical is edgeHeader as a client's Header.Get spells it.
	edgeCanonical = "X-Amz-Meta-Edge"

	rawAuthorization = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host, Signature=0"
	headEnd = "\r\n\r\n"
)

// startEdgeServer runs the real gateway with S3 alone registered. A nil
// protocols leaves the server's default, HTTP/1.1.
func startEdgeServer(t *testing.T, protocols *http.Protocols) *httptest.Server {
	t.Helper()

	srv, _ := startEdgeProvider(t, protocols, nil)

	return srv
}

// startEdgeProvider is startEdgeServer for a test that also calls the provider,
// or that wants to be told what the gateway marks dirty.
func startEdgeProvider(
	t *testing.T, protocols *http.Protocols, dirty gateway.Dirtier,
) (*httptest.Server, *s3.Provider) {
	t.Helper()

	srv, provider := newEdgeServer(t, protocols, dirty)
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, provider
}

// startEdgeTLS is startEdgeServer over TLS with HTTP/1.1. Its clients are
// srv.Client().
func startEdgeTLS(t *testing.T) *httptest.Server {
	t.Helper()

	srv, _ := newEdgeServer(t, nil, nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

// newEdgeServer builds the server of startEdgeServer without starting it.
func newEdgeServer(
	t *testing.T, protocols *http.Protocols, dirty gateway.Dirtier,
) (*httptest.Server, *s3.Provider) {
	t.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(t.TempDir()))
	t.Cleanup(provider.Close)

	gw, err := gateway.New(
		func() gateway.Settings {
			return gateway.Settings{Listen: []string{benchListen}, ResponseHeaderEnabled: true}
		},
		[]service.Registration{provider.Registration()},
		nil,
		dirty,
	)
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(gw.Handler())
	if protocols != nil {
		srv.Config.Protocols = protocols
	}

	return srv, provider
}

// putEdgeObject stores body under /bucket/key with metadata that is edgeValue.
func putEdgeObject(t *testing.T, srv *httptest.Server, body []byte) {
	t.Helper()

	for _, step := range []struct {
		target string
		body   []byte
		header map[string]string
	}{
		{target: bucketPath},
		{target: bucketPath + "/key", body: body, header: map[string]string{edgeHeader: edgeEncoded}},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+step.target, bytes.NewReader(step.body))
		require.NoError(t, err)

		req.Header.Set("Authorization", rawAuthorization)

		for name, value := range step.header {
			req.Header.Set(name, value)
		}

		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

// rawExchange sends one HTTP/1.1 request over a fresh connection, as bytes, and
// returns everything the server writes until it closes the connection.
func rawExchange(t *testing.T, srv *httptest.Server, method string) (head string, body []byte) {
	t.Helper()

	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	request := method + " " + bucketPath + "/key HTTP/1.1\r\nHost: " + srv.Listener.Addr().String() +
		"\r\nAuthorization: " + rawAuthorization + headEnd
	_, err = io.WriteString(conn, request)
	require.NoError(t, err)

	all, err := io.ReadAll(conn)
	require.NoError(t, err, "the server closes the connection after the response")

	head, rest, found := strings.Cut(string(all), headEnd)
	require.True(t, found, "no end of headers in %q", all)

	return head + "\r\n", []byte(rest)
}

func TestEdgeWhitespaceHeadersAreSentUnchangedOverHTTP1(t *testing.T) {
	srv := startEdgeServer(t, nil)
	body := []byte("hello")
	putEdgeObject(t, srv, body)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			head, got := rawExchange(t, srv, method)

			assert.True(t, strings.HasPrefix(head, "HTTP/1.1 200 OK\r\n"), head)
			assert.Contains(t, head, "\r\n"+edgeHeader+": "+edgeValue+"\r\n", "the value, white space and all")
			assert.Contains(t, head, "\r\nContent-Length: 5\r\n")
			assert.Contains(t, head, "\r\nConnection: close\r\n")
			assert.Contains(t, head, "\r\nx-localstack: true\r\n")
			assert.Contains(t, head, "\r\nx-gostack: true\r\n")
			assert.Contains(t, head, "\r\nETag: ")
			assert.Equal(t, 1, strings.Count(head, "\r\nDate: "))

			if method == http.MethodGet {
				assert.Equal(t, body, got)
			} else {
				assert.Empty(t, got, "a HEAD response has no body")
			}
		})
	}
}

func TestEdgeWhitespaceHeadersStreamALargeBody(t *testing.T) {
	srv := startEdgeServer(t, nil)
	body := bytes.Repeat([]byte("0123456789abcdef"), (3<<20)/16)
	putEdgeObject(t, srv, body)

	head, got := rawExchange(t, srv, http.MethodGet)

	assert.Contains(t, head, "\r\n"+edgeHeader+": "+edgeValue+"\r\n")
	assert.Contains(t, head, "\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n")
	assert.Len(t, got, len(body))
	assert.True(t, bytes.Equal(body, got), "the body arrives whole")
}

func TestEdgeWhitespaceHeadersOverH2CAreUnchanged(t *testing.T) {
	both := new(http.Protocols)
	both.SetHTTP1(true)
	both.SetUnencryptedHTTP2(true)

	srv := startEdgeServer(t, both)
	putEdgeObject(t, srv, []byte("hello"))

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)

	client := &http.Client{Transport: &http.Transport{Protocols: protocols}}
	t.Cleanup(client.CloseIdleConnections)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+bucketPath+"/key", http.NoBody)
		require.NoError(t, err)

		req.Header.Set("Authorization", rawAuthorization)

		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		assert.Equal(t, 2, resp.ProtoMajor, method)
		assert.Equal(t, http.StatusOK, resp.StatusCode, method)
		assert.Equal(t, edgeValue, resp.Header.Get(edgeCanonical), method)
	}
}

// TestAnOrdinaryGetKeepsTheConnection shows a response with no edge white space
// takes net/http's own write: it does not announce Connection: close and the
// connection serves the next request. A taken connection could do neither.
func TestAnOrdinaryGetKeepsTheConnection(t *testing.T) {
	srv := startEdgeServer(t, nil)

	put := func(target string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+target, strings.NewReader("hello"))
		require.NoError(t, err)

		req.Header.Set("Authorization", rawAuthorization)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	put(bucketPath)
	put(bucketPath + "/key")

	reused := make([]bool, 0, 2)

	for range 2 {
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = append(reused, info.Reused) }}
		ctx := httptrace.WithClientTrace(context.WithoutCancel(t.Context()), trace)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+bucketPath+"/key", http.NoBody)
		require.NoError(t, err)

		req.Header.Set("Authorization", rawAuthorization)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		assert.False(t, resp.Close, "no Connection: close")
		assert.Equal(t, "hello", string(got))
		assert.Equal(t, int64(5), resp.ContentLength)
	}

	assert.Equal(t, []bool{true, true}, reused, "both GETs reuse the connection the PUTs kept alive")
}
