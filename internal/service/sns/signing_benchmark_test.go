package sns_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
)

// BenchmarkSigningCertificate fetches the signing certificate through the real
// gateway, in process. The cell axes are the method (HEAD reads no body) and
// the callers fetching at once. The key pair is generated before the timer
// starts, so the first fetch does not count the one-time RSA-2048 generation.
//
//	BenchmarkSigningCertificate/method=GET/concurrency=16
func BenchmarkSigningCertificate(b *testing.B) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, workers := range benchConcurrency {
			b.Run(fmt.Sprintf("method=%s/concurrency=%d", method, workers), func(b *testing.B) {
				benchCertificate(b, method, workers)
			})
		}
	}
}

func benchCertificate(b *testing.B, method string, workers int) {
	provider := sns.New(nil, nil)

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration()}, provider.MountProtocol, nil)
	if err != nil {
		b.Fatal(err)
	}

	handler := g.Handler()

	fetchCert := func() int {
		req := httptest.NewRequestWithContext(b.Context(), method, "http://localhost:4566"+envelope.CertPath, http.NoBody)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		return w.Code
	}

	if status := fetchCert(); status != http.StatusOK {
		b.Fatalf("the certificate answered %d", status)
	}

	var fail failed

	b.ReportAllocs()

	for b.Loop() {
		fanOut(workers, func(int) { fail.check(fetchCert(), "") })

		if fail.msg != "" {
			b.Fatal("the certificate answered " + fail.msg)
		}
	}
}
