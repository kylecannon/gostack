package s3_test

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAStreamedPutObjectKeepsTheConnection is SQ-320. S3 reads a PutObject's
// body to its end, the framing after the last aws-chunked chunk included,
// before it answers, so a streamed upload keeps its HTTP/1.1 connection. Each
// body below is over 64 KiB, in several chunks, and is sent without its last
// bytes (the CRLF that ends an aws-chunked body, or the last chunk of an HTTP
// chunked one): S3 must not answer until they arrive, and then answers 200
// without Connection: close, and a GET of the object is served on the same
// connection. A decoder that stopped at the last payload byte would answer
// before they arrive.
func TestAStreamedPutObjectKeepsTheConnection(t *testing.T) {
	srv := startEdgeServer(t, nil)

	const chunk = 64 << 10

	payload := strings.Repeat("abcdefgh", (3*chunk+chunk/2)/8)

	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE([]byte(payload)))
	trailerLine := "x-amz-checksum-crc32:" + base64.StdEncoding.EncodeToString(crc) + "\r\n"

	// awsChunked frames payload as aws-chunked, each chunk header given ext,
	// followed by trailer lines and the blank line that ends the body.
	awsChunked := func(ext, trailer string) string {
		var b strings.Builder

		for rest := payload; rest != ""; {
			n := min(chunk, len(rest))
			b.WriteString(strconv.FormatInt(int64(n), 16) + ext + "\r\n" + rest[:n] + "\r\n")
			rest = rest[n:]
		}

		b.WriteString("0" + ext + "\r\n" + trailer + "\r\n")

		return b.String()
	}

	// httpChunked frames body as an HTTP/1.1 chunked body.
	httpChunked := func(body string) string {
		var b strings.Builder

		for rest := body; rest != ""; {
			n := min(chunk, len(rest))
			b.WriteString(strconv.FormatInt(int64(n), 16) + "\r\n" + rest[:n] + "\r\n")
			rest = rest[n:]
		}

		b.WriteString("0\r\n\r\n")

		return b.String()
	}

	signature := ";chunk-signature=" + strings.Repeat("0", 64)
	decoded := "X-Amz-Decoded-Content-Length: " + strconv.Itoa(len(payload))
	unsigned := []string{
		"Content-Encoding: aws-chunked", "X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER", decoded,
	}
	withTrailer := append([]string{"X-Amz-Trailer: x-amz-checksum-crc32"}, unsigned...)
	chunked := "Transfer-Encoding: chunked"

	const (
		lastCRLF      = len("\r\n")
		lastHTTPChunk = len("0\r\n\r\n")
	)

	for _, tt := range []struct {
		name   string
		header []string
		body   string
		tail   int // the bytes held back until S3 is seen to wait for them
	}{
		{"an HTTP chunked body", []string{chunked}, httpChunked(payload), lastHTTPChunk},
		{"an aws-chunked body with Content-Length", unsigned, awsChunked("", ""), lastCRLF},
		{"an aws-chunked body with a trailer", withTrailer, awsChunked("", trailerLine), lastCRLF},
		{
			"a signed aws-chunked body",
			[]string{"Content-Encoding: aws-chunked", "X-Amz-Content-Sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD", decoded},
			awsChunked(signature, ""),
			lastCRLF,
		},
		{
			"an aws-chunked body sent HTTP chunked",
			append([]string{chunked}, withTrailer...),
			httpChunked(awsChunked("", trailerLine)),
			lastHTTPChunk,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
			require.NoError(t, err)

			t.Cleanup(func() { _ = conn.Close() })

			reader := bufio.NewReader(conn)

			// write sends one request whose body ends with tail held back; a
			// body framed HTTP chunked states no Content-Length.
			write := func(method, target string, header []string, body string, tail int) {
				lines := append([]string{
					method + " " + target + " HTTP/1.1",
					"Host: " + srv.Listener.Addr().String(),
					"Authorization: " + rawAuthorization,
				}, header...)

				if !strings.Contains(strings.Join(header, "\n"), "Transfer-Encoding") {
					lines = append(lines, "Content-Length: "+strconv.Itoa(len(body)))
				}

				_, werr := io.WriteString(conn, strings.Join(lines, "\r\n")+headEnd+body[:len(body)-tail])
				require.NoError(t, werr)
			}

			// answer reads one answer: its status, whether it closes the
			// connection, and its body.
			answer := func() (status int, closes bool, body string) {
				require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

				resp, rerr := http.ReadResponse(reader, nil)
				require.NoError(t, rerr, "the answer arrives on the connection")

				defer func() { require.NoError(t, resp.Body.Close()) }()

				got, rerr := io.ReadAll(resp.Body)
				require.NoError(t, rerr)

				return resp.StatusCode, resp.Close, string(got)
			}

			write(http.MethodPut, bucketPath, nil, "", 0)
			answer()

			write(http.MethodPut, bucketPath+"/key", tt.header, tt.body, tt.tail)

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))

			_, err = reader.Peek(1)
			require.ErrorIs(t, err, os.ErrDeadlineExceeded, "S3 waits for the end of the body before it answers")

			_, err = io.WriteString(conn, tt.body[len(tt.body)-tt.tail:])
			require.NoError(t, err)

			status, closes, got := answer()
			require.Equal(t, http.StatusOK, status, got)
			assert.False(t, closes, "the PutObject answer keeps the connection")

			write(http.MethodGet, bucketPath+"/key", nil, "", 0)

			status, _, got = answer()
			assert.Equal(t, http.StatusOK, status)
			assert.Len(t, got, len(payload), "the GET is served on the same connection")
			assert.Equal(t, crc32.ChecksumIEEE([]byte(payload)), crc32.ChecksumIEEE([]byte(got)), "with the object stored")
		})
	}
}
