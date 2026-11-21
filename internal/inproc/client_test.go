package inproc_test

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	account = "123456789012"
	region  = "eu-west-1"
	target  = "AmazonSQS.SendMessage"

	sqsService  = "sqs"
	createQueue = "AmazonSQS.CreateQueue"
)

func sqsCall(body string) inproc.Call {
	return inproc.Call{Account: account, Region: region, Service: sqsService, Target: target, Body: []byte(body)}
}

func Test_Do_BuildsASignedRequestForTheCallersAccountAndRegion(t *testing.T) {
	var got *http.Request

	var gotBody []byte

	client := inproc.New()
	t.Cleanup(client.Close)
	client.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"MessageId":"m"}`))
	}))

	call := sqsCall(`{"QueueUrl":"q"}`)
	call.Header = http.Header{"X-Amzn-Trace-Id": {"Root=1-abc"}}

	resp, err := client.Do(t.Context(), call)
	require.NoError(t, err)

	assert.Equal(t, http.StatusAccepted, resp.Status)
	assert.True(t, resp.OK())
	assert.JSONEq(t, `{"MessageId":"m"}`, string(resp.Body))

	require.NotNil(t, got)
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, target, got.Header.Get("X-Amz-Target"))
	assert.Equal(t, "application/x-amz-json-1.0", got.Header.Get("Content-Type"))
	assert.Equal(t, "Root=1-abc", got.Header.Get("X-Amzn-Trace-Id"))
	assert.Equal(t, strconv.Itoa(len(gotBody)), got.Header.Get("Content-Length"))
	assert.JSONEq(t, `{"QueueUrl":"q"}`, string(gotBody))

	auth := got.Header.Get("Authorization")
	assert.Contains(t, auth, "AWS4-HMAC-SHA256 Credential="+account+"/")
	assert.Contains(t, auth, "/"+region+"/sqs/aws4_request")
	assert.Contains(t, auth, "SignedHeaders=host;x-amz-date")
}

type ctxKey struct{}

func Test_Do_GivesTheRequestNoValuesOfTheCallersContextButItsCancellation(t *testing.T) {
	var (
		sawValue bool
		sawDone  bool
	)

	client := inproc.New()
	t.Cleanup(client.Close)

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "owner"))
	client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawValue = r.Context().Value(ctxKey{}) != nil

		cancel()
		<-r.Context().Done()

		sawDone = true
	}))

	_, err := client.Do(ctx, sqsCall("{}"))
	require.NoError(t, err)

	assert.False(t, sawValue, "a value of the caller's context would route the call to the caller's service")
	assert.True(t, sawDone, "cancelling the caller's context cancels the request")
}

func Test_Do_ReturnsANon2xxAnswerAsAResponse(t *testing.T) {
	client := inproc.New()
	t.Cleanup(client.Close)
	client.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))

	resp, err := client.Do(t.Context(), sqsCall("{}"))
	require.NoError(t, err)

	assert.Equal(t, http.StatusNotImplemented, resp.Status)
	assert.False(t, resp.OK())
}

func Test_Do_WithoutAHandlerFails(t *testing.T) {
	client := inproc.New()
	t.Cleanup(client.Close)

	assert.False(t, client.Bound())

	_, err := client.Do(t.Context(), sqsCall("{}"))
	require.ErrorIs(t, err, inproc.ErrNotBound)

	var none *inproc.Client

	assert.False(t, none.Bound())
	assert.False(t, none.Enqueue(sqsCall("{}"), nil))
}

func Test_Enqueue_ServesCallsInOrderAndTheWorkerExitsWhenTheQueueIsEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu     sync.Mutex
			bodies []string
		)

		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)

			mu.Lock()
			bodies = append(bodies, string(body))
			mu.Unlock()
		}))

		assert.True(t, client.Idle(), "nothing runs before a call is queued")

		var statuses []int

		for _, body := range []string{"1", "2", "3"} {
			require.True(t, client.Enqueue(sqsCall(body), func(resp inproc.Response, err error) {
				assert.NoError(t, err)

				mu.Lock()
				statuses = append(statuses, resp.Status)
				mu.Unlock()
			}))
		}

		synctest.Wait()

		mu.Lock()
		assert.Equal(t, []string{"1", "2", "3"}, bodies)
		assert.Equal(t, []int{200, 200, 200}, statuses)
		mu.Unlock()

		assert.True(t, client.Idle(), "the worker exits once the queue is empty")

		// A later call starts a new worker.
		require.True(t, client.Enqueue(sqsCall("4"), nil))
		synctest.Wait()

		mu.Lock()
		assert.Equal(t, []string{"1", "2", "3", "4"}, bodies)
		mu.Unlock()

		assert.True(t, client.Idle())
	})
}

func Test_Enqueue_RefusesACallPastTheBound(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)

	client := inproc.New()
	client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}

		<-release
	}))

	// The worker takes the first call and blocks in it, so MaxQueued more fit.
	require.True(t, client.Enqueue(sqsCall("first"), nil))
	<-started

	for range inproc.MaxQueued {
		require.True(t, client.Enqueue(sqsCall("queued"), nil))
	}

	assert.False(t, client.Enqueue(sqsCall("one too many"), nil))

	close(release)
	client.Close()

	assert.True(t, client.Idle())
}

// Test_Unbounded_HoldsEveryWaitingCallAndKeepsDestinationsApart fills one slow
// destination's lane with more waiting calls than MaxQueued: a client made by
// NewUnbounded takes every one, another destination is served meanwhile, and
// once the slow one is let go every call is answered, in the order it was
// queued.
func Test_Unbounded_HoldsEveryWaitingCallAndKeepsDestinationsApart(t *testing.T) {
	const backlog = inproc.MaxQueued + 500

	release := make(chan struct{})

	var (
		mu     sync.Mutex
		served []int
	)

	client := inproc.NewUnbounded()
	t.Cleanup(client.Close)
	client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			return
		}

		<-release

		body, _ := io.ReadAll(r.Body)
		n, err := strconv.Atoi(string(body))
		assert.NoError(t, err)

		mu.Lock()
		defer mu.Unlock()

		served = append(served, n)
	}))

	var slowAnswered, fastAnswered atomic.Int32

	for i := range backlog {
		call := sqsCall(strconv.Itoa(i))
		call.Path = "/slow"

		require.True(t, client.Enqueue(call, func(inproc.Response, error) { slowAnswered.Add(1) }),
			"call %d waits, as there is no bound", i)
	}

	const fast = 20

	for i := range fast {
		call := sqsCall(strconv.Itoa(i))
		call.Path = "/fast"

		require.True(t, client.Enqueue(call, func(inproc.Response, error) { fastAnswered.Add(1) }))
	}

	require.Eventually(t, func() bool { return fastAnswered.Load() == fast }, 10*time.Second, time.Millisecond,
		"the other destination is served while the slow one holds %d calls", backlog)
	assert.Zero(t, slowAnswered.Load())

	close(release)

	require.Eventually(t, func() bool { return slowAnswered.Load() == backlog }, 30*time.Second, time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	want := make([]int, backlog)
	for i := range want {
		want[i] = i
	}

	assert.Equal(t, want, served, "no call was dropped, and the lane kept the order")
}

// A backlog past MaxQueued in an unbounded client is cut at the drain's
// deadline and counted, whole: the hub's warning carries every call it did not
// serve, none lost between the bound and the shutdown.
func Test_Unbounded_DrainCountsTheWholeBacklogItCuts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const backlog = inproc.MaxQueued + 1000

		log := captureLog(t)
		stuck := make(chan struct{})

		hub := inproc.NewHub(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stuck }))

		client := inproc.NewUnbounded()
		client.Bind(hub)

		for i := range backlog {
			require.True(t, client.Enqueue(laneCall(0, i), nil))
		}

		synctest.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		cut := hub.Drain(ctx)

		assert.Equal(t, backlog, cut, "the call running and every call waiting were cut")
		assert.Contains(t, log.String(), "in-process calls cut at shutdown")
		assert.Contains(t, log.String(), "count="+strconv.Itoa(backlog))

		close(stuck)
		client.Close()
	})
}

func Test_Close_DropsQueuedCallsCancelsTheOneInFlightAndRefusesMore(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})

	var served sync.WaitGroup

	served.Add(1)

	client := inproc.New()
	client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer served.Done()

		close(started)

		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(time.Minute):
		}
	}))

	var done sync.WaitGroup

	done.Add(1)

	require.True(t, client.Enqueue(sqsCall("in flight"), func(inproc.Response, error) { done.Done() }))
	<-started

	dropped := false

	require.True(t, client.Enqueue(sqsCall("queued"), func(inproc.Response, error) { dropped = true }))

	client.Close()
	client.Close()

	<-cancelled
	served.Wait()
	done.Wait()

	assert.False(t, dropped, "a queued call is dropped, not served")
	assert.False(t, client.Enqueue(sqsCall("after close"), nil))
	assert.True(t, client.Idle())
}

// laneCall is a call to destination lane, numbered n in its body.
func laneCall(lane, n int) inproc.Call {
	call := sqsCall(strconv.Itoa(n))
	call.Path = "/" + strconv.Itoa(lane)

	return call
}

func Test_Enqueue_RunsDestinationsAtOnceUpToTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu       sync.Mutex
			inFlight int
			most     int
			served   int
		)

		release := make(chan struct{})

		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			served++
			mu.Unlock()
		}))

		for lane := range 2 * inproc.MaxInFlight {
			require.True(t, client.Enqueue(laneCall(lane, 0), nil))
		}

		synctest.Wait()

		mu.Lock()
		assert.Equal(t, inproc.MaxInFlight, inFlight, "every destination runs at once, up to the bound")
		mu.Unlock()

		close(release)
		synctest.Wait()

		mu.Lock()
		assert.Equal(t, inproc.MaxInFlight, most)
		assert.Equal(t, 2*inproc.MaxInFlight, served)
		mu.Unlock()

		assert.True(t, client.Idle(), "the workers exit once nothing waits")
	})
}

// Test_Enqueue_KeepsEachDestinationsOrder runs many destinations at once and
// checks each one's calls arrived one at a time, in the order they were queued.
// Run it with -race -count=20.
func Test_Enqueue_KeepsEachDestinationsOrder(t *testing.T) {
	const (
		lanes   = 3 * inproc.MaxInFlight
		perLane = 40
	)

	var (
		mu       sync.Mutex
		got      = make(map[string][]string)
		running  = make(map[string]bool)
		inFlight int
		most     int
		overlap  bool
		served   sync.WaitGroup
	)

	served.Add(lanes * perLane)

	client := inproc.New()
	t.Cleanup(client.Close)
	client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		overlap = overlap || running[r.URL.Path]
		running[r.URL.Path] = true
		inFlight++
		most = max(most, inFlight)
		got[r.URL.Path] = append(got[r.URL.Path], string(body))
		mu.Unlock()

		runtime.Gosched()

		mu.Lock()
		running[r.URL.Path] = false
		inFlight--
		mu.Unlock()
	}))

	for n := range perLane {
		for lane := range lanes {
			require.True(t, client.Enqueue(laneCall(lane, n), func(inproc.Response, error) { served.Done() }))
		}
	}

	served.Wait()

	mu.Lock()
	defer mu.Unlock()

	assert.False(t, overlap, "two calls to one destination never run at once")
	assert.LessOrEqual(t, most, inproc.MaxInFlight)

	want := make([]string, perLane)
	for n := range perLane {
		want[n] = strconv.Itoa(n)
	}

	for lane := range lanes {
		assert.Equal(t, want, got["/"+strconv.Itoa(lane)], "destination %d", lane)
	}
}

// Test_Enqueue_OrdersCallsByDestinationNotByAccount queues calls from two
// accounts alternately to one region, service, method, path and target: they
// are one destination, so they run in queue order and never overlap.
func Test_Enqueue_OrdersCallsByDestinationNotByAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu       sync.Mutex
			bodies   []string
			inFlight int
			most     int
		)

		release := make(chan struct{})

		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)

			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			bodies = append(bodies, string(body))
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			mu.Unlock()
		}))

		accounts := []string{account, "210987654321"}

		want := make([]string, 0, 6)

		for n := range 6 {
			call := sqsCall(strconv.Itoa(n))
			call.Account = accounts[n%2]
			want = append(want, strconv.Itoa(n))

			require.True(t, client.Enqueue(call, nil))
		}

		synctest.Wait()

		mu.Lock()
		assert.Equal(t, 1, inFlight, "two accounts calling one destination share a lane")
		mu.Unlock()

		close(release)
		synctest.Wait()

		mu.Lock()
		assert.Equal(t, want, bodies, "the calls run in the order they were queued")
		assert.Equal(t, 1, most, "the calls never overlap")
		mu.Unlock()
	})
}

// gauge is a handler that records the bodies of the calls it serves, in the
// order they arrive, and how many ran at once, holding each until release is
// closed.
type gauge struct {
	mu       sync.Mutex
	bodies   []string
	inFlight int
	most     int
	release  chan struct{}
}

func newGauge() *gauge {
	return &gauge{release: make(chan struct{})}
}

func (g *gauge) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	g.mu.Lock()
	g.inFlight++
	g.most = max(g.most, g.inFlight)
	g.bodies = append(g.bodies, string(body))
	g.mu.Unlock()

	<-g.release

	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
}

// running is how many calls the handler holds now.
func (g *gauge) running() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.inFlight
}

// Test_Enqueue_OneLaneOrdersCallsToDifferentDestinations queues calls with one
// Lane but different methods and paths, a PUT and then a DELETE of one S3 key
// among them: the Lane is the whole key, so they run one at a time in queue
// order.
func Test_Enqueue_OneLaneOrdersCallsToDifferentDestinations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGauge()

		client := inproc.New()
		defer client.Close()

		client.Bind(g)

		methods := []string{http.MethodPut, http.MethodDelete, http.MethodGet, http.MethodPost}
		want := make([]string, 0, len(methods))

		for n, method := range methods {
			call := s3Call(method, "/bucket/key-"+strconv.Itoa(n), nil)
			call.Body = []byte(strconv.Itoa(n))
			call.Lane = "arn:aws:s3:::bucket/key"
			want = append(want, strconv.Itoa(n))

			require.True(t, client.Enqueue(call, nil))
		}

		synctest.Wait()
		assert.Equal(t, 1, g.running(), "calls in one lane wait for each other, whatever their destination")

		close(g.release)
		synctest.Wait()

		g.mu.Lock()
		assert.Equal(t, want, g.bodies, "the calls run in the order they were queued")
		assert.Equal(t, 1, g.most, "the calls never overlap")
		g.mu.Unlock()
	})
}

// Test_Enqueue_DifferentLanesToOneDestinationRunAtOnce queues calls to one
// destination, an SQS SendMessage in one region, each in a Lane of its own, as
// S3 does for the queues it notifies: they no longer wait for each other.
func Test_Enqueue_DifferentLanesToOneDestinationRunAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const lanes = 4

		g := newGauge()

		client := inproc.New()
		defer client.Close()

		client.Bind(g)

		for n := range lanes {
			call := sqsCall(strconv.Itoa(n))
			call.Lane = "arn:aws:sqs:" + region + ":" + account + ":queue-" + strconv.Itoa(n)

			require.True(t, client.Enqueue(call, nil))
		}

		synctest.Wait()
		assert.Equal(t, lanes, g.running(), "each lane runs at once")

		close(g.release)
		synctest.Wait()
		assert.True(t, client.Idle())
	})
}

// abortAfterStatus answers a call whose body is "abort" the way the gateway
// aborts a response whose status it has already written: it panics with
// http.ErrAbortHandler. Any other call gets a 200.
func abortAfterStatus(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	w.WriteHeader(http.StatusInternalServerError)

	if string(body) == "abort" {
		panic(http.ErrAbortHandler)
	}
}

func Test_Do_AHandlerPanicAfterTheStatusIsAnError(t *testing.T) {
	client := inproc.New()
	t.Cleanup(client.Close)

	client.Bind(http.HandlerFunc(abortAfterStatus))

	resp, err := client.Do(t.Context(), sqsCall("abort"))
	require.ErrorIs(t, err, inproc.ErrAborted, "the call fails instead of its caller")
	assert.Equal(t, inproc.Response{}, resp)

	resp, err = client.Do(t.Context(), sqsCall("fine"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.Status)
}

func Test_Enqueue_AHandlerPanicAfterTheStatusIsTheCallsErrorAndTheWorkerGoesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(abortAfterStatus))

		var (
			aborted error
			next    inproc.Response
			nextErr error
		)

		require.True(t, client.Enqueue(sqsCall("abort"), func(_ inproc.Response, err error) { aborted = err }))
		require.True(t, client.Enqueue(sqsCall("fine"), func(resp inproc.Response, err error) {
			next, nextErr = resp, err
		}))

		synctest.Wait()

		require.ErrorIs(t, aborted, inproc.ErrAborted, "done gets the panic as the call's error")
		require.NoError(t, nextErr, "the next call in the lane is still served")
		assert.Equal(t, http.StatusInternalServerError, next.Status)
		assert.True(t, client.Idle())
	})
}

// Test_Stream_ReturnsAtTheHeadAndStreamsTheBody checks the head is the one the
// handler sent with its status, and the body arrives as it is written.
func Test_Stream_ReturnsAtTheHeadAndStreamsTheBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		next := make(chan struct{})

		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)

			w.Header()["x-amz-request-id"] = []string{"req"} // spelled as S3 spells it, to be canonicalized.
			w.WriteHeader(http.StatusPartialContent)
			w.Header().Set("X-Late", "never sent")

			_, _ = w.Write(body)
			<-next
			_, _ = w.Write([]byte(" second"))
		}))

		resp, err := client.Stream(t.Context(), sqsCall(""), strings.NewReader("first"), -1)
		require.NoError(t, err)

		assert.Equal(t, http.StatusPartialContent, resp.Status)
		assert.True(t, resp.OK())
		assert.Equal(t, "req", resp.Header.Get("X-Amz-Request-Id"))
		assert.Empty(t, resp.Header.Get("X-Late"), "a header set after the status is not part of the head")

		buf := make([]byte, 64)
		n, err := resp.Body.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, "first", string(buf[:n]), "the first write arrives before the handler writes again")

		close(next)

		rest, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, " second", string(rest))
		require.NoError(t, resp.Body.Close())
	})
}

// endless is a handler that writes until a write fails, and reports that it
// returned.
func endless(returned *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer returned.Store(true)

		chunk := make([]byte, 1024)

		for {
			_, err := w.Write(chunk)
			if err != nil {
				return
			}
		}
	})
}

func Test_Stream_ClosingTheBodyEarlyEndsTheHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var returned atomic.Bool

		client := inproc.New()
		defer client.Close()

		client.Bind(endless(&returned))

		resp, err := client.Stream(t.Context(), sqsCall(""), nil, 0)
		require.NoError(t, err)

		_, err = io.ReadFull(resp.Body, make([]byte, 4096))
		require.NoError(t, err)
		assert.False(t, returned.Load())

		require.NoError(t, resp.Body.Close())
		assert.True(t, returned.Load(), "Close returns once the handler has")
		require.NoError(t, resp.Body.Close(), "a second Close is harmless")
	})
}

func Test_Stream_TheEndOfTheContextEndsTheHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			returned atomic.Bool
			sawDone  atomic.Bool
		)

		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			endless(&returned).ServeHTTP(w, r)
			sawDone.Store(r.Context().Err() != nil)
		}))

		ctx, cancel := context.WithCancel(t.Context())

		resp, err := client.Stream(ctx, sqsCall(""), nil, 0)
		require.NoError(t, err)

		// The caller stops reading and never closes the body: the handler is
		// blocked in a write when the context ends.
		cancel()
		synctest.Wait()

		assert.True(t, returned.Load(), "the handler returns without Close")
		assert.True(t, sawDone.Load(), "the request's context ended with the caller's")

		_, err = resp.Body.Read(make([]byte, 1))
		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, resp.Body.Close())
	})
}

func Test_Stream_AHandlerPanicIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := inproc.New()
		defer client.Close()

		client.Bind(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}))

		_, err := client.Stream(t.Context(), sqsCall(""), nil, 0)
		require.ErrorIs(t, err, inproc.ErrAborted, "a panic before the head is the error of the call")

		client.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))

			panic(http.ErrAbortHandler)
		}))

		resp, err := client.Stream(t.Context(), sqsCall(""), nil, 0)
		require.NoError(t, err)

		got, err := io.ReadAll(resp.Body)
		require.ErrorIs(t, err, inproc.ErrAborted, "a panic after the head ends the body")
		assert.Equal(t, "partial", string(got))
		require.NoError(t, resp.Body.Close())
	})
}

func Test_Stream_WithoutAHandlerFails(t *testing.T) {
	client := inproc.New()
	t.Cleanup(client.Close)

	_, err := client.Stream(t.Context(), sqsCall(""), nil, 0)
	require.ErrorIs(t, err, inproc.ErrNotBound)
}
