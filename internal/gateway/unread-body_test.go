package gateway_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_AnswerBeforeTheBody is SQ-320. A handler that answers without
// reading its request's body (S3 answers a PutObject to a bucket that does not
// exist so) gets its answer to a client that stopped sending,
// and the server gives back the connection soon after: net/http would read the
// rest of the body, under 256 KiB, before writing the answer, with no
// deadline. A client that sends its whole body still reads the answer, and a
// handler that reads its body keeps the connection open for the next request.
func Test_Gateway_AnswerBeforeTheBody(t *testing.T) {
	early := sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
		if rc.Request.Header.Get("X-Read") != "" {
			_, _ = io.Copy(io.Discard, rc.Request.Body)
		}

		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "early")
	})

	server := httptest.NewServer(newGateway(t, newSettings(), []service.Registration{early}).Handler())
	defer server.Close()

	const stated = 100_000

	head := func(extra string) string {
		return "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
			"X-Amz-Target: AmazonSQS.SendMessage\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n" +
			extra + "Content-Length: " + strconv.Itoa(stated) + "\r\n\r\n"
	}

	dial := func(t *testing.T) (net.Conn, *bufio.Reader) {
		t.Helper()

		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		return conn, bufio.NewReader(conn)
	}

	// answer reads the answer and returns its status and whether it closes
	// the connection.
	answer := func(t *testing.T, conn net.Conn, reader *bufio.Reader) (int, bool) {
		t.Helper()

		require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))

		resp, err := http.ReadResponse(reader, nil)
		require.NoError(t, err, "the answer arrives")

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "early", string(body))

		return resp.StatusCode, resp.Close
	}

	t.Run("a client that stopped sending gets the answer and the connection back", func(t *testing.T) {
		conn, reader := dial(t)

		_, err := conn.Write([]byte(head("") + "{" + strings.Repeat(" ", 999)))
		require.NoError(t, err)

		status, closes := answer(t, conn, reader)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.True(t, closes, "the answer closes the connection")

		_, err = reader.ReadByte()
		assert.ErrorIs(t, err, io.EOF, "the server closes the connection")
	})

	t.Run("a client that sends its whole body reads the answer", func(t *testing.T) {
		conn, reader := dial(t)

		_, err := conn.Write([]byte(head("") + "{" + strings.Repeat(" ", stated-2) + "}"))
		require.NoError(t, err)

		status, _ := answer(t, conn, reader)
		assert.Equal(t, http.StatusBadRequest, status)
	})

	t.Run("a handler that reads its body keeps the connection", func(t *testing.T) {
		conn, reader := dial(t)

		for range 2 {
			_, err := conn.Write([]byte(head("X-Read: 1\r\n") + "{" + strings.Repeat(" ", stated-2) + "}"))
			require.NoError(t, err)

			_, closes := answer(t, conn, reader)
			assert.False(t, closes, "an answer after the whole body keeps the connection")
		}
	})
}
