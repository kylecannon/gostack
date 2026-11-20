package inproc_test

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const localstackHost = "localhost.localstack.cloud:4566"

var update = flag.Bool("update", false, "rewrite the golden requests under testdata")

// stack is a real gateway serving S3 and SQS, with a client bound to it. Every
// request the gateway sees is also handed to seen, when it is set.
type stack struct {
	client *inproc.Client

	mu   sync.Mutex
	seen func(*http.Request)
}

func newStack(t testing.TB) *stack {
	t.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(t.TempDir()))
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{Host: localstackHost} }, func(string) {})

	t.Cleanup(func() { provider.Close(context.WithoutCancel(t.Context())) })
	t.Cleanup(queues.Close)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration(), queues.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	provider.Bind(gw.Handler())

	s := &stack{client: inproc.New()}
	t.Cleanup(s.client.Close)

	inner := gw.Handler()
	s.client.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		seen := s.seen
		s.mu.Unlock()

		if seen != nil {
			seen(r)
		}

		inner.ServeHTTP(w, r)
	}))

	return s
}

// watch sets the function each request is handed to.
func (s *stack) watch(f func(*http.Request)) {
	s.mu.Lock()
	s.seen = f
	s.mu.Unlock()
}

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)

	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(path, got, 0o600))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

// Test_Do_JSONCallIsUnchangedOnTheWire pins the request a json 1.0 call makes,
// byte for byte, to the one captured before Call had a method, path, query or
// content type. synctest fixes the clock, so X-Amz-Date is stable.
func Test_Do_JSONCallIsUnchangedOnTheWire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStack(t)

		var dump []byte

		s.watch(func(r *http.Request) {
			var err error

			dump, err = httputil.DumpRequest(r, true)
			assert.NoError(t, err)
		})

		call := sqsCall(`{"QueueName":"golden"}`)
		call.Target = createQueue
		call.Header = http.Header{"X-Amzn-Trace-Id": {"Root=1-abc"}}

		resp, err := s.client.Do(t.Context(), call)
		require.NoError(t, err)
		require.True(t, resp.OK(), string(resp.Body))

		golden(t, "json-1.0.request", dump)
	})
}

// s3Call is a rest call to S3 as account in us-east-1.
func s3Call(method, path string, query url.Values) inproc.Call {
	return inproc.Call{Account: account, Region: "us-east-1", Service: "s3", Method: method, Path: path, Query: query}
}

// do makes call and fails the test unless the answer is a 2xx.
func (s *stack) do(t testing.TB, call inproc.Call) inproc.Response {
	t.Helper()

	resp, err := s.client.Do(t.Context(), call)
	require.NoError(t, err)
	require.True(t, resp.OK(), "%d %s", resp.Status, resp.Body)

	return resp
}

func Test_Do_ServesEveryProtocolThroughTheGateway(t *testing.T) {
	s := newStack(t)

	s.do(t, inproc.Call{
		Account: account, Region: region, Service: sqsService, Target: createQueue,
		Body: []byte(`{"QueueName":"proto"}`),
	})
	s.do(t, s3Call(http.MethodPut, "/proto-bucket", nil))

	for _, tc := range []struct {
		name string
		call inproc.Call
		// method, uri, contentType and target are the request the gateway sees.
		method, uri, contentType, target string
		// want is in the answer's body, and header names a header of the answer.
		want, header string
	}{
		{
			name: "json",
			call: inproc.Call{
				Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.GetQueueUrl",
				Body: []byte(`{"QueueName":"proto"}`),
			},
			method: http.MethodPost, uri: "/", contentType: "application/x-amz-json-1.0",
			target: "AmazonSQS.GetQueueUrl", want: `"QueueUrl":`, header: "X-Amzn-Requestid",
		},
		{
			name: "query",
			call: inproc.Call{
				Account: account, Region: region, Service: sqsService,
				ContentType: "application/x-www-form-urlencoded; charset=utf-8",
				Body:        []byte("Action=GetQueueUrl&QueueName=proto&Version=2012-11-05"),
			},
			method: http.MethodPost, uri: "/", contentType: "application/x-www-form-urlencoded; charset=utf-8",
			want: "<QueueUrl>", header: "X-Amzn-Requestid",
		},
		{
			name:   "rest with a path and a subresource",
			call:   s3Call(http.MethodGet, "/proto-bucket", url.Values{"location": {""}}),
			method: http.MethodGet, uri: "/proto-bucket?location", want: "<LocationConstraint", header: "X-Amz-Request-Id",
		},
		{
			name:   "rest with an escaped key and a query",
			call:   s3Call(http.MethodGet, "/proto-bucket", url.Values{"list-type": {"2"}, "prefix": {"a b/"}}),
			method: http.MethodGet, uri: "/proto-bucket?list-type=2&prefix=a+b%2F", want: "<ListBucketResult",
			header: "X-Amz-Request-Id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen *http.Request

			s.watch(func(r *http.Request) { seen = r.Clone(r.Context()) })
			t.Cleanup(func() { s.watch(nil) })

			resp := s.do(t, tc.call)

			assert.Contains(t, string(resp.Body), tc.want)
			assert.NotEmpty(t, resp.Header.Get(tc.header), "the answer's head is read back: %v", resp.Header)

			require.NotNil(t, seen)
			assert.Equal(t, tc.method, seen.Method)
			assert.Equal(t, tc.uri, seen.URL.RequestURI())
			assert.Equal(t, tc.contentType, seen.Header.Get("Content-Type"))

			_, hasTarget := seen.Header["X-Amz-Target"]
			assert.Equal(t, tc.target != "", hasTarget, "an X-Amz-Target only for a call with a Target")
			assert.Equal(t, tc.target, seen.Header.Get("X-Amz-Target"))
			assert.Contains(t, seen.Header.Get("Authorization"),
				"/"+tc.call.Region+"/"+tc.call.Service+"/aws4_request", "the credential names the signing name")
		})
	}
}

func Test_Do_ACallerContentTypeHeaderIsKept(t *testing.T) {
	var got string

	client := inproc.New()
	t.Cleanup(client.Close)
	client.Bind(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Get("Content-Type") }))

	call := sqsCall("{}")
	call.Header = http.Header{"Content-Type": {"application/x-amz-json-1.1"}}

	_, err := client.Do(t.Context(), call)
	require.NoError(t, err)
	assert.Equal(t, "application/x-amz-json-1.1", got)

	call.ContentType = "application/json"

	_, err = client.Do(t.Context(), call)
	require.NoError(t, err)
	assert.Equal(t, "application/json", got, "ContentType wins over the header")
}

// eightMiB is the size of the streamed object.
const eightMiB = 8 << 20

// pattern is an endless reader of a repeating byte pattern, which needs no
// buffer the size of what it yields.
type pattern struct{ n int64 }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(p.n % 251)
		p.n++
	}

	return len(b), nil
}

// allocated is how many bytes f allocates on the heap.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.GC() //nolint:revive // settles the heap so TotalAlloc counts only f.
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// Test_Stream_MovesAnEightMiBObjectWithoutHoldingItWhole puts and gets an 8
// MiB object through the gateway. Neither direction allocates anything near
// the object's size, while Do, which reads the answer whole, does.
func Test_Stream_MovesAnEightMiBObjectWithoutHoldingItWhole(t *testing.T) {
	s := newStack(t)
	s.do(t, s3Call(http.MethodPut, "/stream-bucket", nil))

	// limit is half the object: under it, no copy of the whole was made. S3
	// itself keeps up to 512 KiB of a new body in memory before it spills.
	const limit = eightMiB / 2

	put := allocated(func() {
		resp, err := s.client.Stream(t.Context(), s3Call(http.MethodPut, "/stream-bucket/big", nil),
			io.LimitReader(&pattern{}, eightMiB), eightMiB)
		require.NoError(t, err)
		require.True(t, resp.OK(), resp.Status)
		assert.NotEmpty(t, resp.Header.Get("ETag"))
		require.NoError(t, resp.Body.Close())
	})
	assert.Less(t, put, uint64(limit), "a streamed PUT allocated %d bytes", put)

	var read int64

	get := allocated(func() {
		resp, err := s.client.Stream(t.Context(), s3Call(http.MethodGet, "/stream-bucket/big", nil), nil, 0)
		require.NoError(t, err)
		require.True(t, resp.OK(), resp.Status)
		assert.Equal(t, strconv.Itoa(eightMiB), resp.Header.Get("Content-Length"))

		want := io.LimitReader(&pattern{}, eightMiB)
		buf, expect := make([]byte, 32<<10), make([]byte, 32<<10)

		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = io.ReadFull(want, expect[:n])
				require.Equal(t, expect[:n], buf[:n], "byte %d", read)
				read += int64(n)
			}

			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)
		}

		require.NoError(t, resp.Body.Close())
	})
	assert.Equal(t, int64(eightMiB), read)
	assert.Less(t, get, uint64(limit), "a streamed GET allocated %d bytes", get)

	buffered := allocated(func() {
		resp := s.do(t, s3Call(http.MethodGet, "/stream-bucket/big", nil))
		assert.Len(t, resp.Body, eightMiB)
	})
	assert.Greater(t, buffered, uint64(eightMiB), "the same GET through Do holds the body whole")
	t.Logf("allocated: streamed PUT %d, streamed GET %d, buffered GET %d", put, get, buffered)
}

// Test_Do_ServesCallsAtOnceThroughTheGateway runs many calls from one client at
// the same time, each to a queue of its own, and reads each queue back.
func Test_Do_ServesCallsAtOnceThroughTheGateway(t *testing.T) {
	s := newStack(t)

	const calls = 32

	var wg sync.WaitGroup

	for i := range calls {
		wg.Go(func() {
			name := "at-once-" + strconv.Itoa(i)

			created, err := s.client.Do(t.Context(), inproc.Call{
				Account: account, Region: region, Service: sqsService, Target: createQueue,
				Body: []byte(`{"QueueName":"` + name + `"}`),
			})
			if err != nil || !created.OK() {
				t.Errorf("CreateQueue %s: %v %s", name, err, created.Body)

				return
			}

			sent, err := s.client.Do(t.Context(), inproc.Call{
				Account: account, Region: region, Service: sqsService,
				ContentType: "application/x-www-form-urlencoded",
				Body: []byte("Action=SendMessage&Version=2012-11-05&MessageBody=" + name +
					"&QueueUrl=" + url.QueryEscape("http://sqs."+region+"."+localstackHost+"/"+account+"/"+name)),
			})
			if err != nil || !sent.OK() {
				t.Errorf("SendMessage %s: %v %s", name, err, sent.Body)
			}
		})
	}

	wg.Wait()

	for i := range calls {
		name := "at-once-" + strconv.Itoa(i)
		got := s.do(t, inproc.Call{
			Account: account, Region: region, Service: sqsService, Target: "AmazonSQS.ReceiveMessage",
			Body: []byte(`{"QueueUrl":"http://sqs.` + region + `.` + localstackHost + `/` + account + `/` + name + `"}`),
		})
		assert.Contains(t, string(got.Body), `"Body":"`+name+`"`)
	}
}

// dirtyLog is a gateway.Dirtier that keeps the names it is given.
type dirtyLog struct {
	mu     sync.Mutex
	marked []string
}

func (d *dirtyLog) MarkDirty(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.marked = append(d.marked, name)
}

func (d *dirtyLog) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.marked...)
}

// Test_Enqueue_AWriteWhoseHandlerPanicsIsMarkedDirty queues an SQS
// SendMessage through the gateway's internal handler, to a service that makes
// its change and then panics. The gateway answers it, and marks the service
// dirty, since the change it made would otherwise stay unsaved until the next
// write. A handler that calls runtime.Goexit instead is marked too, and its
// call fails with ErrAborted.
func Test_Enqueue_AWriteWhoseHandlerPanicsIsMarkedDirty(t *testing.T) {
	const panicLog = "ERROR panic while serving request"

	tests := []struct {
		name string
		// giveStatus is the status the handler writes before it panics, or 0.
		giveStatus int
		// giveGoexit has the handler call runtime.Goexit instead of panicking.
		giveGoexit bool
		wantStatus int
		wantErr    error
		wantLog    []string
	}{
		{name: "before the status", wantStatus: http.StatusInternalServerError, wantLog: []string{panicLog}},
		{name: "after the status", giveStatus: http.StatusOK, wantErr: inproc.ErrAborted, wantLog: []string{panicLog}},
		{name: "a Goexit", giveGoexit: true, wantErr: inproc.ErrAborted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &records{}
			previous := slog.Default()

			slog.SetDefault(slog.New(log))
			t.Cleanup(func() { slog.SetDefault(previous) })

			dirty := &dirtyLog{}
			reg := service.Registration{
				Metadata: service.Metadata{
					Name: sqsService, SigningName: sqsService, TargetPrefix: "AmazonSQS", EndpointPrefix: sqsService,
					APIVersion: "2012-11-05", Protocols: []protocol.Kind{protocol.KindJSON, protocol.KindQuery},
				},
				Serve: func(w http.ResponseWriter, _ *request.Context) {
					if tt.giveStatus != 0 {
						w.WriteHeader(tt.giveStatus)
					}

					if tt.giveGoexit {
						runtime.Goexit()
					}

					panic("the change was made")
				},
			}

			gw, err := gateway.New(
				func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
				[]service.Registration{reg}, nil, dirty)
			require.NoError(t, err)

			client := inproc.New()
			t.Cleanup(client.Close)
			client.Bind(gw.InternalHandler())

			answers := make(chan answer, 1)

			require.True(t, client.Enqueue(sqsCall("{}"), func(resp inproc.Response, err error) {
				answers <- answer{status: resp.Status, err: err}
			}))

			select {
			case got := <-answers:
				assert.Equal(t, tt.wantStatus, got.status)
				require.ErrorIs(t, got.err, tt.wantErr)
			case <-time.After(bound):
				require.FailNow(t, "the call was never answered")
			}

			assert.Equal(t, []string{sqsService}, dirty.list(), "the write that panicked is marked")
			assert.Equal(t, tt.wantLog, log.list())
		})
	}
}
