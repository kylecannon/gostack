package inproc_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
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
)

func sqsCall(body string) inproc.Call {
	return inproc.Call{Account: account, Region: region, Service: "sqs", Target: target, Body: []byte(body)}
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
