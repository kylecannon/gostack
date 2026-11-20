package inproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/request"
)

const (
	// jsonContentType is the content type of the json 1.0 protocol, the one a
	// call with a Target gets unless it names another.
	jsonContentType = "application/x-amz-json-1.0"

	// host is the Host of an in-process request. The gateway routes a call by
	// its X-Amz-Target, its signing name and its path, so the host only has to
	// be a plain one, and S3 reads a plain host as path style.
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
)

var (
	// ErrNotBound is the error of a call made before Bind gave the client a
	// handler.
	ErrNotBound = errors.New("inproc: no handler bound")

	// ErrAborted is the error of a call whose handler panicked, which is how the
	// gateway aborts a response it has already started.
	ErrAborted = errors.New("inproc: the handler aborted the response")
)

// Call is one request to a service of the same process. Its zero values make
// a json 1.0 call: POST to /, with the X-Amz-Target of Target.
type Call struct {
	// Account and Region are the caller's: they go into the credential scope of
	// the request, which is how the gateway tells which account's state a call
	// acts on. A 12-digit account is its own access key.
	Account string
	Region  string
	// Service is the SigV4 signing name, for example "sqs". The gateway routes
	// a query or rest call by it.
	Service string
	// Target is the X-Amz-Target of a json operation, for example
	// "AmazonSQS.SendMessage". A call without one sends no X-Amz-Target.
	Target string
	// Method is the HTTP method; empty is POST.
	Method string
	// Path is the request path, unescaped, for example "/bucket/key"; empty is
	// "/".
	Path string
	// Query is the query string. A parameter with an empty value is sent as its
	// bare name, so {"location": {""}} is "?location", as S3's subresources are.
	Query url.Values
	// ContentType is the request's Content-Type. Empty keeps the one Header
	// names, or else json 1.0 when Target is set. The json 1.0 default applies
	// only with a Target; a call without one gets no default.
	ContentType string
	// Body is the request body.
	Body []byte
	// Header holds extra request headers.
	Header http.Header
	// Lane, when set, is the whole lane key of an Enqueued call: queued calls
	// with the same Lane run one at a time, in the order they were queued,
	// whatever their destination, and calls with different Lanes run at once.
	// Empty puts the call in the lane of its destination: its region, signing
	// name, method, path and target. It is never sent, and Do and Stream ignore
	// it.
	Lane string
}

// Response is what the handler answered.
type Response struct {
	Status int
	// Header is the response head as the handler sent it with its status.
	Header http.Header
	Body   []byte
}

// OK reports whether the status is a 2xx.
func (r Response) OK() bool {
	return ok(r.Status)
}

// NotEnabled reports whether the gateway answered that the service is not
// enabled: a service that is not registered is a 501 whose message says so.
func (r Response) NotEnabled() bool {
	return r.Status == http.StatusNotImplemented && bytes.Contains(r.Body, []byte("is not enabled"))
}

func ok(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// Client makes calls to a service of the same process through the gateway's
// own handler chain, so that a call behaves as one from outside does and never
// reaches into a service's state. Do and Stream are synchronous and run on the
// caller's goroutine, apart from a streamed handler. Enqueue hands a call to
// workers that exist only while calls are waiting.
//
// The zero value is not ready: use New.
type Client struct {
	queue queue

	handler atomic.Pointer[http.Handler]
}

// New returns a Client with no handler. The gateway is built from the services
// the client is given to, so the handler arrives later, through Bind.
func New() *Client {
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{queue: queue{ctx: ctx, cancel: cancel}}
}

// Bind sets the handler calls are served through, normally the gateway's
// InternalHandler, through a Hub. When h is a Hub, the client joins it, so the
// hub's Drain covers its queue.
func (c *Client) Bind(h http.Handler) {
	c.handler.Store(&h)

	hub, _ := h.(*Hub)
	if hub != nil {
		hub.add(c)
	}

	c.queue.mu.Lock()
	c.queue.hub = hub
	c.queue.mu.Unlock()
}

// Bound reports whether Bind has been called.
func (c *Client) Bound() bool {
	return c != nil && c.handler.Load() != nil
}

// Do serves call through the handler and returns the answer, its body read
// whole. A non-2xx answer is a Response, not an error; an error means the call
// could not be made, or its handler panicked (ErrAborted), as the gateway does
// to abort a response whose status it has already written.
func (c *Client) Do(ctx context.Context, call Call) (Response, error) {
	if !c.Bound() {
		return Response{}, ErrNotBound
	}

	own, cancel := detach(ctx)
	defer cancel()

	req, err := newRequest(own, call, bytes.NewReader(call.Body), int64(len(call.Body)))
	if err != nil {
		return Response{}, err
	}

	var body bytes.Buffer

	w := &writer{header: make(http.Header), body: &body}

	err = serve(*c.handler.Load(), w, req)
	if err != nil {
		return Response{}, err
	}

	w.finish()

	return Response{Status: w.status, Header: w.sent, Body: body.Bytes()}, nil
}

// serve runs the handler and turns a panic into ErrAborted, so that a handler
// that aborts its response fails the one call instead of the caller: a panic on
// a queue worker would end the process, and one on a request's goroutine would
// abort that request too.
func serve(h http.Handler, w http.ResponseWriter, req *http.Request) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", ErrAborted, recovered)
		}
	}()

	h.ServeHTTP(w, req)

	return nil
}

// detach gives a request a context of its own that ends when ctx does and
// carries none of ctx's values: a caller inside the gateway holds the values of
// its own request, among them the mark that names its service as the one the
// request is for, and a call that inherited them would be routed to the caller
// instead of its target.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	own, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)

	return own, func() {
		stop()
		cancel()
	}
}

// newRequest builds the request the gateway sees for call, with body of size
// bytes, or of unknown length when size is negative.
func newRequest(ctx context.Context, call Call, body io.Reader, size int64) (*http.Request, error) {
	target := url.URL{Scheme: "http", Host: host, Path: call.Path, RawQuery: encodeQuery(call.Query)}
	if target.Path == "" {
		target.Path = "/"
	}

	// The mark tells the gateway the call is internal: its body is not charged
	// against the in-flight bound, having been charged when the request that
	// caused it came in.
	req, err := http.NewRequestWithContext(request.MarkInternal(ctx), methodOf(call), target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("inproc: build request: %w", err)
	}

	now := time.Now().UTC()

	for name, values := range call.Header {
		req.Header[http.CanonicalHeaderKey(name)] = values
	}

	req.RemoteAddr = remoteAddr
	req.ContentLength = size

	if size >= 0 {
		req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	} else {
		req.Header.Del("Content-Length")
	}

	switch {
	case call.ContentType != "":
		req.Header.Set("Content-Type", call.ContentType)
	case call.Target != "" && req.Header.Get("Content-Type") == "":
		req.Header.Set("Content-Type", jsonContentType)
	default:
		// The caller's own Content-Type header, or none.
	}

	if call.Target != "" {
		req.Header.Set("X-Amz-Target", call.Target)
	}

	req.Header.Set("X-Amz-Date", now.Format(timestampFormat))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+call.Account+"/"+now.Format(dateFormat)+"/"+
		call.Region+"/"+call.Service+"/aws4_request, SignedHeaders="+signedHeaders+", Signature="+signature)

	return req, nil
}

// methodOf is the call's method, POST when it names none.
func methodOf(call Call) string {
	if call.Method == "" {
		return http.MethodPost
	}

	return call.Method
}

// pathOf is the call's path, / when it names none.
func pathOf(call Call) string {
	if call.Path == "" {
		return "/"
	}

	return call.Path
}

// encodeQuery is url.Values.Encode, except that a parameter with an empty value
// is its bare name.
func encodeQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}

	var b strings.Builder

	for _, name := range slices.Sorted(maps.Keys(query)) {
		escaped := url.QueryEscape(name)

		for _, value := range query[name] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}

			b.WriteString(escaped)

			if value != "" {
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(value))
			}
		}
	}

	return b.String()
}

// writer is the ResponseWriter of an in-process call. The head is fixed when
// the handler first writes the status, a byte of body or a flush, or returns;
// the body goes to body as it is written.
type writer struct {
	header http.Header
	body   io.Writer

	// committed, status and sent are the head as the caller sees it.
	committed bool
	status    int
	sent      http.Header

	// onCommit, when set, runs once the head is fixed.
	onCommit func()
}

var _ http.Flusher = (*writer)(nil)

func (w *writer) Header() http.Header { return w.header }

// WriteHeader fixes the head. An informational status has no body after it, so
// it is not the call's answer and is dropped.
func (w *writer) WriteHeader(status int) {
	if w.committed || status < http.StatusOK {
		return
	}

	w.commit(status)
}

func (w *writer) Write(p []byte) (int, error) {
	w.finish()

	return w.body.Write(p)
}

// Flush fixes the head; the body is never held back.
func (w *writer) Flush() {
	w.finish()
}

// finish fixes the head with 200 when the handler has not written one.
func (w *writer) finish() {
	if !w.committed {
		w.commit(http.StatusOK)
	}
}

func (w *writer) commit(status int) {
	w.committed = true
	w.status = status

	// A client outside reads the head off the wire, which canonicalizes each
	// name, however the handler spelled it.
	w.sent = make(http.Header, len(w.header))
	for name, values := range w.header {
		canonical := http.CanonicalHeaderKey(name)
		w.sent[canonical] = append(w.sent[canonical], values...)
	}

	if w.onCommit != nil {
		w.onCommit()
	}
}
