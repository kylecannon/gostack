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
//	BenchmarkPutObject/payload=256KiB/concurrency=1
//
// At concurrency 16 one iteration is 16 simultaneous calls, so ns/op and the
// byte rate are per sixteen calls. The 1 KiB and 256 KiB bodies stay in memory
// and the 1 MiB body spills to a file, so the three sizes cover both storage
// paths. The system bench measures against LocalStack.
//
// The matrix is cut (docs/standards.md, "Benchmarks"): tooSlowToGate leaves out
// a cell whose iteration moves more bytes than the operation's budget,
// writeBudget or readBudget. There is no 16 MiB body, for no cell of it fits:
// GetObject at 16MiB and one worker took 11.8 ms on a runner, and a PutObject
// of it moves sixteen times what the 4.3 ms PutObject at 1MiB does. The widest
// cells kept on a runner: GetObject 7.1 ms at 1MiB and 16 workers, and,
// persisted, 6.1 ms.

const benchAuth = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0"

var (
	benchPayloads    = []benchPayload{{"1KiB", 1 << 10}, {"256KiB", 256 << 10}, {"1MiB", 1 << 20}}
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
	b.Cleanup(func() { provider.Close(context.WithoutCancel(b.Context())) })

	return serveObjectBench(b, provider.Registration(), nil)
}

// serveObjectBench serves reg through the real gateway and creates the bucket.
// A nil dirtier is persistence off.
func serveObjectBench(b *testing.B, reg service.Registration, dirtier gateway.Dirtier) *objectBenchServer {
	b.Helper()

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{reg},
		nil,
		dirtier,
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

const (
	// writeBudget is the bytes one iteration of a write cell may move. A PUT
	// writes its body to the gateway's store and, from 1 MiB, to a file.
	writeBudget = 1 << 20

	// readBudget is the same for a GetObject, which streams the stored body out
	// and costs a fraction of a PUT of it. 1MiB at 16 workers moves 16 MiB and
	// took 7.1 ms on a runner, so no wider cell is added.
	readBudget = 16 << 20
)

// tooSlowToGate is true for the cells that a run at the gate's 500 ms could not
// make 50 iterations of (docs/standards.md, "Benchmarks"): the 16 workers of an
// iteration at payload move payload.size*workers bytes, and a cell that moves
// more than budget is left out. The payload axis is covered by the widest cell
// under the budget.
func tooSlowToGate(payload benchPayload, workers, budget int) bool {
	return payload.size*workers > budget
}

// benchMatrix runs one sub-benchmark for every payload at every concurrency
// whose iteration is within budget, and then those of persistedPayloads again
// on a persisted server, under persist=on.
func benchMatrix(b *testing.B, budget int, run func(b *testing.B, s *objectBenchServer, p benchPayload, workers int)) {
	b.Helper()

	for _, persisted := range persistRows {
		payloads := benchPayloads
		if persisted == persistOn {
			payloads = persistedPayloads
		}

		for _, payload := range payloads {
			for _, workers := range benchConcurrency {
				if tooSlowToGate(payload, workers, budget) {
					continue
				}

				name := persistName(persisted, fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers))

				b.Run(name, func(b *testing.B) {
					run(b, serverFor(b, persisted), payload, workers)
				})
			}
		}
	}
}

// putSpillPayload and putSpillWorkers are the one persisted PutObject cell whose
// body spills to a file (over 512 KiB) and whose iteration has more than one
// worker, which neither the 1KiB cell at 16 workers nor the 1MiB cell at one
// is. An iteration moves 1.25 MiB, over writeBudget, so the cell stands outside
// the matrix with a budget of its own: it took 3.3 ms, the slowest of ten runs
// on a runner.
var putSpillPayload = benchPayload{"640KiB", 640 << 10}

const putSpillWorkers = 2

func BenchmarkPutObject(b *testing.B) {
	benchMatrix(b, writeBudget, benchPutObject)

	name := persistName(persistOn, fmt.Sprintf("payload=%s/concurrency=%d", putSpillPayload.name, putSpillWorkers))

	b.Run(name, func(b *testing.B) {
		benchPutObject(b, serverFor(b, persistOn), putSpillPayload, putSpillWorkers)
	})
}

func benchPutObject(b *testing.B, s *objectBenchServer, p benchPayload, workers int) {
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
}

func BenchmarkGetObject(b *testing.B) {
	benchMatrix(b, readBudget, func(b *testing.B, s *objectBenchServer, p benchPayload, workers int) {
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
