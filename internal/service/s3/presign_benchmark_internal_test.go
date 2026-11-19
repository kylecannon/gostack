package s3

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkCheckPresigned measures the presign check of a GET, by signature
// version and with S3_SKIP_SIGNATURE_VALIDATION off and on:
//
//	BenchmarkCheckPresigned/sig=v4/validate=on
//
// validate=off reads the parameters and moves the query's headers; on also
// computes the signature and checks the expiry.
func BenchmarkCheckPresigned(b *testing.B) {
	now := time.Now()
	queries := []struct {
		name  string
		query string
	}{
		{"v2", presignV2(now.Add(time.Hour))},
		{"v4", presignV4(goldenHost, now, 3600)},
	}

	for _, q := range queries {
		for _, validate := range []bool{false, true} {
			name := "sig=" + q.name + "/validate=off"
			if validate {
				name = "sig=" + q.name + "/validate=on"
			}

			b.Run(name, func(b *testing.B) {
				p := New(func() Settings { return Settings{ValidateSignatures: validate, GatewayPort: 4566} }, nil)
				r := presignedRequest(q.query)
				require.Nil(b, p.checkPresigned(r))

				b.ReportAllocs()

				for b.Loop() {
					_ = p.checkPresigned(r)
				}
			})
		}
	}
}
