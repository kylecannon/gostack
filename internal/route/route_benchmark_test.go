package route_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/stretchr/testify/require"
)

func BenchmarkRouteDetect(b *testing.B) {
	table, err := route.NewTable(registered(sqs, sts, sns))
	require.NoError(b, err)

	signed := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "http://localhost:4566/", nil)
	signed.Header.Set(headerAuth, scope(nameSQS))
	signed.Header.Set(headerTarget, targetSendMessage)
	signed.Header.Set(headerType, "application/x-amz-json-1.0")

	hosted := httptest.NewRequestWithContext(
		b.Context(), http.MethodPost, "http://sts.localhost.localstack.cloud:4566/", nil)
	hosted.Header.Set(headerType, "application/x-www-form-urlencoded")

	b.Run("signing name and target", func(b *testing.B) {
		for b.Loop() {
			match, err := table.Detect(signed, nil)
			if err != nil || match.Protocol != protocol.KindJSON {
				b.Fatal("detect failed")
			}
		}
	})

	b.Run("host", func(b *testing.B) {
		for b.Loop() {
			_, err := table.Detect(hosted, nil)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
