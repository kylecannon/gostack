package awschunked_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awschunked"
)

// FuzzReader decodes any body with any declared length: it must not panic,
// must stop, must fail only with ErrMalformed or ErrLength, and must hand out
// exactly the declared length when it succeeds. It also encodes the input as
// a payload and requires the decoder to give it back with its trailer.
func FuzzReader(f *testing.F) {
	f.Add([]byte("d"+signature+"\r\nHello\r\n\r\n\r\n\r\n\r\n0"+signature+"\r\n"), int64(13), uint16(5))
	f.Add([]byte("23\r\ntest;test;test\r\ntest1;test1;test1\r\n\r\n0\r\nx-amz-checksum-crc32:AKHICA==\r\n\r\n"),
		int64(35), uint16(1))
	f.Add([]byte("0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"), int64(0), uint16(64))
	f.Add([]byte("\r\nHello Blob\r\n0"+signature+"\r\n"), int64(10), uint16(3))
	f.Add([]byte("2\r\nab\r\n1\r\nc\r\n0"), int64(-1), uint16(2))

	f.Fuzz(func(t *testing.T, body []byte, decodedLength int64, chunkSize uint16) {
		r := awschunked.NewReader(bytes.NewReader(body), decodedLength)

		payload, err := io.ReadAll(r)
		switch {
		case err == nil:
			if decodedLength >= 0 && int64(len(payload)) != decodedLength {
				t.Fatalf("decoded %d bytes, declared %d", len(payload), decodedLength)
			}
		case !errors.Is(err, awschunked.ErrMalformed) && !errors.Is(err, awschunked.ErrLength):
			t.Fatalf("error is neither ErrMalformed nor ErrLength: %v", err)
		default:
		}

		ext := ""
		if chunkSize%2 == 0 {
			ext = signature
		}

		encoded := encode(body, int(chunkSize)+1, ext, "x-amz-checksum-crc32:AAAAAA==")
		back := awschunked.NewReader(bytes.NewReader(encoded), int64(len(body)))

		decoded, err := io.ReadAll(back)
		if err != nil {
			t.Fatalf("decode an encoded payload: %v", err)
		}

		if !bytes.Equal(decoded, body) {
			t.Fatal("round trip changed the payload")
		}

		if got := back.Trailer().Get("X-Amz-Checksum-Crc32"); got != "AAAAAA==" {
			t.Fatalf("trailer %q", got)
		}
	})
}
