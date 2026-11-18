package s3

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	corsProbeOrigin = "https://app.example"
	// edgePadded starts with a space and ends with a tab, which net/http trims.
	edgePadded = " v\t"
)

// allowAll is a request.DefaultCORS that allows every origin and sends one
// header, the way the gateway's own policy does for an allowed origin.
type allowAll struct{}

func (allowAll) OriginAllowed(http.Header) bool { return true }
func (allowAll) ChecksDisabled() bool           { return false }

func (allowAll) AddHeaders(response http.Header, r *http.Request) {
	response.Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
}

// TestDefaultCORSHeadersReachATakenResponse: ListBuckets and CreateBucket get
// the gateway's CORS headers from defaultCORSWriter's WriteHeader, which a
// response written by hand never reaches. Both the response net/http writes and
// the one takeConn writes carry them.
func TestDefaultCORSHeadersReachATakenResponse(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		value     string
		wantTaken bool
		wantCORS  bool
	}{
		{name: "ListBuckets written by net/http", operation: "ListBuckets", value: "ordinary", wantCORS: true},
		{name: "ListBuckets taken", operation: "ListBuckets", value: edgePadded, wantTaken: true, wantCORS: true},
		{name: "CreateBucket taken", operation: "CreateBucket", value: edgePadded, wantTaken: true, wantCORS: true},
		{name: "another operation taken", operation: "HeadObject", value: edgePadded, wantTaken: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rc := &request.Context{Operation: tt.operation, Request: r, CORS: allowAll{}}
				w := &responseWriter{
					ResponseWriter: &defaultCORSWriter{ResponseWriter: rw, rc: rc},
					method:         r.Method, http1: true, http11: true,
				}

				w.Header().Set("X-Amz-Meta-Edge", tt.value)
				w.Header().Set(headerContentLength, "2")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
				w.finish()
			}))
			t.Cleanup(srv.Close)

			conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
			require.NoError(t, err)

			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nOrigin: "+corsProbeOrigin+
				"\r\nConnection: close\r\n\r\n")
			require.NoError(t, err)

			all, err := io.ReadAll(conn)
			require.NoError(t, err)

			head, _, _ := strings.Cut(string(all), "\r\n\r\n")

			// A taken response keeps the value's white space, net/http's trims it.
			assert.Equal(t, tt.wantTaken, strings.Contains(head, "\r\nx-amz-meta-edge:  v\t\r\n"), head)
			cors := "\r\nAccess-Control-Allow-Origin: " + corsProbeOrigin + "\r\n"
			assert.Equal(t, tt.wantCORS, strings.Contains(head, cors), head)
		})
	}
}
