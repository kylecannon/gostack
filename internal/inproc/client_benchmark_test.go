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
