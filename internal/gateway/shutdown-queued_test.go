package gateway_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stallStating opens a connection to server, sends head with a Content-Length
// of length and the first bytes of the body, sent, then sends nothing more.
func stallStating(t *testing.T, server *httptest.Server, head, sent string, length int64) net.Conn {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Write([]byte(head + "Content-Length: " + strconv.FormatInt(length, 10) + "\r\n\r\n" + sent))
	require.NoError(t, err)

	return conn
}

// Test_Gateway_ShutdownAnswersQueuedBodies: a body queued for room in the
// in-flight bound when the shutdown begins is answered with its service's
// throttle at once, not left to the body wait or cut with no answer when the
// drain ends. A json body that states the cap and stalls holds most of the
// pool; a second, queued behind it, is answered 400 ThrottlingException as
// BeginShutdown runs, long before the minute's body wait.
func Test_Gateway_ShutdownAnswersQueuedBodies(t *testing.T) {
	withInFlight(t, time.Minute)

	reg := generatedRegistration(sqsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
		sqsapi.Serve(sqsapi.Unimplemented{}, w, rc)
	})
	g := newGateway(t, newSettings(), []service.Registration{reg})

	server := httptest.NewServer(g.Handler())
	defer server.Close()

	head := "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
		"X-Amz-Target: AmazonSQS.SendMessage\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n"
	sent := `{"QueueUrl":"` + strings.Repeat("a", 128<<10)

	holder := stallStating(t, server, head, sent, protocol.DefaultMaxBody)

	require.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held > protocol.DefaultInFlight/2
	}, 5*time.Second, time.Millisecond, "the first body holds most of the pool")

	queued := stallStating(t, server, head, sent, protocol.DefaultMaxBody)

	require.Eventually(t, func() bool { return protocol.BodiesWait(0) }, 5*time.Second, time.Millisecond,
		"the second body waits for room")

	start := time.Now()

	g.BeginShutdown()

	status, body := answerOf(t, queued, 10*time.Second)

	assert.Equal(t, http.StatusBadRequest, status, body)
	assert.Contains(t, body, throttled)
	assert.Less(t, time.Since(start), 5*time.Second, "answered as the shutdown begins, not at the body wait")

	_ = holder.Close()

	assert.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held == 0
	}, 5*time.Second, time.Millisecond, "nothing is held once both are gone")
}
