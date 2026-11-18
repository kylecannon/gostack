package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/require"
)

// The object benchmark matrix (docs/standards.md, "Benchmarks"): PutObject and
// GetObject through the real gateway over HTTP, across the payload axis and at
// concurrency 1 and 16. Sub-benchmark names carry the axes, for example
//
//	BenchmarkPutObject/payload=256KiB/concurrency=16
//
// At concurrency 16 one iteration is 16 simultaneous calls, so ns/op and the
// byte rate are per sixteen calls. The 1 KiB and 256 KiB bodies stay in memory
// and the 1 MiB and 16 MiB bodies spill to a file, so the four sizes cover both
// storage paths. The system bench measures against LocalStack.

const benchAuth = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0"

var (
	benchPayloads    = []benchPayload{{"1KiB", 1 << 10}, {"256KiB", 256 << 10}, {"1MiB", 1 << 20}, {"16MiB", 16 << 20}}
	benchConcurrency = []int{1, 16}
)

type benchPayload struct {
	name string
	size int
}

// objectBenchServer runs the real gateway with S3 alone registered, and a client that
// keeps a connection for every worker.
type objectBenchServer struct {
	url    string
	client *http.Client
}

func newObjectBenchServer(b *testing.B) *objectBenchServer {
	b.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(b.TempDir()))
	b.Cleanup(provider.Close)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(b, err)

	srv := httptest.NewServer(gw.Handler())
	b.Cleanup(srv.Close)

	transport := &http.Transport{MaxIdleConnsPerHost: 64}
	b.Cleanup(transport.CloseIdleConnections)

	s := &objectBenchServer{url: srv.URL, client: &http.Client{Transport: transport}}

	status, err := s.call(b.Context(), http.MethodPut, "/bucket", nil)
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, status)

	return s
}

// call sends one request and reads the whole response, returning its status.
func (s *objectBenchServer) call(ctx context.Context, method, target string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.url+target, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", benchAuth)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return resp.StatusCode, nil
}

// fanOut runs fn on workers goroutines at once and returns the first error.
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

// benchMatrix runs one sub-benchmark for every payload at every concurrency.
func benchMatrix(b *testing.B, run func(b *testing.B, s *objectBenchServer, p benchPayload, workers int)) {
	b.Helper()

	for _, payload := range benchPayloads {
		for _, workers := range benchConcurrency {
			b.Run(fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers), func(b *testing.B) {
				run(b, newObjectBenchServer(b), payload, workers)
			})
		}
	}
}

func BenchmarkPutObject(b *testing.B) {
	benchMatrix(b, func(b *testing.B, s *objectBenchServer, p benchPayload, workers int) {
		body := bytes.Repeat([]byte("x"), p.size)

		b.SetBytes(int64(p.size * workers))
		b.ReportAllocs()

		for b.Loop() {
			err := fanOut(workers, func(worker int) error {
				status, err := s.call(b.Context(), http.MethodPut, fmt.Sprintf("/bucket/key-%d", worker), body)
				if err == nil && status != http.StatusOK {
					err = fmt.Errorf("PUT answered %d", status)
				}

				return err
			})
			require.NoError(b, err)
		}
	})
}

func BenchmarkGetObject(b *testing.B) {
	benchMatrix(b, func(b *testing.B, s *objectBenchServer, p benchPayload, workers int) {
		body := bytes.Repeat([]byte("x"), p.size)

		for worker := range workers {
			status, err := s.call(b.Context(), http.MethodPut, fmt.Sprintf("/bucket/key-%d", worker), body)
			require.NoError(b, err)
			require.Equal(b, http.StatusOK, status)
		}

		b.SetBytes(int64(p.size * workers))
		b.ReportAllocs()

		for b.Loop() {
			err := fanOut(workers, func(worker int) error {
				status, err := s.call(b.Context(), http.MethodGet, fmt.Sprintf("/bucket/key-%d", worker), nil)
				if err == nil && status != http.StatusOK {
					err = fmt.Errorf("GET answered %d", status)
				}

				return err
			})
			require.NoError(b, err)
		}
	})
}
