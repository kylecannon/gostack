package gateway_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testIdle is the idle timeout the tests here run with.
const testIdle = 300 * time.Millisecond

// withBodyIdle sets the idle timeout for the test and puts the default back
// after it.
func withBodyIdle(t *testing.T) {
	t.Helper()

	protocol.SetBodyIdle(testIdle)
	t.Cleanup(func() { protocol.SetBodyIdle(protocol.DefaultBodyIdle) })
}

// stall opens a connection to server, sends head and the first sent bytes of a
// body that states it is 1 MiB, then sends nothing more.
func stall(t *testing.T, server *httptest.Server, head, body string) net.Conn {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	require.NoError(t, err)

	_, err = conn.Write([]byte(head + "Content-Length: " + strconv.Itoa(1<<20) + "\r\n\r\n" + body))
	require.NoError(t, err)

	return conn
}

// answerOf reads the response on conn, waiting at most wait.
func answerOf(t *testing.T, conn net.Conn, wait time.Duration) (int, string) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(wait)))

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Errorf("no answer: %v", err)

		return 0, ""
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

// Test_Gateway_StalledBodies: 64 clients each send 128 KiB of a body that
// states 1 MiB, enough to be metered and charged, then send nothing. While they
// stall, honest requests are served. Once the idle timeout passes, each stalled
// request is answered with its protocol's answer: S3's 400 RequestTimeout, or
// the 400 a json service gives a body it cannot read. What they held is given
// back.
func Test_Gateway_StalledBodies(t *testing.T) {
	withInFlight(t, 2*time.Second)
	withBodyIdle(t)

	server := httptest.NewServer(wideGateway(t))
	defer server.Close()

	s3Head := "PUT /b?lifecycle HTTP/1.1\r\nHost: localhost\r\nAuthorization: " + signed("test", usEast1, nameS3) + "\r\n"
	jsonHead := "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
		"X-Amz-Target: AmazonSQS.SendMessageBatch\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n"
	// Text, not dense elements: what each holds fits the pool with room for the
	// honest request, so every stalled body is charged and none is throttled.
	s3Body := "<LifecycleConfiguration><Rule><ID>" + strings.Repeat("a", 128<<10)
	jsonBody := `{"QueueUrl":"` + strings.Repeat("a", 128<<10)

	conns := make([]net.Conn, concurrentBodies)
	for i := range conns {
		if i%2 == 0 {
			conns[i] = stall(t, server, s3Head, s3Body)
		} else {
			conns[i] = stall(t, server, jsonHead, jsonBody)
		}

		t.Cleanup(func() { _ = conns[i].Close() })
	}

	// While every stalled body holds its charge, an honest metered request
	// is served.
	time.Sleep(testIdle / 3)

	_, held := protocol.InFlight()
	assert.Positive(t, held, "the stalled bodies are charged")

	honest := newRequest(t, http.MethodPut, server.URL+"/b?lifecycle", thousandRules(),
		map[string]string{headerAuth: signed("test", usEast1, nameS3)})
	honest.RequestURI = ""

	resp, err := server.Client().Do(honest)
	require.NoError(t, err)

	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode, string(answer))

	var wg sync.WaitGroup

	for i, conn := range conns {
		wg.Go(func() {
			status, body := answerOf(t, conn, 10*testIdle)

			assert.Equal(t, http.StatusBadRequest, status, body)

			if i%2 == 0 {
				assert.Contains(t, body, "<Code>RequestTimeout</Code>")
				assert.Contains(t, body, "Idle connections will be closed.")
			} else {
				assert.NotContains(t, body, throttled)
			}
		})
	}

	wg.Wait()

	_, held = protocol.InFlight()
	assert.Zero(t, held, "every stalled body gave back what it held")
}

// Test_Gateway_IdleDeadlineEndsWithTheBody: a request held open after its body
// was read, as an SQS long poll is, is answered however long it is held, and
// the connection serves the next request: the deadline is cleared at the end of
// the body and when the request's charge is given back.
func Test_Gateway_IdleDeadlineEndsWithTheBody(t *testing.T) {
	withInFlight(t, 2*time.Second)
	withBodyIdle(t)

	const hold = 3 * testIdle

	reg := service.Registration{
		Metadata: sqsapi.Metadata,
		Serve: func(w http.ResponseWriter, rc *request.Context) {
			body, err := protocol.ReadBody(rc.Request.Body, rc.Request.ContentLength)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			if strings.Contains(string(body), "release") {
				protocol.ReleaseBody(rc.Request.Context())
			}

			time.Sleep(hold)
			_, _ = w.Write([]byte(strconv.Itoa(len(body))))
		},
	}

	server := httptest.NewServer(newGateway(t, newSettings(), []service.Registration{reg}).Handler())
	defer server.Close()

	client := server.Client()
	headers := jsonCall(nameSQS, "AmazonSQS.ReceiveMessage", "1.0")

	for _, body := range []string{
		`{"x":"` + strings.Repeat("a", 128<<10) + `"}`,
		`{"x":"` + strings.Repeat("a", 128<<10) + `","release":1}`,
		`{"x":"` + strings.Repeat("a", 128<<10) + `"}`,
	} {
		req := newRequest(t, http.MethodPost, server.URL+"/", body, headers)
		req.RequestURI = ""

		resp, err := client.Do(req)
		require.NoError(t, err)

		answer, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode, string(answer))
		assert.Equal(t, strconv.Itoa(len(body)), string(answer))
	}
}
