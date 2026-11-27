package gateway_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_UnreadBodyDeadline is SQ-320: once a handler has answered
// without reading all of its request's body, the read net/http makes of the
// rest is bounded by one second, whoever armed the read deadline before. The
// meter's own deadline (BodyMeter.IdleBy), cleared when the charge is given
// back, must not undo it; a body the meter already timed out gets no further
// second; and a handler that panics gets the same bound as one that returns.
func Test_Gateway_UnreadBodyDeadline(t *testing.T) {
	withInFlight(t, 2*time.Second)

	reader := sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
		if rc.Request.Header.Get("X-Panic") != "" {
			panic("early")
		}

		body, err := protocol.ReadRequest(rc.Request)
		if err != nil || protocol.OverBody(len(body)) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "early")

			return
		}

		_, _ = io.WriteString(w, "read")
	})

	server := httptest.NewServer(newGateway(t, newSettings(), []service.Registration{reader}).Handler())
	defer server.Close()

	head := "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
		"X-Amz-Target: AmazonSQS.SendMessage\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n"

	// send writes raw to the server, then sends nothing more, and returns
	// the connection.
	send := func(t *testing.T, raw string) (net.Conn, *bufio.Reader) {
		t.Helper()

		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		_, err = conn.Write([]byte(raw))
		require.NoError(t, err)

		return conn, bufio.NewReader(conn)
	}

	// closedWithin reads the answer, wanting status, then reports whether the
	// server closed the connection within wait of it.
	closedWithin := func(t *testing.T, conn net.Conn, reader *bufio.Reader, status int, wait time.Duration) {
		t.Helper()

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

		resp, err := http.ReadResponse(reader, nil)
		require.NoError(t, err, "the answer arrives")

		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(t, status, resp.StatusCode)
		assert.True(t, resp.Close, "the answer closes the connection")

		answered := time.Now()

		require.NoError(t, conn.SetReadDeadline(answered.Add(wait)))

		_, err = reader.ReadByte()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the server kept the connection %v after the answer", wait)

			return
		}

		assert.ErrorIs(t, err, io.EOF, "the server closes the connection")
	}

	t.Run("an armed meter and an early answer close within a second", func(t *testing.T) {
		previous := protocol.MaxBody()

		protocol.SetMaxBody(64 << 10)
		t.Cleanup(func() { protocol.SetMaxBody(previous) })

		// A chunk that states 192 KiB, of which 128 KiB is sent: the body is
		// cut at MaxBody, and what is left of it never arrives.
		conn, reader := send(t, head+"Transfer-Encoding: chunked\r\n\r\n"+
			strconv.FormatInt(192<<10, 16)+"\r\n"+`{"QueueUrl":"`+strings.Repeat("a", 128<<10))

		closedWithin(t, conn, reader, http.StatusBadRequest, 1500*time.Millisecond)
	})

	t.Run("a body the meter timed out gets no further second", func(t *testing.T) {
		withBodyIdle(t)

		conn, reader := send(t, head+"Content-Length: 200000\r\n\r\n"+`{"QueueUrl":"`+strings.Repeat("a", 1000))

		closedWithin(t, conn, reader, http.StatusBadRequest, 500*time.Millisecond)
	})

	t.Run("a handler that panics gets the bound too", func(t *testing.T) {
		conn, reader := send(t, head+"X-Panic: 1\r\nContent-Length: 100000\r\n\r\n{"+strings.Repeat(" ", 999))

		closedWithin(t, conn, reader, http.StatusInternalServerError, 1500*time.Millisecond)
	})
}
