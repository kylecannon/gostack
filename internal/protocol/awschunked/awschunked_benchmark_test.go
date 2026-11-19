package awschunked_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awschunked"
)

// BenchmarkDecode measures decode throughput across the payload axis (small,
// medium and an 8 MiB part) and the two chunk sizes SDKs send: the 64 KiB of
// the Java and Go SDKs and the 8 KiB of botocore's legacy streaming, signed
// and unsigned.
func BenchmarkDecode(b *testing.B) {
	payloads := []struct {
		name string
		size int
	}{
		{"1KiB", 1 << 10},
		{"64KiB", 64 << 10},
		{"8MiB", 8 << 20},
	}

	for _, payload := range payloads {
		for _, chunk := range []struct {
			name string
			size int
		}{{"8KiB", 8 << 10}, {"64KiB", 64 << 10}} {
			for _, signing := range []struct{ name, ext string }{{"yes", signature}, {"no", ""}} {
				name := "payload=" + payload.name + "/chunk=" + chunk.name + "/signed=" + signing.name

				b.Run(name, func(b *testing.B) {
					data := bytes.Repeat([]byte{'x'}, payload.size)
					body := encode(data, chunk.size, signing.ext, "x-amz-checksum-crc32:AAAAAA==")
					src := bytes.NewReader(body)
					buf := make([]byte, 32<<10)

					b.SetBytes(int64(payload.size))
					b.ReportAllocs()

					for b.Loop() {
						src.Reset(body)

						_, err := io.CopyBuffer(io.Discard, awschunked.NewReader(src, int64(payload.size)), buf)
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
