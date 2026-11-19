package s3_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
)

// headerCRC32 is the canonical form of x-amz-checksum-crc32.
const headerCRC32 = "X-Amz-Checksum-Crc32"

// BenchmarkMultipartUpload uploads a 64 MiB object in 8 MiB parts through the
// gateway: create, eight UploadParts and the completion that assembles them.
// It runs with no checksum, which costs a full-object CRC64NVME at
// completion, and with a COMPOSITE CRC32 checked on every part.
func BenchmarkMultipartUpload(b *testing.B) {
	const (
		objectSize = 64 << 20
		partBytes  = 8 << 20
	)

	part := bytes.Repeat([]byte("p"), partBytes)
	crc := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(part)))

	for _, variant := range []struct {
		name   string
		create http.Header
		part   http.Header
		// listed is what the completion lists of each part beside its ETag.
		listed string
	}{
		{"checksum=none", http.Header{}, http.Header{}, ""},
		{
			"checksum=crc32-composite",
			http.Header{headerChecksumAlg: {"CRC32"}},
			http.Header{headerCRC32: {crc}},
			"<ChecksumCRC32>" + crc + "</ChecksumCRC32>",
		},
	} {
		b.Run("size=64MiB/part=8MiB/"+variant.name, func(b *testing.B) {
			url := benchServer(b)
			benchSend(b, http.MethodPut, url+"/bench", nil, nil)

			b.SetBytes(objectSize)
			b.ReportAllocs()

			for b.Loop() {
				var started struct {
					UploadID string `xml:"UploadId"`
				}

				must(b, xml.Unmarshal(benchSend(b, http.MethodPost, url+"/bench/key?uploads", variant.create, nil), &started))

				var parts bytes.Buffer

				parts.WriteString("<CompleteMultipartUpload>")

				for n := 1; n <= objectSize/partBytes; n++ {
					target := fmt.Sprintf("%s/bench/key?partNumber=%d&uploadId=%s", url, n, started.UploadID)
					etag := benchPut(b, target, variant.part, part)
					fmt.Fprintf(&parts, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag>%s</Part>", n, etag, variant.listed)
				}

				parts.WriteString("</CompleteMultipartUpload>")
				benchSend(b, http.MethodPost, url+"/bench/key?uploadId="+started.UploadID, nil, parts.Bytes())
			}
		})
	}
}

// benchServer runs the gateway with S3 alone and returns its URL.
func benchServer(b *testing.B) string {
	b.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(b.TempDir()))
	b.Cleanup(func() { provider.Close(context.WithoutCancel(b.Context())) })

	gw, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration()}, nil, nil)
	must(b, err)

	srv := httptest.NewServer(gw.Handler())
	b.Cleanup(srv.Close)

	return srv.URL
}

// benchSend makes a signed S3 call that must answer 200 and returns its body.
func benchSend(b *testing.B, method, url string, header http.Header, body []byte) []byte {
	b.Helper()

	resp := benchDo(b, method, url, header, body)

	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	must(b, err)

	return out
}

// benchPut PUTs body and returns the ETag of the 200 answer.
func benchPut(b *testing.B, url string, header http.Header, body []byte) string {
	b.Helper()

	resp := benchDo(b, http.MethodPut, url, header, body)

	_, err := io.Copy(io.Discard, resp.Body)
	must(b, err)
	must(b, resp.Body.Close())

	return resp.Header.Get("ETag")
}

func benchDo(b *testing.B, method, url string, header http.Header, body []byte) *http.Response {
	b.Helper()

	req, err := http.NewRequestWithContext(b.Context(), method, url, bytes.NewReader(body))
	must(b, err)

	maps.Copy(req.Header, header)

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/"+regionUSEast1+
		"/s3/aws4_request, SignedHeaders=host, Signature=0")

	resp, err := http.DefaultClient.Do(req)
	must(b, err)

	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		b.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, out)
	}

	return resp
}

func must(b *testing.B, err error) {
	b.Helper()

	if err != nil {
		b.Fatal(err)
	}
}
