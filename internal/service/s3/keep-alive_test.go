package s3_test

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAStreamedPutObjectKeepsTheConnection is SQ-320. The gateway closes the
// connection after an answer whose request body was not read to its end
// (gateway.unreadBody), so S3 must read a PutObject's body to its end, the
// framing after the last aws-chunked chunk included, or every streamed upload
// would cost the client a new connection. Over HTTP/1.1, each PutObject below
// is answered without Connection: close, and a GET of the object is served on
// the same connection.
func TestAStreamedPutObjectKeepsTheConnection(t *testing.T) {
	srv := startEdgeServer(t, nil)

	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE([]byte("hello")))
	checksum := base64.StdEncoding.EncodeToString(crc)

	const (
		awsChunked = "5\r\nhello\r\n0\r\n\r\n"
		signed     = "5;chunk-signature=" + "0000000000000000000000000000000000000000000000000000000000000000" +
			"\r\nhello\r\n0;chunk-signature=" + "0000000000000000000000000000000000000000000000000000000000000000" +
			"\r\n\r\n"
	)

	trailer := "5\r\nhello\r\n0\r\nx-amz-checksum-crc32:" + checksum + "\r\n\r\n"

	// httpChunked frames body as an HTTP/1.1 chunked body of one chunk.
	httpChunked := func(body string) string {
		return strconv.FormatInt(int64(len(body)), 16) + "\r\n" + body + "\r\n0\r\n\r\n"
	}

	streaming := []string{
		"Content-Encoding: aws-chunked",
		"X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"X-Amz-Decoded-Content-Length: 5",
	}

	for _, tt := range []struct {
		name   string
		header []string
		body   string
	}{
		{"an HTTP chunked body", []string{"Transfer-Encoding: chunked"}, httpChunked("hello")},
		{"an aws-chunked body with Content-Length", streaming, awsChunked},
		{
			"an aws-chunked body with a trailer",
			append([]string{"X-Amz-Trailer: x-amz-checksum-crc32"}, streaming...),
			trailer,
		},
		{
			"a signed aws-chunked body",
			[]string{
				"Content-Encoding: aws-chunked",
				"X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
				"X-Amz-Decoded-Content-Length: 5",
			},
			signed,
		},
		{
			"an aws-chunked body sent HTTP chunked",
			append([]string{"Transfer-Encoding: chunked", "X-Amz-Trailer: x-amz-checksum-crc32"}, streaming...),
			httpChunked(trailer),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
			require.NoError(t, err)

			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

			reader := bufio.NewReader(conn)

			// send writes one request with body; a body framed HTTP chunked
			// states no Content-Length.
			send := func(method, target string, header []string, body string) *http.Response {
				lines := append([]string{
					method + " " + target + " HTTP/1.1",
					"Host: " + srv.Listener.Addr().String(),
					"Authorization: " + rawAuthorization,
				}, header...)

				if !strings.Contains(strings.Join(header, "\n"), "Transfer-Encoding") {
					lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)))
				}

				_, werr := io.WriteString(conn, strings.Join(lines, "\r\n")+headEnd+body)
				require.NoError(t, werr)

				resp, rerr := http.ReadResponse(reader, nil)
				require.NoError(t, rerr, "the answer arrives on the connection")

				return resp
			}

			resp := send(http.MethodPut, bucketPath, nil, "")
			require.NoError(t, resp.Body.Close())

			resp = send(http.MethodPut, bucketPath+"/key", tt.header, tt.body)
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, http.StatusOK, resp.StatusCode, string(got))
			assert.False(t, resp.Close, "the PutObject answer keeps the connection")

			resp = send(http.MethodGet, bucketPath+"/key", nil, "")
			got, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "hello", string(got), "the GET is served on the same connection")
		})
	}
}
