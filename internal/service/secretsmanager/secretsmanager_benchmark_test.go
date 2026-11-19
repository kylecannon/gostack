package secretsmanager_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/require"
)

// The Secrets Manager benchmark matrix (docs/standards.md, "Benchmarks"): each
// operation through the real gateway, in process. Sub-benchmark names carry
// the axes, for example
//
//	BenchmarkGetSecretValue/payload=64KiB/concurrency=16
//	BenchmarkListSecrets/secrets=10000
//
// Every worker reads and writes a secret of its own, so the cells measure the
// handler path and not one secret's lock. PutSecretValue sends a new token on
// every call: a repeated one would measure the idempotent path instead. The
// service speaks one protocol, json 1.1, so there is no protocol axis.

var (
	benchPayloads = []struct {
		name string
		size int
	}{{"64B", 64}, {"64KiB", 64 << 10}}
	benchConcurrency = []int{1, 16, 64}
)

// send makes one call to h in us-east-1 and returns the status.
func send(ctx context.Context, h http.Handler, operation, body string) int {
	return sendIn(ctx, h, "us-east-1", operation, body)
}

// sendIn is send signed for region.
func sendIn(ctx context.Context, h http.Handler, region, operation, body string) int {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/"+region+
		"/secretsmanager/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec.Code
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

// tokenOf is a 36-character ClientRequestToken unique to worker and n.
func tokenOf(worker int, n int64) string {
	return fmt.Sprintf("%08d-0000-0000-0000-%012d", worker, n)
}

// secretsFor creates one secret per worker, each holding value, and returns
// the handler that serves them.
func secretsFor(b *testing.B, workers int, value string, server func(*testing.B) http.Handler) http.Handler {
	b.Helper()

	h := server(b)

	for worker := range workers {
		status := send(b.Context(), h, "CreateSecret", `{"Name":"bench-`+strconv.Itoa(worker)+`","SecretString":"`+
			value+`","ClientRequestToken":"`+tokenOf(worker, 0)+`"}`)
		require.Equal(b, http.StatusOK, status)
	}

	return h
}

// plainHandler is a gateway in front of a Secrets Manager with no persistence.
func plainHandler(b *testing.B) http.Handler {
	b.Helper()

	return handlerFor(b, secretsmanager.New(nil))
}

func benchMatrix(b *testing.B, run func(b *testing.B, value string, workers int)) {
	b.Helper()

	for _, payload := range benchPayloads {
		for _, workers := range benchConcurrency {
			b.Run(fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers), func(b *testing.B) {
				value := strings.Repeat("x", payload.size)

				b.SetBytes(int64(payload.size * workers))
				b.ReportAllocs()
				run(b, value, workers)
			})
		}
	}
}

func BenchmarkGetSecretValue(b *testing.B) {
	benchMatrix(b, func(b *testing.B, value string, workers int) {
		h := secretsFor(b, workers, value, plainHandler)

		for b.Loop() {
			err := fanOut(workers, func(worker int) error {
				status := send(b.Context(), h, "GetSecretValue", `{"SecretId":"bench-`+strconv.Itoa(worker)+`"}`)
				if status != http.StatusOK {
					return fmt.Errorf("GetSecretValue answered %d", status)
				}

				return nil
			})
			require.NoError(b, err)
		}
	})
}

// BenchmarkGetSecretValueReplica is the GetSecretValue matrix read through a
// replica in eu-west-1 of each worker's secret, the replication option.
func BenchmarkGetSecretValueReplica(b *testing.B) {
	benchMatrix(b, func(b *testing.B, value string, workers int) {
		h := secretsFor(b, workers, value, plainHandler)

		for worker := range workers {
			status := send(b.Context(), h, "ReplicateSecretToRegions", `{"SecretId":"bench-`+strconv.Itoa(worker)+
				`","AddReplicaRegions":[{"Region":"eu-west-1"}]}`)
			require.Equal(b, http.StatusOK, status)
		}

		for b.Loop() {
			err := fanOut(workers, func(worker int) error {
				status := sendIn(b.Context(), h, "eu-west-1", "GetSecretValue",
					`{"SecretId":"bench-`+strconv.Itoa(worker)+`"}`)
				if status != http.StatusOK {
					return fmt.Errorf("GetSecretValue answered %d", status)
				}

				return nil
			})
			require.NoError(b, err)
		}
	})
}

// BenchmarkPutSecretValue runs the matrix with persistence off, and again under
// a persist=on key with the engine wired as PERSIST_SECRETSMANAGER=1 wires it:
//
//	BenchmarkPutSecretValue/persist=on/payload=64KiB/concurrency=16
func BenchmarkPutSecretValue(b *testing.B) {
	benchPutSecretValue(b, plainHandler)

	b.Run(persistOn, func(b *testing.B) { benchPutSecretValue(b, persistedHandler) })
}

func benchPutSecretValue(b *testing.B, server func(*testing.B) http.Handler) {
	b.Helper()

	benchMatrix(b, func(b *testing.B, value string, workers int) {
		h := secretsFor(b, workers, value, server)

		var calls atomic.Int64

		for b.Loop() {
			err := fanOut(workers, func(worker int) error {
				status := send(b.Context(), h, "PutSecretValue", `{"SecretId":"bench-`+strconv.Itoa(worker)+
					`","SecretString":"`+value+`","ClientRequestToken":"`+tokenOf(worker, calls.Add(1))+`"}`)
				if status != http.StatusOK {
					return fmt.Errorf("PutSecretValue answered %d", status)
				}

				return nil
			})
			require.NoError(b, err)
		}
	})
}

// BenchmarkListSecrets lists the first page, the default 100, of a region
// holding 10 or 10,000 secrets.
func BenchmarkListSecrets(b *testing.B) {
	for _, count := range []int{10, 10_000} {
		b.Run("secrets="+strconv.Itoa(count), func(b *testing.B) {
			h := secretsFor(b, count, "v", plainHandler)

			b.ReportAllocs()

			for b.Loop() {
				require.Equal(b, http.StatusOK, send(b.Context(), h, "ListSecrets", `{}`))
			}
		})
	}
}
