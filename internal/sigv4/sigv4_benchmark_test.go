package sigv4_test

import (
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/sigv4"
)

// BenchmarkSign recomputes a presigned request's signature, the work a service
// does per checked request, across the number of signed headers.
func BenchmarkSign(b *testing.B) {
	query := []sigv4.Pair{
		{Key: "X-Amz-Algorithm", Value: sigv4.Algorithm},
		{Key: "X-Amz-Credential", Value: "test/20261118/us-east-1/s3/aws4_request"},
		{Key: "X-Amz-Date", Value: "20261118T121823Z"},
		{Key: "X-Amz-Expires", Value: "60"},
		{Key: "X-Amz-SignedHeaders", Value: "host"},
	}

	for _, n := range []int{1, 8, 32} {
		headers := []sigv4.Header{{Name: host, Value: "s3.localhost.localstack.cloud:4566"}}
		for i := 1; i < n; i++ {
			headers = append(headers, sigv4.Header{Name: "x-amz-meta-h" + strconv.Itoa(i), Value: " a  value "})
		}

		b.Run("headers="+strconv.Itoa(n), func(b *testing.B) {
			for b.Loop() {
				block, signed := sigv4.CanonicalHeaders(headers)
				request := sigv4.CanonicalRequest("GET", sigv4.EscapePath("/bucket/some key"),
					sigv4.CanonicalQuery(query), block, signed, sigv4.UnsignedPayload)
				toSign := sigv4.StringToSign("20261118T121823Z", sigv4.Scope("20261118", "us-east-1", "s3"), request)
				_ = sigv4.Signature(sigv4.SigningKey("test", "20261118", "us-east-1", "s3"), toSign)
			}
		})
	}
}
