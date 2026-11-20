package gateway_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryingClient is server's client, dialing again when the listen queue was
// full: many clients dialing at once overflow it on macOS, which resets the
// connection before the gateway sees it.
func retryingClient(server *httptest.Server) *http.Client {
	client := server.Client()
	transport, _ := client.Transport.(*http.Transport)
	transport = transport.Clone()

	var dialer net.Dialer

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		for {
			conn, err := dialer.DialContext(ctx, network, addr)
			if !errors.Is(err, syscall.ECONNRESET) || ctx.Err() != nil {
				return conn, err //nolint:wrapcheck // the dialer's own error
			}

			time.Sleep(time.Millisecond)
		}
	}

	return &http.Client{Transport: resetRetrier{next: transport}}
}

// resetRetrier sends a request again when its connection was reset or closed
// before an answer came. macOS takes a burst of connections past its listen
// backlog and resets some of them after the dial has succeeded, while the
// request is being written or its answer awaited; the server never saw those
// requests, so sending them again is what a client would do.
type resetRetrier struct {
	next http.RoundTripper
}

func (r resetRetrier) RoundTrip(req *http.Request) (*http.Response, error) {
	const attempts = 50

	for attempt := 1; ; attempt++ {
		resp, err := r.next.RoundTrip(req)

		reset := errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed)
		if !reset || attempt == attempts || req.GetBody == nil || req.Context().Err() != nil {
			return resp, err //nolint:wrapcheck // the transport's own error
		}

		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return resp, err //nolint:wrapcheck // the transport's own error
		}

		req = req.Clone(req.Context())
		req.Body = body

		time.Sleep(time.Millisecond)
	}
}

// barrier holds every handler that reaches it until each of n requests has
// either reached it or been refused, or until its deadline.
type barrier struct {
	n, seen atomic.Int64
	all     chan struct{}
	once    sync.Once
}

func newBarrier(n int64) *barrier {
	b := &barrier{all: make(chan struct{})}
	b.n.Store(n)

	return b
}

func (b *barrier) mark() {
	if b.seen.Add(1) == b.n.Load() {
		b.once.Do(func() { close(b.all) })
	}
}

func (b *barrier) wait() {
	b.mark()

	select {
	case <-b.all:
	case <-time.After(20 * time.Second):
	}
}

// holdingLifecycle answers PutBucketLifecycleConfiguration once every request
// of the test has reached it or been refused, holding the decoded input until
// then.
type holdingLifecycle struct {
	s3api.Unimplemented

	barrier *barrier
	rules   atomic.Int64
}

func (h *holdingLifecycle) PutBucketLifecycleConfiguration(
	_ context.Context, in *s3api.PutBucketLifecycleConfigurationInput,
) (*s3api.PutBucketLifecycleConfigurationOutput, error) {
	h.barrier.wait()
	h.rules.Add(int64(len(in.LifecycleConfiguration.Rules)))

	return &s3api.PutBucketLifecycleConfigurationOutput{}, nil
}

// denseRules is the number of empty rules in each body of
// Test_Gateway_DecodeChargeBound: about as many as the decode budget admits,
// since each charges about 624 bytes (a 104-byte LifecycleRule at the append
// cost, and two tokens) against a budget of 16 MiB and twice the body.
const denseRules = 25_000

// Test_Gateway_DecodeChargeBound: 64 clients each send a lifecycle
// configuration of 25,000 empty rules, about 170 KiB that decode into about
// 3 MiB, and the handler holds every decoded input until all 64 have reached it
// or been refused. With the decode charge on, what decoding holds past
// protocol.FreeDecode is drawn from the in-flight pool, so the heap stays under
// the pool and what each request holds for free; some requests are served and
// the rest get SlowDown. With the charge off (each call marked internal, which
// exempts its decoding but not its body) every request decodes at once and the
// heap passes that bound.
func Test_Gateway_DecodeChargeBound(t *testing.T) {
	const (
		clients = concurrentBodies
		// what a request holds outside the pool: what it decodes for free, its
		// connection's buffers and the request's own structures.
		perRequest = protocol.FreeDecode + 256<<10
		bound      = uint64(protocol.DefaultInFlight + clients*perRequest)
	)

	head, tail := `<LifecycleConfiguration>`, `</LifecycleConfiguration>`
	body := head + strings.Repeat(`<Rule/>`, denseRules) + tail
	require.Greater(t, len(body), protocol.FreeBody, "the body is metered")

	peaks := map[bool]uint64{}
	prepares := map[bool]func(*request.Context){
		true: func(*request.Context) {},
		// Marking the call internal after the gateway metered its body exempts
		// its decoding only.
		false: func(rc *request.Context) {
			rc.Request = rc.Request.WithContext(request.MarkInternal(rc.Request.Context()))
		},
	}

	for _, charged := range []bool{false, true} {
		withInFlight(t, time.Second)

		run := runHeld(t, clients, body, prepares[charged])
		peaks[charged] = run.peak

		t.Logf("decode charged %v: served %d, throttled %d, peak heap above the start %d MiB (bound %d MiB)",
			charged, run.served, run.refused, run.peak>>20, bound>>20)
		assert.Equal(t, int64(clients), run.served+run.refused)
		assert.Equal(t, run.served*denseRules, run.rules, "every served request decoded every rule")

		_, held := protocol.InFlight()
		assert.Zero(t, held, "every request gives back what it held")
	}

	assert.Less(t, peaks[true], bound, "the decode charge bounds the decodings in flight")
	assert.Greater(t, peaks[false], bound, "without the charge, the same requests pass the bound")
}

// heldRun is what runHeld saw: how many requests were served and refused, the
// peak heap, and the rules the handler decoded.
type heldRun struct {
	served, refused int64
	peak            uint64
	rules           int64
}

// runHeld sends body from that many clients at once to a gateway whose
// lifecycle handler holds each decoded input (holdingLifecycle), each call
// made ready by prepare.
func runHeld(t *testing.T, clients int, body string, prepare func(*request.Context)) heldRun {
	t.Helper()

	handler := &holdingLifecycle{barrier: newBarrier(int64(clients))}
	reg := generatedRegistration(s3api.Metadata, func(w http.ResponseWriter, rc *request.Context) {
		prepare(rc)

		s3api.Serve(handler, w, rc)
	})

	server := httptest.NewServer(newGateway(t, newSettings(), []service.Registration{reg}).Handler())
	defer server.Close()

	client := retryingClient(server)
	auth := signed("test", usEast1, nameS3)

	stop := make(chan struct{})
	heap := peakHeap(stop)

	var (
		wg        sync.WaitGroup
		ok, busy  atomic.Int64
		unexpects atomic.Int64
	)

	for range clients {
		wg.Go(func() {
			status, answer := putLifecycle(t, client, server.URL, auth, body)

			switch {
			case status == http.StatusOK:
				ok.Add(1)
			case status == http.StatusServiceUnavailable && strings.Contains(answer, "SlowDown"):
				busy.Add(1)
				handler.barrier.mark()
			default:
				unexpects.Add(1)
				handler.barrier.mark()
				t.Errorf("unexpected answer %d %s", status, answer)
			}
		})
	}

	wg.Wait()
	close(stop)

	return heldRun{served: ok.Load(), refused: busy.Load(), peak: <-heap, rules: handler.rules.Load()}
}

func putLifecycle(t *testing.T, client *http.Client, url, auth, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url+"/b?lifecycle", strings.NewReader(body))
	if err != nil {
		t.Error(err)

		return 0, ""
	}

	req.Header.Set(headerAuth, auth)

	resp, err := client.Do(req)
	if err != nil {
		t.Error(err)

		return 0, ""
	}

	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode, string(answer)
}
