package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

// noop answers an empty 200, so the benchmarks measure the gateway.
func noop(w http.ResponseWriter, _ *request.Context) {
	w.WriteHeader(http.StatusOK)
}

func BenchmarkGatewayServeNoop(b *testing.B) {
	g, err := gateway.New(newSettings().get, []service.Registration{sqsRegistration(noop)}, nil, nil)
	require.NoError(b, err)

	b.Run("handler", func(b *testing.B) {
		r := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "http://localhost:4566/", nil)
		r.Header.Set(headerAuth, signed("test", usEast1, nameSQS))
		r.Header.Set(headerTarget, targetSendMessage)
		r.Header.Set(headerType, jsonType)

		for b.Loop() {
			g.Handler().ServeHTTP(httptest.NewRecorder(), r)
		}
	})

	b.Run("round trip", func(b *testing.B) {
		server := httptest.NewServer(g.Handler())
		b.Cleanup(server.Close)

		for b.Loop() {
			r, err := http.NewRequestWithContext(b.Context(), http.MethodPost, server.URL+"/", nil)
			if err != nil {
				b.Fatal(err)
			}

			r.Header.Set(headerAuth, signed("test", usEast1, nameSQS))
			r.Header.Set(headerTarget, targetSendMessage)
			r.Header.Set(headerType, jsonType)

			resp, err := server.Client().Do(r)
			if err != nil {
				b.Fatal(err)
			}

			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}
