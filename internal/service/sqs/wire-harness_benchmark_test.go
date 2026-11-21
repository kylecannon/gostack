package sqs_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
)

// This file is the harness of the SQS benchmark matrix. A benchmark builds the
// request bodies once and sends them through the real gateway, in process, the
// way internal/gateway's benchmarks do. Setup that is not the operation under
// test (creating and filling queues, taking receipt handles, purging) runs with
// the timer stopped.

const (
	benchEndpoint    = "http://localhost:4566/"
	benchMessageAttr = 10
	// benchRetainBytes bounds the message bytes a send benchmark keeps in a
	// queue before it purges, so a 1 MiB cell does not grow to the gigabyte.
	benchRetainBytes = 64 << 20
	benchMaxRetain   = 4096
	// benchRefillCalls is how many calls a receive benchmark makes between
	// refills of its queue. Each call adds a receipt handle to every message it
	// takes, and the queue keeps them all until the message is deleted.
	benchRefillCalls = 1000
	// benchHandles is how many receipt handles a delete benchmark takes in one
	// refill. It is a multiple of the batch size.
	benchHandles = 1000
	benchBatch   = 10
	benchGroup   = "bench-group"
	sinkHead     = 512
)

// queueKind is a point on the queue axis.
type queueKind string

const (
	queueStandard queueKind = "standard"
	queueFIFO     queueKind = "fifo"
)

// benchPayload is a point on the payload axis.
type benchPayload struct {
	name  string
	bytes int
}

var (
	payload256B  = benchPayload{"256B", 256}
	payload64Ki  = benchPayload{"64KiB", 64 << 10}
	payload1MiB  = benchPayload{"1MiB", 1 << 20}
	allPayloads  = []benchPayload{payload256B, payload64Ki, payload1MiB}
	fifoPayloads = []benchPayload{payload256B, payload1MiB}
)

// entryBytes is the body size of one entry of a batch: the payload, shrunk so
// ten entries stay inside the 1 MiB limit on a batch request.
func (p benchPayload) entryBytes() int { return min(p.bytes, 1048576/benchBatch) }

func benchBody(n int) string {
	return strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", n/36+1)[:n]
}

// benchText is a point on the text axis: what a body is made of. The zero
// value is benchBody's ASCII, and its cells keep the names they had before the
// axis.
type benchText struct {
	name string
	unit string
}

// benchTexts are the bodies that quote differently in json: two-byte Latin
// among ASCII, three-byte CJK, four-byte emoji, and json text, a third of it
// escaped quotes.
var benchTexts = []benchText{
	{"latin", "caf\u00e9 na\u00efve "},
	{"cjk", "\u65e5\u672c\u8a9e\u30c6\u30ad\u30b9\u30c8"},
	{"emoji", "\U0001f389\U0001f680\u2728"},
	{"json", `{"k":"v","n":1},`},
}

// body is n bytes of the text: whole units, then ASCII to make up the size.
func (t benchText) body(n int) string {
	if t.unit == "" {
		return benchBody(n)
	}

	return strings.Repeat(t.unit, n/len(t.unit)) + strings.Repeat("x", n%len(t.unit))
}

// benchEnv is the provider behind the real gateway.
type benchEnv struct {
	b        *testing.B
	provider *sqs.Provider
	handler  http.Handler
	ctx      context.Context //nolint:containedctx // the direct-call request context of the whole benchmark
	queues   int
	// sent numbers the messages fill sends, so that a FIFO deduplication id is
	// never used twice, whatever the queue was purged of.
	sent int
}

func newBenchEnv(b *testing.B) *benchEnv {
	b.Helper()

	provider := sqs.New(nil, nil)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{provider.Registration()}, nil, nil,
	)
	if err != nil {
		b.Fatal(err)
	}

	// Reset closes the queues, which stops their deadline timers. An armed timer
	// keeps its queue, and every message in it, reachable until it fires, which
	// is 30 seconds for an in-flight message, so without it each cell would
	// leave its queues in memory under the cells after it.
	b.Cleanup(func() {
		provider.Close()
		provider.Reset("", "")
	})

	return &benchEnv{b: b, provider: provider, handler: gw.Handler(), ctx: direct(b.Context())}
}

// queue creates a queue and returns its URL.
func (e *benchEnv) queue(kind queueKind) string {
	e.b.Helper()

	e.queues++
	name := "bench-" + strconv.Itoa(e.queues)
	in := &sqsapi.CreateQueueInput{QueueName: &name}

	if kind == queueFIFO {
		name += ".fifo"
		in.Attributes = map[string]string{"FifoQueue": "true"}
	}

	out, err := e.provider.CreateQueue(e.ctx, in)
	if err != nil {
		e.b.Fatal(err)
	}

	return *out.QueueUrl
}

func (e *benchEnv) purge(url string) {
	e.b.Helper()

	_, err := e.provider.PurgeQueue(e.ctx, &sqsapi.PurgeQueueInput{QueueUrl: &url})
	if err != nil {
		e.b.Fatal(err)
	}
}

// attributes returns n message attributes, or nil for none.
func attributes(n int) map[string]sqsapi.MessageAttributeValue {
	if n == 0 {
		return nil
	}

	out := make(map[string]sqsapi.MessageAttributeValue, n)
	for i := range n {
		out["attr"+strconv.Itoa(i)] = sqsapi.MessageAttributeValue{
			DataType: new("String"), StringValue: new("value" + strconv.Itoa(i)),
		}
	}

	return out
}

// fill sends count messages of bodyBytes bytes to a queue directly. On a FIFO
// queue the messages go to groups message groups in turn, so that a delete
// benchmark can have many messages in flight at once, as a busy queue has.
func (e *benchEnv) fill(url string, kind queueKind, count, bodyBytes, attrs, groups int) {
	e.b.Helper()
	e.fillBody(url, kind, count, benchBody(bodyBytes), attrs, groups)
}

// fillBody is fill with a body of the caller's.
func (e *benchEnv) fillBody(url string, kind queueKind, count int, body string, attrs, groups int) {
	e.b.Helper()

	// The provider keeps the map it is given and never changes it, so the
	// messages share one, as they share the body.
	shared := attributes(attrs)

	for i := range count {
		in := &sqsapi.SendMessageInput{QueueUrl: &url, MessageBody: &body, MessageAttributes: shared}

		if kind == queueFIFO {
			group := benchGroup + strconv.Itoa(i%groups)

			in.MessageGroupId = &group
			e.sent++
			in.MessageDeduplicationId = new("fill-" + strconv.Itoa(e.sent))
		}

		_, err := e.provider.SendMessage(e.ctx, in)
		if err != nil {
			e.b.Fatal(err)
		}
	}
}

// handles receives count messages from a queue directly, each with a visibility
// timeout that outlasts the benchmark, and returns their receipt handles.
func (e *benchEnv) handles(url string, count int) []string {
	e.b.Helper()

	out := make([]string, 0, count)

	for len(out) < count {
		got, err := e.provider.ReceiveMessage(e.ctx, &sqsapi.ReceiveMessageInput{
			QueueUrl: &url, MaxNumberOfMessages: new(int32(benchBatch)),
		})
		if err != nil {
			e.b.Fatal(err)
		}

		if len(got.Messages) == 0 {
			e.b.Fatalf("the queue gave %d of %d messages", len(out), count)
		}

		for _, m := range got.Messages {
			out = append(out, *m.ReceiptHandle)
		}
	}

	return out[:count]
}

// segmented is a request body made of fixed pieces with a few pieces that
// change on each call, such as a deduplication id. The payload is in the fixed
// pieces, so a call does not copy it to build its body, and it is read like any
// request body.
type segmented struct {
	static [][]byte // one more piece than dyn: static[0] dyn[0] static[1] ...
	dyn    [][]byte
	piece  int
	off    int
}

func newSegmented(static []string, dynamic int) *segmented {
	s := &segmented{dyn: make([][]byte, dynamic)}
	for _, piece := range static {
		s.static = append(s.static, []byte(piece))
	}

	return s
}

func (s *segmented) Read(p []byte) (int, error) {
	for s.piece < 2*len(s.static)-1 {
		cur := s.static[s.piece/2]
		if s.piece%2 == 1 {
			cur = s.dyn[s.piece/2]
		}

		if s.off >= len(cur) {
			s.piece++
			s.off = 0

			continue
		}

		n := copy(p, cur[s.off:])
		s.off += n

		return n, nil
	}

	return 0, io.EOF
}

func (*segmented) Close() error { return nil }

// set puts the value of the i-th changing piece.
func (s *segmented) set(i int, value string) {
	s.dyn[i] = append(s.dyn[i][:0], value...)
}

func (s *segmented) rewind() { s.piece, s.off = 0, 0 }

func (s *segmented) length() int64 {
	var n int

	for _, piece := range s.static {
		n += len(piece)
	}

	for _, piece := range s.dyn {
		n += len(piece)
	}

	return int64(n)
}

// sink is the ResponseWriter of a benchmark: it keeps the status and the size,
// and the body only when asked, so the measurement is the server's work.
type sink struct {
	header  http.Header
	status  int
	size    int
	capture bool
	body    bytes.Buffer
	// head is the start of the body, for the message of a failed call.
	head []byte
}

func (s *sink) Header() http.Header { return s.header }

func (s *sink) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
}

func (s *sink) Write(p []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	s.size += len(p)

	if room := sinkHead - len(s.head); room > 0 {
		s.head = append(s.head, p[:min(room, len(p))]...)
	}

	if s.capture {
		s.body.Write(p)
	}

	return len(p), nil
}

func (s *sink) reset() {
	clear(s.header)
	s.status, s.size = 0, 0
	s.head = s.head[:0]
	s.body.Reset()
}

// caller sends the calls of one cell: a protocol, an operation and a body.
type benchCaller struct {
	env    *benchEnv
	header http.Header
	body   *segmented
	sink   *sink
}

const (
	protoJSONName  = "json"
	protoQueryName = "query"
)

func (e *benchEnv) caller(proto, operation string, body *segmented) *benchCaller {
	header := http.Header{"Authorization": {signed("test", usEast1)}}

	if proto == protoJSONName {
		header.Set("Content-Type", jsonType)
		header.Set("X-Amz-Target", "AmazonSQS."+operation)
	} else {
		header.Set("Content-Type", formType)
	}

	return &benchCaller{env: e, header: header, body: body, sink: &sink{header: http.Header{}}}
}

// call sends the current body and fails the benchmark on any answer but 200.
func (c *benchCaller) call() {
	c.body.rewind()
	c.sink.reset()

	req, err := http.NewRequestWithContext(c.env.b.Context(), http.MethodPost, benchEndpoint, c.body)
	if err != nil {
		c.env.b.Fatal(err)
	}

	req.ContentLength = c.body.length()
	req.Header = c.header

	c.env.handler.ServeHTTP(c.sink, req)

	if c.sink.status != http.StatusOK {
		c.env.b.Fatalf("status %d: %s", c.sink.status, c.sink.head)
	}
}

// probe sends the current body once and returns the answer, for a check that
// the cell measures the call it means to.
func (c *benchCaller) probe() string {
	c.sink.capture = true
	c.call()

	c.sink.capture = false

	return c.sink.body.String()
}
