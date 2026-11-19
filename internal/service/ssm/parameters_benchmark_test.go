package ssm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// The Parameter Store benchmark matrix (docs/standards.md, "Benchmarks"):
// GetParameter, GetParametersByPath and PutParameter through the real gateway
// handler, with no network between, and the snapshot and restore of the same
// stores. Sub-benchmark names carry the axes, for example
//
//	BenchmarkGetParametersByPath/params=10000/concurrency=16
//
// params is how many parameters the account and region hold, all under one
// path. At concurrency 16 one iteration is 16 simultaneous calls, so ns/op is
// per sixteen calls. Reads never mark SSM dirty, so they carry no persistence
// axis; PutParameter's persist=on rows count the MarkDirty a write adds. The
// system bench measures against LocalStack.

const (
	benchPath     = "/bench"
	benchEndpoint = "http://localhost:4566/"
)

var (
	benchSizes       = []int{10, 10_000}
	benchConcurrency = []int{1, 16, 64}
	// benchPayloads are a small value and the 4 KiB limit of a Standard tier
	// parameter.
	benchPayloads = []struct {
		name string
		size int
	}{{"64B", 64}, {"4KiB", 4 << 10}}
	benchAuth = "AWS4-HMAC-SHA256 Credential=test/20240101/us-east-1/ssm/aws4_request, " +
		"SignedHeaders=host, Signature=x"
)

// sink is a ResponseWriter that keeps the status and drops the body.
type sink struct {
	header http.Header
	status int
}

func newSink() *sink { return &sink{header: http.Header{}, status: http.StatusOK} }

func (s *sink) Header() http.Header { return s.header }

func (s *sink) WriteHeader(status int) { s.status = status }

func (*sink) Write(p []byte) (int, error) { return len(p), nil }

func (s *sink) reset() {
	s.status = http.StatusOK
	clear(s.header)
}

// benchHeader is the header of a json 1.1 call of op.
func benchHeader(op string) http.Header {
	return http.Header{
		"Authorization": {benchAuth},
		"Content-Type":  {"application/x-amz-json-1.1"},
		"X-Amz-Target":  {"AmazonSSM." + op},
	}
}

// benchBody is v as json.
func benchBody(b *testing.B, v any) []byte {
	b.Helper()

	raw, err := json.Marshal(v)
	require.NoError(b, err)

	return raw
}

// paramName is the name of the parameter numbered i.
func paramName(i int) string { return benchPath + "/p" + strconv.Itoa(i) }

// send serves one call of op with body and fails on any answer but 200.
func send(h http.Handler, w *sink, op string, body []byte) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, benchEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s: %w", op, err)
	}

	req.Header = benchHeader(op)
	w.reset()
	h.ServeHTTP(w, req)

	if w.status != http.StatusOK {
		return fmt.Errorf("%s answered %d", op, w.status)
	}

	return nil
}

// filled returns a provider whose default account and region hold n String
// parameters under benchPath, and the handler in front of it.
func filled(b *testing.B, n int) (*ssm.Provider, http.Handler) {
	b.Helper()

	p := ssm.New()
	h := handlerFor(b, p, nil)
	w := newSink()

	for i := range n {
		body := benchBody(b, obj{kName: paramName(i), kValue: "value-" + strconv.Itoa(i), kType: kString})
		require.NoError(b, send(h, w, kPutParameter, body))
	}

	return p, h
}

// fanOut runs fn once on each of workers goroutines and returns the first error.
func fanOut(workers int, fn func(worker int) error) error {
	errs := make([]error, workers)

	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() { errs[worker] = fn(worker) })
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// readMatrix runs op at every size and concurrency, each worker sending the
// body its index picks.
func readMatrix(b *testing.B, op string, body func(b *testing.B, n, worker int) []byte) {
	b.Helper()

	for _, n := range benchSizes {
		for _, workers := range benchConcurrency {
			b.Run(fmt.Sprintf("params=%d/concurrency=%d", n, workers), func(b *testing.B) {
				_, h := filled(b, n)

				bodies := make([][]byte, workers)
				sinks := make([]*sink, workers)

				for worker := range workers {
					bodies[worker], sinks[worker] = body(b, n, worker), newSink()
				}

				b.ReportAllocs()

				for b.Loop() {
					err := fanOut(workers, func(worker int) error {
						return send(h, sinks[worker], op, bodies[worker])
					})
					require.NoError(b, err)
				}
			})
		}
	}
}

func BenchmarkGetParameter(b *testing.B) {
	readMatrix(b, kGetParameter, func(b *testing.B, n, worker int) []byte {
		return benchBody(b, obj{kName: paramName(worker * (n / 64) % n)})
	})
}

func BenchmarkGetParametersByPath(b *testing.B) {
	readMatrix(b, kGetParametersByPath, func(b *testing.B, _, _ int) []byte {
		return benchBody(b, obj{kPath: benchPath})
	})
}

// BenchmarkPutParameter overwrites one parameter per worker, across the
// payload axis, with persistence off and then on.
func BenchmarkPutParameter(b *testing.B) {
	for _, persisted := range []bool{false, true} {
		for _, payload := range benchPayloads {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers)
				if persisted {
					name = "persist=on/" + name
				}

				b.Run(name, func(b *testing.B) {
					var eng gateway.Dirtier
					if persisted {
						eng = &marks{}
					}

					h := handlerFor(b, ssm.New(), eng)
					value := strings.Repeat("x", payload.size)
					bodies := make([][]byte, workers)
					sinks := make([]*sink, workers)

					for worker := range workers {
						bodies[worker] = benchBody(b, obj{
							kName: paramName(worker), kValue: value, kType: kString, kOverwrite: true,
						})
						sinks[worker] = newSink()
					}

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						err := fanOut(workers, func(worker int) error {
							return send(h, sinks[worker], kPutParameter, bodies[worker])
						})
						require.NoError(b, err)
					}
				})
			}
		}
	}
}

// countingWriter counts the bytes written to it.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)

	return len(p), nil
}

func BenchmarkSnapshot(b *testing.B) {
	for _, n := range benchSizes {
		b.Run("params="+strconv.Itoa(n), func(b *testing.B) {
			p, _ := filled(b, n)

			var file countingWriter

			b.ReportAllocs()

			for b.Loop() {
				file.n = 0
				require.NoError(b, p.Snapshot(&file))
			}

			b.SetBytes(int64(file.n))
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, n := range benchSizes {
		b.Run("params="+strconv.Itoa(n), func(b *testing.B) {
			p, _ := filled(b, n)

			var file bytes.Buffer
			require.NoError(b, p.Snapshot(&file))

			raw := file.Bytes()
			target := ssm.New()

			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, target.Restore(bytes.NewReader(raw)))
			}
		})
	}
}
