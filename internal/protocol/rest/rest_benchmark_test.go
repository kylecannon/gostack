package rest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
)

// BenchmarkPutObjectHeaders decodes the headers of a PutObject request and
// encodes those of its response.
func BenchmarkPutObjectHeaders(b *testing.B) {
	pattern := rest.MustParsePattern("/{Bucket}/{Key+}?x-id=PutObject")

	req := httptest.NewRequestWithContext(b.Context(), http.MethodPut, "/bucket/photos/2019/cat%20one.jpg", nil)
	for k, v := range map[string]string{
		"Content-Type": "image/jpeg", "Content-Length": "65536", "Content-MD5": "1B2M2Y8AsgTpgAmY7PhCfg==",
		"Cache-Control": "max-age=60", "Expires": "Mon, 16 Dec 2019 23:48:18 GMT", "x-amz-acl": "private",
		"x-amz-storage-class": "STANDARD", "x-amz-server-side-encryption": "AES256",
		"x-amz-checksum-crc32": "AAAAAA==", "x-amz-object-lock-retain-until-date": "2030-01-01T00:00:00Z",
		"x-amz-meta-owner": "alice", "x-amz-meta-camera": "x100", "x-amz-tagging": "a=b&c=d",
	} {
		req.Header.Set(k, v)
	}

	b.Run("op=decode", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			labels, _ := pattern.Match(req.URL.EscapedPath())
			d := rest.NewDecoder(req, labels)
			_, _ = d.Label("Bucket"), d.Label("Key")
			_, _, _ = d.Header("Content-Type"), d.HeaderInt64("Content-Length"), d.Header("Content-MD5")
			_, _ = d.Header("Cache-Control"), d.HeaderTime("Expires", "")
			_, _, _ = d.Header("x-amz-acl"), d.Header("x-amz-storage-class"), d.Header("x-amz-server-side-encryption")
			_, _ = d.Header("x-amz-checksum-crc32"), d.HeaderTime("x-amz-object-lock-retain-until-date", rest.TimeISO8601)
			_, _ = d.HeaderMap("x-amz-meta-"), d.Header("x-amz-tagging")
		}
	})

	etag, version := `"9b2cf535f27731c974343645a3985328"`, "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"
	sse, crc := "AES256", "AAAAAA=="
	expiration := &protocol.Time{}

	b.Run("op=encode", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			h := make(http.Header, 6)
			rest.SetString(h, "ETag", &etag)
			rest.SetString(h, "x-amz-version-id", &version)
			rest.SetString(h, "x-amz-server-side-encryption", &sse)
			rest.SetString(h, "x-amz-checksum-crc32", &crc)
			rest.SetTime(h, "Last-Modified", expiration, "")
			rest.SetMap(h, "x-amz-meta-", map[string]string{"owner": "alice"})
		}
	})
}
