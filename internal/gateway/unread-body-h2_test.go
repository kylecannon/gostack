package gateway_test

import (
	"context"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener

	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}

	return conn, err //nolint:wrapcheck // net.Listener: the error of the Accept it delegates to.
}

// Test_Gateway_UnreadBodyKeepsHTTP2 is SQ-320. On HTTP/2 a handler's
// Connection: close makes net/http send a GOAWAY and end the whole connection,
// with every other stream on it, and a request with no body still has one, of
// length zero. Neither a bodyless request nor an answer before the body closes
// an HTTP/2 connection: every request below runs on the one connection, and
// every stream is answered.
func Test_Gateway_UnreadBodyKeepsHTTP2(t *testing.T) {
	release, waiting := make(chan struct{}), make(chan struct{})

	early := sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
		if rc.Request.Header.Get("X-Wait") != "" {
			close(waiting)
			<-release
		}

		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "early")
	})

	health := func(r chi.Router) {
		r.Get("/_localstack/health", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "up")
		})
	}

	g := newGateway(t, newSettings(), []service.Registration{early}, health)

	var config net.ListenConfig

	raw, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	listener := &countingListener{Listener: raw}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- g.Serve(ctx, listener) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	base := "http://" + raw.Addr().String()

	call := func(ctx context.Context, h2c *http.Client, method, path, body string, header http.Header) (int, int, error) {
		r, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			return 0, 0, err //nolint:wrapcheck // a test helper
		}

		if body == "" {
			r.Body = http.NoBody
		}

		maps.Copy(r.Header, header)

		resp, err := h2c.Do(r)
		if err != nil {
			return 0, 0, err //nolint:wrapcheck // a test helper
		}

		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		return resp.StatusCode, resp.ProtoMajor, nil
	}

	sqs := func(extra string) http.Header {
		h := http.Header{
			headerAuth:     {signed("test", usEast1, nameSQS)},
			"Content-Type": {"application/x-amz-json-1.0"},
			"X-Amz-Target": {"AmazonSQS.SendMessage"},
		}
		if extra != "" {
			h.Set(extra, "1")
		}

		return h
	}

	t.Run("two health checks share the connection", func(t *testing.T) {
		h2c, before := client(t, "h2c"), listener.accepted.Load()

		for range 2 {
			status, major, err := call(t.Context(), h2c, http.MethodGet, "/_localstack/health", "", nil)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, status)
			assert.Equal(t, 2, major)
		}

		assert.Equal(t, int64(1), listener.accepted.Load()-before, "no GOAWAY: one connection")
	})

	t.Run("an answer before the body leaves a concurrent stream and the connection", func(t *testing.T) {
		h2c, before := client(t, "h2c"), listener.accepted.Load()

		var (
			wg      sync.WaitGroup
			waited  int
			waitErr error
		)

		wg.Go(func() {
			waited, _, waitErr = call(context.Background(), h2c, http.MethodPost, "/", "{}", sqs("X-Wait"))
		})

		<-waiting

		status, _, err := call(t.Context(), h2c, http.MethodPost, "/", "{"+strings.Repeat(" ", 100_000)+"}", sqs(""))

		// The waiting stream is let go before any assertion can end the test.
		close(release)
		wg.Wait()

		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, status)

		require.NoError(t, waitErr, "the waiting stream is answered")
		assert.Equal(t, http.StatusBadRequest, waited)

		status, _, err = call(t.Context(), h2c, http.MethodGet, "/_localstack/health", "", nil)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)

		assert.Equal(t, int64(1), listener.accepted.Load()-before, "no GOAWAY: one connection")
	})
}
