package sts_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sts"
)

// sink is the ResponseWriter of a benchmark. It keeps the status and the size,
// so the measurement is the server's work and not a recorder's.
type sink struct {
	header http.Header
	status int
	size   int
}

func (s *sink) Header() http.Header { return s.header }

func (s *sink) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
}

func (s *sink) Write(p []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	s.size += len(p)

	return len(p), nil
}

// BenchmarkGetCallerIdentity calls GetCallerIdentity through the real gateway.
// STS speaks one wire protocol, query. A caller that sends "Accept:
// application/json" gets the answer as json, which is a second path through the
// service, so both are cells. STS has no payload or queue axis, and it keeps no
// state, so it has no persistence cell.
func BenchmarkGetCallerIdentity(b *testing.B) {
	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{sts.Registration(nil)}, nil, nil,
	)
	if err != nil {
		b.Fatal(err)
	}

	form := "Action=GetCallerIdentity&Version=2011-06-15"
	auth := "AWS4-HMAC-SHA256 Credential=test/20130524/" + region +
		"/sts/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc"

	for _, accept := range []struct{ name, header string }{{"xml", ""}, {"json", "application/json"}} {
		b.Run("proto=query/accept="+accept.name, func(b *testing.B) {
			header := http.Header{
				"Content-Type":  {"application/x-www-form-urlencoded; charset=utf-8"},
				"Authorization": {auth},
			}

			if accept.header != "" {
				header.Set("Accept", accept.header)
			}

			w := &sink{header: http.Header{}}

			b.ReportAllocs()

			for b.Loop() {
				clear(w.header)
				w.status, w.size = 0, 0

				req, err := http.NewRequestWithContext(
					b.Context(), http.MethodPost, "http://localhost:4566/", strings.NewReader(form),
				)
				if err != nil {
					b.Fatal(err)
				}

				req.Header = header

				g.Handler().ServeHTTP(w, req)

				if w.status != http.StatusOK {
					b.Fatalf("status %d", w.status)
				}
			}
		})
	}
}
