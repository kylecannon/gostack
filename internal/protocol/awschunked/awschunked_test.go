package awschunked_test

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/kylecannon/gostack/internal/protocol/awschunked"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const signature = ";chunk-signature=af5e6c0a698b0192e9aa5d9083553d4d241d81f69ec62b184d05c509ad5166af"

// encode writes payload as an aws-chunked body of chunkSize chunks, each
// header carrying the extension ext, with trailer lines after the last chunk.
func encode(payload []byte, chunkSize int, ext string, trailer ...string) []byte {
	var b bytes.Buffer

	for chunk := range slicesChunk(payload, chunkSize) {
		b.WriteString(strconv.FormatInt(int64(len(chunk)), 16) + ext + "\r\n")
		b.Write(chunk)
		b.WriteString("\r\n")
	}

	b.WriteString("0" + ext + "\r\n")

	for _, line := range trailer {
		b.WriteString(line + "\r\n")
	}

	if len(trailer) > 0 {
		b.WriteString("\r\n")
	}

	return b.Bytes()
}

// slicesChunk yields payload in pieces of at most size bytes, and nothing for
// an empty payload.
func slicesChunk(payload []byte, size int) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for len(payload) > 0 {
			n := min(size, len(payload))
			if !yield(payload[:n]) {
				return
			}

			payload = payload[n:]
		}
	}
}

func decode(body string, decodedLength int64) ([]byte, http.Header, error) {
	r := awschunked.NewReader(strings.NewReader(body), decodedLength)
	payload, err := io.ReadAll(r)

	return payload, r.Trailer(), err
}

func TestReader(t *testing.T) {
	tests := []struct {
		name        string
		give        string
		giveLength  int64
		wantPayload string
		wantTrailer http.Header
	}{
		{
			name:        "signed chunk whose payload holds CRLFs",
			give:        "d" + signature + "\r\nHello\r\n\r\n\r\n\r\n\r\n0" + signature + "\r\n",
			giveLength:  13,
			wantPayload: "Hello\r\n\r\n\r\n\r\n",
		},
		{
			name:        "unsigned chunk with a checksum trailer",
			give:        "23\r\ntest;test;test\r\ntest1;test1;test1\r\n\r\n0\r\nx-amz-checksum-crc32:AKHICA==\r\n\r\n",
			giveLength:  35,
			wantPayload: "test;test;test\r\ntest1;test1;test1\r\n",
			wantTrailer: http.Header{"X-Amz-Checksum-Crc32": {"AKHICA=="}},
		},
		{
			name:        "empty payload with a trailer",
			give:        "0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n",
			giveLength:  0,
			wantTrailer: http.Header{"X-Amz-Checksum-Crc32": {"AAAAAA=="}},
		},
		{
			name: "signed trailer",
			give: "a" + signature + "\r\nHello Blob\r\n0" + signature + "\r\n" +
				"x-amz-checksum-sha256: abc=\r\nx-amz-trailer-signature:712f\r\n\r\n",
			giveLength:  10,
			wantPayload: "Hello Blob",
			wantTrailer: http.Header{"X-Amz-Checksum-Sha256": {"abc="}, "X-Amz-Trailer-Signature": {"712f"}},
		},
		{
			name:        "several chunks and no declared length",
			give:        "3\r\nabc\r\n2;ext\r\nde\r\n1\r\nf\r\n0\r\n\r\n",
			giveLength:  -1,
			wantPayload: "abcdef",
		},
		{
			name:        "last chunk header without a newline",
			give:        "2\r\nhi\r\n0",
			giveLength:  2,
			wantPayload: "hi",
		},
		{
			name:        "upper-case hex size",
			give:        "A\r\n0123456789\r\n0\r\n",
			giveLength:  10,
			wantPayload: "0123456789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, trailer, err := decode(tt.give, tt.giveLength)
			require.NoError(t, err)
			assert.Equal(t, tt.wantPayload, string(payload))
			assert.Equal(t, tt.wantTrailer, trailer)
		})
	}
}

func TestReaderRefusesAMalformedBody(t *testing.T) {
	tests := []struct {
		name       string
		give       string
		giveLength int64
		wantErr    error
	}{
		{"empty size line", "\r\nHello Blob\r\n0" + signature + "\r\n", 10, awschunked.ErrMalformed},
		{"size that is not hex", "zz\r\nab\r\n0\r\n", -1, awschunked.ErrMalformed},
		{"negative size", "-2\r\nab\r\n0\r\n", -1, awschunked.ErrMalformed},
		{"size past int64", "8000000000000000\r\nab\r\n0\r\n", -1, awschunked.ErrMalformed},
		{"empty body", "", -1, awschunked.ErrMalformed},
		{"body that ends inside a chunk", "5\r\nab", -1, awschunked.ErrMalformed},
		{"body that ends before the last chunk", "2\r\nab\r\n", -1, awschunked.ErrMalformed},
		{"chunk without its CRLF", "2\r\nabX\r\n0\r\n", -1, awschunked.ErrMalformed},
		{"trailer without a colon", "0\r\nx-amz-checksum-crc32\r\n\r\n", -1, awschunked.ErrMalformed},
		{"trailer without a name", "0\r\n: value\r\n\r\n", -1, awschunked.ErrMalformed},
		{"chunk header past the line limit", strings.Repeat("1", 5000) + "\r\n", -1, awschunked.ErrMalformed},
		{"trailers past their limit", "0\r\n" + strings.Repeat("x-a: b\r\n", 3000), -1, awschunked.ErrMalformed},
		{"payload shorter than declared", "2\r\nab\r\n0\r\n", 3, awschunked.ErrLength},
		{"payload longer than declared", "2\r\nab\r\n1\r\nc\r\n0\r\n", 2, awschunked.ErrLength},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, trailer, err := decode(tt.give, tt.giveLength)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Nil(t, trailer)
		})
	}
}

func TestReaderStreamsAnyReadSize(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef\r\n"), 10_000)
	body := encode(payload, 8<<10, signature, "x-amz-checksum-crc32:AAAAAA==")

	r := awschunked.NewReader(iotest.HalfReader(bytes.NewReader(body)), int64(len(payload)))
	require.NoError(t, iotest.TestReader(r, payload))

	one, err := io.ReadAll(awschunked.NewReader(iotest.OneByteReader(bytes.NewReader(body)), int64(len(payload))))
	require.NoError(t, err)
	assert.Equal(t, payload, one)
}

func TestReaderTrailerWaitsForTheEnd(t *testing.T) {
	r := awschunked.NewReader(strings.NewReader("2\r\nab\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n"), 2)

	buf := make([]byte, 2)
	n, err := r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	assert.Nil(t, r.Trailer(), "the trailer is read only after the last chunk")

	n, err = r.Read(buf)
	require.ErrorIs(t, err, io.EOF)
	assert.Zero(t, n)
	assert.Equal(t, "AAAAAA==", r.Trailer().Get("X-Amz-Checksum-Crc32"))
}

func TestReaderPassesOnTheSourceError(t *testing.T) {
	failure := io.ErrClosedPipe
	r := awschunked.NewReader(io.MultiReader(strings.NewReader("5\r\nab"), iotest.ErrReader(failure)), -1)

	_, err := io.ReadAll(r)
	require.ErrorIs(t, err, failure)
}
