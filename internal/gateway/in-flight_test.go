package gateway_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	concurrentBodies = 64
	throttled        = "ThrottlingException"

	// containerHeap is the most heap 64 bodies at the cap may take at once:
	// three quarters of the 512 MiB container DefaultInFlight is sized for.
	containerHeap = 384 << 20
)

// withInFlight sets the in-flight bound and the wait for the test, and puts
// both back after it.
func withInFlight(t *testing.T, bound int64, wait time.Duration) {
	t.Helper()

	protocol.SetInFlight(bound)
	protocol.SetBodyWait(wait)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})
}

// peakHeap samples HeapInuse until stop is closed and returns the most it saw
// above the value when it started.
func peakHeap(stop <-chan struct{}) <-chan uint64 {
	out := make(chan uint64, 1)

	var stats runtime.MemStats

	runtime.GC() //nolint:revive // the baseline is the heap that is live, not garbage
	runtime.ReadMemStats(&stats)

	base := stats.HeapInuse

	go func() {
		var peak uint64

		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		for {
			runtime.ReadMemStats(&stats)
			peak = max(peak, stats.HeapInuse-min(stats.HeapInuse, base))

			select {
			case <-stop:
				out <- peak

				return
			case <-ticker.C:
			}
		}
	}()

	return out
}

// Test_Gateway_InFlightBound: 64 clients each send a body at the cap at once,
// json bodies of empty batch entries that the decode budget refuses. The
// in-flight bound admits as many as fit and holds the rest in order; each is
// answered either with the budget's refusal or, when no room came within the
// wait, with ThrottlingException. Some are served, the heap stays under what
// the bound admits, and nothing is held once they are answered.
func Test_Gateway_InFlightBound(t *testing.T) {
	withInFlight(t, protocol.DefaultInFlight, 2*time.Second)

	server := httptest.NewServer(wideGateway(t))
	defer server.Close()

	body := repeatTo(`{"QueueUrl":"q","Entries":[{}`, `,{}`, `]}`)
	headers := jsonCall(nameSQS, "AmazonSQS.SendMessageBatch", "1.0")

	stop := make(chan struct{})
	peak := peakHeap(stop)

	var (
		wg              sync.WaitGroup
		served, refused atomic.Int64
	)

	for range concurrentBodies {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/", strings.NewReader(body))
			if err != nil {
				t.Error(err)

				return
			}

			for name, value := range headers {
				req.Header.Set(name, value)
			}

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Error(err)

				return
			}

			answer, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

			switch {
			case strings.Contains(string(answer), malformedQuery):
				served.Add(1)
			case strings.Contains(string(answer), throttled):
				refused.Add(1)
			default:
				t.Errorf("unexpected answer %s", answer)
			}
		})
	}

	wg.Wait()
	close(stop)

	most := <-peak

	t.Logf("served %d, throttled %d, peak heap above the start %d MiB", served.Load(), refused.Load(), most>>20)
	assert.Positive(t, served.Load(), "the bound serves bodies in order, it does not refuse them all")

	// The default bound admits three bodies at the cap at once. With the
	// garbage their buffers leave until the collector runs, the heap stays
	// within three quarters of a 512 MiB container; without the bound the 64
	// bodies held over 1 GiB.
	assert.Less(t, most, uint64(containerHeap))

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every body gives back what it held")
}

// Test_Gateway_LyingSender: 64 clients each state a body at the cap and send
// one byte, then hold the connection open. A body is buffered as it arrives,
// so each holds what it sent rather than what it stated.
func Test_Gateway_LyingSender(t *testing.T) {
	withInFlight(t, protocol.DefaultInFlight, 2*time.Second)

	server := httptest.NewServer(wideGateway(t))
	defer server.Close()

	stop := make(chan struct{})
	peak := peakHeap(stop)

	conns := make([]net.Conn, 0, concurrentBodies)

	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()

	for range concurrentBodies {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", server.Listener.Addr().String())
		require.NoError(t, err)

		conns = append(conns, conn)

		head := "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-amz-json-1.0\r\n" +
			"X-Amz-Target: AmazonSQS.SendMessageBatch\r\nAuthorization: " + signed("test", usEast1, nameSQS) + "\r\n" +
			"Content-Length: " + strconv.Itoa(int(protocol.DefaultMaxBody)) + "\r\n\r\n{"
		_, err = conn.Write([]byte(head))
		require.NoError(t, err)
	}

	// Let every request reach its body read.
	time.Sleep(500 * time.Millisecond)
	close(stop)

	most := <-peak

	t.Logf("peak heap above the start with %d lying senders: %d KiB", concurrentBodies, most>>10)
	assert.Less(t, most, uint64(concurrentBodies*4*protocol.FreeBody+8<<20),
		"a stated length is not allocated before its bytes arrive")
}
