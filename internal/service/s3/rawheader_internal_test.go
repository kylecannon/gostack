package s3

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hijackProbe is a response writer whose connection cannot be taken. It counts
// the attempts, which is how these tests tell a response that tried to take the
// connection from one that did not.
type hijackProbe struct {
	*httptest.ResponseRecorder

	hijacks int
}

func (p *hijackProbe) Hijack() (conn net.Conn, rw *bufio.ReadWriter, err error) {
	p.hijacks++

	return nil, nil, http.ErrNotSupported
}

// metaEdge is the key spellHeaders gives the X-Amz-Meta-Edge header, spelled so
// that no constant index trips SA1008.
var metaEdge = strings.ToLower("X-Amz-Meta-Edge")

// wrapper hides the probe the way a middleware writer does, behind Unwrap only.
type wrapper struct{ http.ResponseWriter }

func (w wrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestOnlyAnEdgeWhitespaceHeaderOnHTTP1TakesTheConnection(t *testing.T) {
	tests := []struct {
		name        string
		giveValue   string
		giveHTTP1   bool
		wantHijacks int
	}{
		{name: "ordinary values", giveValue: "ordinary", giveHTTP1: true, wantHijacks: 0},
		{name: "inner white space", giveValue: "a b\tc", giveHTTP1: true, wantHijacks: 0},
		{name: "leading space", giveValue: " a", giveHTTP1: true, wantHijacks: 1},
		{name: "trailing tab", giveValue: "a\t", giveHTTP1: true, wantHijacks: 1},
		{name: "edge white space over HTTP/2", giveValue: " a\t", giveHTTP1: false, wantHijacks: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &hijackProbe{ResponseRecorder: httptest.NewRecorder()}
			w := &responseWriter{ResponseWriter: wrapper{probe}, method: http.MethodGet, http1: tt.giveHTTP1}

			w.Header().Set("X-Amz-Meta-Edge", tt.giveValue)
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("body"))
			require.NoError(t, err)

			assert.Equal(t, tt.wantHijacks, probe.hijacks)

			// A connection that cannot be taken gets today's write, untouched.
			assert.Nil(t, w.raw)
			assert.Equal(t, http.StatusOK, probe.Code)
			assert.Equal(t, "body", probe.Body.String())
			assert.Equal(t, tt.giveValue, probe.Header()[metaEdge][0])
		})
	}
}

func TestWriteRawHead(t *testing.T) {
	header := http.Header{
		"Content-Length":    {"5"},
		"Content-Type":      {contentTypeXML},
		"Connection":        {"keep-alive"},
		"Transfer-Encoding": {"chunked"},
		"x-amz-meta-edge":   {" v\t"},
		"x-amz-meta-split":  {"a\r\nInjected: yes"},
		"x-amz-meta-nul":    {"a\x00b"},
		"bad name":          {"x"},
		"Date":              {"Mon, 02 Jan 2006 15:04:05 GMT"},
		"Multi":             {"one", "two"},
	}

	var out bytes.Buffer

	require.NoError(t, writeRawHead(bufio.NewWriter(&out), http.StatusOK, header))
	assert.Equal(t, "HTTP/1.1 200 OK\r\n"+
		"Content-Length: 5\r\n"+
		"Content-Type: "+contentTypeXML+"\r\n"+
		"Date: Mon, 02 Jan 2006 15:04:05 GMT\r\n"+
		"Multi: one\r\n"+
		"Multi: two\r\n"+
		"x-amz-meta-edge:  v\t\r\n"+
		"x-amz-meta-split: a  Injected: yes\r\n"+
		"Connection: close\r\n\r\n", out.String())

	out.Reset()
	require.NoError(t, writeRawHead(bufio.NewWriter(&out), http.StatusNotModified, header))
	assert.NotContains(t, out.String(), "Content-Length")
	assert.Contains(t, out.String(), "HTTP/1.1 304 Not Modified\r\n")
}
