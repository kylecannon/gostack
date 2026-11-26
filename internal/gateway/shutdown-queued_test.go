package gateway_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
// of protocol.DefaultMaxBody and the first bytes of the body, sent, then sends
// nothing more.
func stallStating(t *testing.T, server *httptest.Server, head, sent string) net.Conn {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	length := strconv.FormatInt(protocol.DefaultMaxBody, 10)
	_, err = conn.Write([]byte(head + "Content-Length: " + length + "\r\n\r\n" + sent))
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

	holder := stallStating(t, server, head, sent)

	require.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held > protocol.DefaultInFlight/2
	}, 5*time.Second, time.Millisecond, "the first body holds most of the pool")

	queued := stallStating(t, server, head, sent)

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

// Test_Gateway_ShutdownStopsOnlyItsOwnQueuedBodies: two gateways share the
// in-flight bound. A body queued at the first keeps waiting while the second,
// made after it, shuts down, and is answered with its throttle once the first
// begins to shut down.
func Test_Gateway_ShutdownStopsOnlyItsOwnQueuedBodies(t *testing.T) {
	withInFlight(t, time.Minute)

	reg := generatedRegistration(sqsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
		sqsapi.Serve(sqsapi.Unimplemented{}, w, rc)
	})
	g1 := newGateway(t, newSettings(), []service.Registration{reg})
	g2 := newGateway(t, newSettings(), []service.Registration{reg})

	server := httptest.NewServer(g1.Handler())
	defer server.Close()

	head := "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
		"X-Amz-Target: AmazonSQS.SendMessage\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n"
	sent := `{"QueueUrl":"` + strings.Repeat("a", 128<<10)

	holder := stallStating(t, server, head, sent)

	require.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held > protocol.DefaultInFlight/2
	}, 5*time.Second, time.Millisecond, "the first body holds most of the pool")

	queued := stallStating(t, server, head, sent)

	require.Eventually(t, func() bool { return protocol.BodiesWait(0) }, 5*time.Second, time.Millisecond,
		"the second body waits for room")

	g2.BeginShutdown()

	assert.Never(t, func() bool { return !protocol.BodiesWait(0) }, time.Second, 10*time.Millisecond,
		"the other gateway's shutdown leaves the body waiting")

	start := time.Now()

	g1.BeginShutdown()

	status, body := answerOf(t, queued, 10*time.Second)

	assert.Equal(t, http.StatusBadRequest, status, body)
	assert.Contains(t, body, throttled)
	assert.Less(t, time.Since(start), 5*time.Second, "answered as its own gateway's shutdown begins")

	_ = holder.Close()

	assert.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held == 0
	}, 5*time.Second, time.Millisecond, "nothing is held once both are gone")
}

// sentSQS answers every SendMessage as sent.
type sentSQS struct{ sqsapi.Unimplemented }

func (sentSQS) SendMessage(context.Context, *sqsapi.SendMessageInput) (*sqsapi.SendMessageOutput, error) {
	return &sqsapi.SendMessageOutput{}, nil
}

// Test_Gateway_ShutdownServesAdmittedBodies: a chunked body already admitted
// and growing when the shutdown begins keeps waiting for its next step, and is
// served to completion once room frees within the drain. The shutdown stop
// answers only bodies that hold nothing of the pool. The chunked body sends
// 1 MiB and is admitted; a json body that states the cap and stalls then
// holds most of the pool, so the chunked body's next step waits for room.
// BeginShutdown runs, the stalled body goes away, and the chunked body is
// answered 200.
func Test_Gateway_ShutdownServesAdmittedBodies(t *testing.T) {
	withInFlight(t, time.Minute)

	reg := generatedRegistration(sqsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
		sqsapi.Serve(sentSQS{}, w, rc)
	})
	g := newGateway(t, newSettings(), []service.Registration{reg})

	server := httptest.NewServer(g.Handler())
	defer server.Close()

	head := "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
		"X-Amz-Target: AmazonSQS.SendMessage\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n"

	growing, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	require.NoError(t, err)

	defer func() { _ = growing.Close() }()

	chunk := func(data string) string { return strconv.FormatInt(int64(len(data)), 16) + "\r\n" + data + "\r\n" }

	_, err = growing.Write([]byte(head + "Transfer-Encoding: chunked\r\n\r\n" +
		chunk(`{"QueueUrl":"q","MessageBody":"`+strings.Repeat("a", 1<<20))))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held > 1<<20
	}, 5*time.Second, time.Millisecond, "the chunked body is admitted")

	holder := stallStating(t, server, head, `{"QueueUrl":"`+strings.Repeat("a", 128<<10))

	require.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held > protocol.DefaultInFlight/2
	}, 5*time.Second, time.Millisecond, "the stated body holds most of the pool")

	// The rest of the chunked body: the writes block once the gateway stops
	// reading, while its next step waits for room.
	sent := make(chan error, 1)

	go func() {
		rest := chunk(strings.Repeat("a", 8<<20)) + chunk(`"}`) + "0\r\n\r\n"
		_, werr := growing.Write([]byte(rest))
		sent <- werr
	}()

	require.Eventually(t, func() bool { return protocol.BodiesWait(0) }, 5*time.Second, time.Millisecond,
		"the chunked body's next step waits for room")

	g.BeginShutdown()

	require.NoError(t, growing.SetReadDeadline(time.Now().Add(500*time.Millisecond)))

	_, err = growing.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the admitted body is not answered as the shutdown begins")

	_ = holder.Close()

	status, body := answerOf(t, growing, 10*time.Second)

	assert.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, throttled)
	require.NoError(t, <-sent)

	assert.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held == 0
	}, 5*time.Second, time.Millisecond, "nothing is held once both are gone")
}
