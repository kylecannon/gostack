package inproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// jsonContentType is the content type of the json 1.0 protocol, which every
	// service in-process callers use (SQS) accepts.
	jsonContentType = "application/x-amz-json-1.0"

	// host is the Host of an in-process request. The gateway routes a json call
	// by its X-Amz-Target and signing name, so the host only has to be a plain
	// one.
	host = "localhost.localstack.cloud:4566"

	// remoteAddr is the peer address of an in-process request.
	remoteAddr = "127.0.0.1:0"

	// signedHeaders and signature fill the parts of a SigV4 Authorization header
	// that nothing checks for an in-process call: the gateway reads the
	// credential scope and no service verifies a signature.
	signedHeaders = "host;x-amz-date"
	signature     = "0000000000000000000000000000000000000000000000000000000000000000"

	dateFormat      = "20060102"
	timestampFormat = "20060102T150405Z"

	// MaxQueued is how many calls Enqueue holds before it refuses the next.
	MaxQueued = 4096

	// callTimeout bounds one call the worker makes.
	callTimeout = 30 * time.Second
)

// ErrNotBound is the error of a call made before Bind gave the client a handler.
var ErrNotBound = errors.New("inproc: no handler bound")

// Call is one request to a service of the same process.
type Call struct {
	// Account and Region are the caller's: they go into the credential scope of
	// the request, which is how the gateway tells which account's state a call
	// acts on. A 12-digit account is its own access key.
	Account string
	Region  string
	// Service is the SigV4 signing name, for example "sqs".
	Service string
	// Target is the X-Amz-Target of the operation, for example
	// "AmazonSQS.SendMessage".
	Target string
	// Body is the json request body.
	Body []byte
	// Header holds extra request headers.
	Header http.Header
}

// Response is what the handler answered.
type Response struct {
	Status int
	Body   []byte
}

// OK reports whether the status is a 2xx.
func (r Response) OK() bool {
	return r.Status >= http.StatusOK && r.Status < http.StatusMultipleChoices
}

// NotEnabled reports whether the gateway answered that the service is not
// enabled: a service that is not registered is a 501 whose message says so.
func (r Response) NotEnabled() bool {
	return r.Status == http.StatusNotImplemented && bytes.Contains(r.Body, []byte("is not enabled"))
}

// queued is a call waiting for the worker, and what to do with its answer.
type queued struct {
	call Call
	done func(Response, error)
}

// Client makes calls to a service of the same process through the gateway's
// own handler chain, so that a call behaves as one from outside does and never
// reaches into a service's state. Do is synchronous. Enqueue hands a call to a
// worker that exists only while calls are waiting.
//
// The zero value is not ready: use New.
type Client struct {
	handler atomic.Pointer[http.Handler]

	// ctx ends when Close is called.
	ctx    context.Context //nolint:containedctx // the lifetime of the worker, not a request's.
	cancel context.CancelFunc

	mu     sync.Mutex
	queue  []queued
	active bool
	closed bool
	worker sync.WaitGroup
}

// New returns a Client with no handler. The gateway is built from the services
// the client is given to, so the handler arrives later, through Bind.
func New() *Client {
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{ctx: ctx, cancel: cancel}
}

// Bind sets the handler calls are served through, normally gateway.Handler().
func (c *Client) Bind(h http.Handler) {
	c.handler.Store(&h)
}

// Bound reports whether Bind has been called.
func (c *Client) Bound() bool {
	return c != nil && c.handler.Load() != nil
}

// Do serves call through the handler and returns the answer. A non-2xx answer
// is a Response, not an error; an error means the call could not be made.
func (c *Client) Do(ctx context.Context, call Call) (Response, error) {
	if !c.Bound() {
		return Response{}, ErrNotBound
	}

	// The request gets a context of its own that ends when ctx does and carries
	// none of ctx's values: a caller inside the gateway holds the values of its
	// own request, among them the mark that names its service as the one the
	// request is for, and a call that inherited them would be routed to the
	// caller instead of its target.
	own, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	req, err := newRequest(own, call) //nolint:contextcheck // own deliberately inherits none of ctx's values.
	if err != nil {
		return Response{}, err
	}

	rec := &recorder{header: make(http.Header), status: http.StatusOK}
	(*c.handler.Load()).ServeHTTP(rec, req)

	return Response{Status: rec.status, Body: rec.body.Bytes()}, nil
}

// newRequest builds the request the gateway sees for call.
func newRequest(ctx context.Context, call Call) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+"/", bytes.NewReader(call.Body))
	if err != nil {
		return nil, fmt.Errorf("inproc: build request: %w", err)
	}

	now := time.Now().UTC()

	for name, values := range call.Header {
		req.Header[http.CanonicalHeaderKey(name)] = values
	}

	req.RemoteAddr = remoteAddr
	req.ContentLength = int64(len(call.Body))
	req.Header.Set("Content-Type", jsonContentType)
	req.Header.Set("Content-Length", strconv.Itoa(len(call.Body)))
	req.Header.Set("X-Amz-Target", call.Target)
	req.Header.Set("X-Amz-Date", now.Format(timestampFormat))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+call.Account+"/"+now.Format(dateFormat)+"/"+
		call.Region+"/"+call.Service+"/aws4_request, SignedHeaders="+signedHeaders+", Signature="+signature)

	return req, nil
}

// Enqueue hands call to the worker and returns at once; done, when it is not
// nil, gets the answer on the worker's goroutine. The queue holds MaxQueued
// calls: past that, and after Close, Enqueue refuses the call and reports false.
// The worker starts when the queue goes from empty to not empty and exits when
// it is empty again, so nothing runs while idle. Calls run one at a time, in
// the order they were queued.
func (c *Client) Enqueue(call Call, done func(Response, error)) bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || len(c.queue) >= MaxQueued {
		return false
	}

	c.queue = append(c.queue, queued{call: call, done: done})

	if !c.active {
		c.active = true

		c.worker.Go(c.work)
	}

	return true
}

// Idle reports whether no call is queued and no worker is running.
func (c *Client) Idle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return !c.active && len(c.queue) == 0
}

// Close drops the calls still queued, cancels the one in flight and waits for
// the worker to exit. Later Enqueue calls are refused. It is safe to call more
// than once.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.queue = nil
	c.mu.Unlock()

	c.cancel()
	c.worker.Wait()
}

// work serves the queue until it is empty.
func (c *Client) work() {
	for {
		next, ok := c.next()
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
		resp, err := c.Do(ctx, next.call)

		cancel()

		if next.done != nil {
			next.done(resp, err)
		}
	}
}

// next pops the oldest queued call. When there is none it marks the worker
// gone under the same lock, so a call queued a moment later starts a new one.
func (c *Client) next() (queued, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.queue) == 0 || c.closed {
		c.active = false
		c.queue = nil

		return queued{}, false
	}

	head := c.queue[0]
	c.queue[0] = queued{}
	c.queue = c.queue[1:]

	return head, true
}

// recorder is the ResponseWriter of an in-process call: it keeps the status and
// the body.
type recorder struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

var _ io.Writer = (*recorder)(nil)

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.wrote || status < http.StatusOK {
		return
	}

	r.wrote = true
	r.status = status
}

func (r *recorder) Write(p []byte) (int, error) {
	r.wrote = true

	return r.body.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails.
}
