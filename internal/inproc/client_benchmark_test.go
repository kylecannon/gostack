package inproc_test

import (
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/require"
)

// enqueueBatch is how many calls one iteration of BenchmarkEnqueue queues: a
// multiple of every lane and concurrency count, and far below MaxQueued.
const enqueueBatch = 64

// BenchmarkEnqueue queues enqueueBatch S3 GETs of a 512-byte object per
// iteration, spread evenly over 1, 16 or 64 lanes, from 1 or 16 goroutines at
// once, and waits for every answer. One lane runs its calls one at a time; 16
// lanes is MaxInFlight; 64 lanes is more than the workers can take at once.
func BenchmarkEnqueue(b *testing.B) {
	const size = 512

	s := newStack(b)
	s.do(b, s3Call(http.MethodPut, "/bench-bucket", nil))

	put := s3Call(http.MethodPut, "/bench-bucket/small", nil)
	put.Body = make([]byte, size)
	s.do(b, put)

	for _, lanes := range []int{1, 16, 64} {
		calls := make([]inproc.Call, enqueueBatch)
		for i := range calls {
			calls[i] = s3Call(http.MethodGet, "/bench-bucket/small", nil)
			calls[i].Lane = "lane-" + strconv.Itoa(i%lanes)
		}

		for _, concurrency := range []int{1, 16} {
			name := "lanes=" + strconv.Itoa(lanes) + "/payload=512B/concurrency=" + strconv.Itoa(concurrency)

			b.Run(name, func(b *testing.B) {
				b.SetBytes(size * enqueueBatch)
				b.ReportAllocs()

				for b.Loop() {
					enqueueAll(b, s.client, calls, concurrency)
				}
			})
		}
	}
}

// enqueueAll queues calls from concurrency goroutines, each taking every
// concurrency-th call, and waits until every call is answered.
func enqueueAll(b *testing.B, client *inproc.Client, calls []inproc.Call, concurrency int) {
	b.Helper()

	var answered, producers sync.WaitGroup

	answered.Add(len(calls))

	done := func(resp inproc.Response, err error) {
		defer answered.Done()

		if err != nil || !resp.OK() {
			b.Errorf("GET: %v %d", err, resp.Status)
		}
	}

	for p := range concurrency {
		producers.Go(func() {
			for i := p; i < len(calls); i += concurrency {
				if !client.Enqueue(calls[i], done) {
					b.Error("Enqueue refused the call")
					answered.Done()
				}
			}
		})
	}

	producers.Wait()
	answered.Wait()
}

// tooSlowToGate reports the cells BenchmarkGetObject leaves out under the 10 ms
// rule (docs/standards.md, "Benchmarks"): payload=8MiB at concurrency=16, 16
// bodies of 8 MiB an iteration, took NUMBERS-PENDING. The same body at one call
// stays in both modes, and so does 512B at 16.
func tooSlowToGate(size int64, concurrency int) bool {
	return size == eightMiB && concurrency == 16
}

// BenchmarkGetObject is an S3 GET through the gateway, its body read whole by
// Do or streamed by Stream, for a small and a large object, with one call or 16
// at once per iteration.
func BenchmarkGetObject(b *testing.B) {
	s := newStack(b)
	s.do(b, s3Call(http.MethodPut, "/bench-bucket", nil))

	payloads := []struct {
		name string
		size int64
	}{{"512B", 512}, {"8MiB", eightMiB}}

	for _, payload := range payloads {
		resp, err := s.client.Stream(b.Context(), s3Call(http.MethodPut, "/bench-bucket/"+payload.name, nil),
			io.LimitReader(&pattern{}, payload.size), payload.size)
		require.NoError(b, err)
		require.True(b, resp.OK(), resp.Status)
		require.NoError(b, resp.Body.Close())
	}

	modes := []struct {
		name string
		get  func(tb testing.TB, call inproc.Call)
	}{
		{"buffered", func(tb testing.TB, call inproc.Call) {
			resp, err := s.client.Do(tb.Context(), call)
			if err != nil || !resp.OK() {
				tb.Errorf("GET: %v %d", err, resp.Status)
			}
		}},
		{"streamed", func(tb testing.TB, call inproc.Call) {
			resp, err := s.client.Stream(tb.Context(), call, nil, 0)
			if err != nil || !resp.OK() {
				tb.Errorf("GET: %v", err)

				return
			}

			_, err = io.Copy(io.Discard, resp.Body)
			if err != nil {
				tb.Errorf("read: %v", err)
			}

			_ = resp.Body.Close()
		}},
	}

	for _, mode := range modes {
		for _, payload := range payloads {
			for _, concurrency := range []int{1, 16} {
				if tooSlowToGate(payload.size, concurrency) {
					continue
				}

				name := "mode=" + mode.name + "/payload=" + payload.name + "/concurrency=" + strconv.Itoa(concurrency)
				call := s3Call(http.MethodGet, "/bench-bucket/"+payload.name, nil)

				b.Run(name, func(b *testing.B) {
					b.SetBytes(payload.size * int64(concurrency))
					b.ReportAllocs()

					for b.Loop() {
						var wg sync.WaitGroup

						for range concurrency {
							wg.Go(func() { mode.get(b, call) })
						}

						wg.Wait()
					}
				})
			}
		}
	}
}
