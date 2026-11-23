package sqs_test

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/service/sqs"
)

// The message benchmark matrix (docs/standards.md, "Benchmarks"): each message
// operation through the real gateway, over both wire protocols, on a standard
// and a FIFO queue, across the payload axis. Sub-benchmark names carry the
// axes, for example
//
//	BenchmarkSendMessage/proto=json/queue=fifo/payload=64KiB
//	BenchmarkReceiveMessage/proto=query/queue=standard/max=10/payload=256B/attrs=10
//	BenchmarkReceiveMessage/proto=json/queue=standard/max=1/payload=64KiB/text=cjk
//
// Cells left out, and why. The matrix is the cross product less the cells that
// run no code the kept ones do not, so that `make bench` at -count=10 fits its
// budget:
//
//   - FIFO at 64KiB. A FIFO queue differs from a standard one in the group,
//     deduplication and sequence work, which does not depend on the body size.
//     256B and 1MiB pin the fixed cost and the per-byte cost of that queue
//     kind, and the standard queue covers all three sizes.
//   - Attributes on a FIFO queue, and attributes at 64KiB and 1MiB. The
//     attribute work is the MD5 over the attributes, which is the same code on
//     both kinds of queue and does not read the body, so one size on the
//     standard queue measures it. The attrs=10 cells carry ten String
//     attributes on a 256B body.
//   - Payload and attributes on DeleteMessage and DeleteMessageBatch. They
//     take a receipt handle and read neither.
//   - The text axis on anything but json ReceiveMessage from a standard queue.
//     Body text changes only how json quotes a body on the way out; the
//     ASCII cells measure every other path. The text cells are the non-ASCII
//     and escape-dense bodies that quote differently from ASCII.
//   - ReceiveMessage of ten messages at 1MiB, a call that returns 10 MiB:
//     NUMBERS-PENDING. The 1MiB message received alone and ten of 64KiB stay,
//     so the payload axis is covered by the widest cells under the limit
//     (cellOpts.maxCallBytes).
//   - Persistence is measured by the snapshot benchmarks, and the system bench
//     runs the operations with PERSIST_SQS=1.

// retainChunk is how many calls a send benchmark makes between purges.
func retainChunk(callBytes int) int {
	return max(1, min(benchMaxRetain, benchRetainBytes/max(callBytes, 1)))
}

// cell is one point of the matrix.
type cell struct {
	proto   string
	kind    queueKind
	payload benchPayload
	max     int
	attrs   int
	text    benchText
}

// cellOpts says which axes an operation has.
type cellOpts struct {
	maxes   []int // receive sizes; nil when the operation has none
	payload bool  // the payload axis applies
	attrs   bool  // the attribute cells apply
	texts   bool  // the text cells apply

	// maxCallBytes, when above 0, leaves out the cells whose call returns more
	// than that: max messages of the cell's payload.
	maxCallBytes int
}

// name is the sub-benchmark name of a cell. The axes an operation does not have
// are left out.
func (c cell) name(opts cellOpts) string {
	name := "proto=" + c.proto + "/queue=" + string(c.kind)

	if c.max > 0 {
		name += "/max=" + strconv.Itoa(c.max)
	}

	if opts.payload {
		name += "/payload=" + c.payload.name
	}

	if c.attrs > 0 {
		name += "/attrs=" + strconv.Itoa(c.attrs)
	}

	if c.text.name != "" {
		name += "/text=" + c.text.name
	}

	return name
}

// payloads are the payload sizes a queue kind is measured at.
func (o cellOpts) payloads(kind queueKind) []benchPayload {
	switch {
	case !o.payload:
		return []benchPayload{payload256B}
	case kind == queueFIFO:
		return fifoPayloads
	default:
		return allPayloads
	}
}

// cells lists the matrix of an operation.
func (o cellOpts) cells() []cell {
	maxes := o.maxes
	if maxes == nil {
		maxes = []int{0}
	}

	var out []cell

	add := func(c cell) {
		if o.maxCallBytes == 0 || c.max*c.payload.bytes <= o.maxCallBytes {
			out = append(out, c)
		}
	}

	for _, proto := range []string{protoJSONName, protoQueryName} {
		for _, kind := range []queueKind{queueStandard, queueFIFO} {
			for _, size := range maxes {
				for _, payload := range o.payloads(kind) {
					add(cell{proto: proto, kind: kind, payload: payload, max: size})
				}

				if o.attrs && kind == queueStandard {
					add(cell{proto: proto, kind: kind, payload: payload256B, max: size, attrs: benchMessageAttr})
				}
			}
		}
	}

	if o.texts {
		for _, size := range maxes {
			for _, payload := range allPayloads {
				for _, text := range benchTexts {
					add(cell{proto: protoJSONName, kind: queueStandard, payload: payload, max: size, text: text})
				}
			}
		}
	}

	return out
}

// eachCell runs fn as one sub-benchmark per cell of the matrix.
func eachCell(b *testing.B, opts cellOpts, fn func(b *testing.B, c cell)) {
	b.Helper()

	for _, c := range opts.cells() {
		b.Run(c.name(opts), func(b *testing.B) { fn(b, c) })
	}
}

// bodyBuilder assembles a request body of fixed pieces and changing pieces.
type bodyBuilder struct {
	pieces []string
	cur    strings.Builder
	dyns   int
}

func (w *bodyBuilder) add(s string) { w.cur.WriteString(s) }

// dyn marks where a value that changes on each call goes.
func (w *bodyBuilder) dyn() {
	w.pieces = append(w.pieces, w.cur.String())
	w.cur.Reset()
	w.dyns++
}

func (w *bodyBuilder) done() *segmented {
	return newSegmented(append(w.pieces, w.cur.String()), w.dyns)
}

func jsonAttrs(n int) string {
	var b strings.Builder

	b.WriteString(`{`)

	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(`"attr` + strconv.Itoa(i) + `":{"DataType":"String","StringValue":"value` + strconv.Itoa(i) + `"}`)
	}

	b.WriteString(`}`)

	return b.String()
}

func queryAttrs(prefix string, n int) string {
	var b strings.Builder

	for i := 1; i <= n; i++ {
		at := prefix + "MessageAttribute." + strconv.Itoa(i)
		b.WriteString("&" + at + ".Name=attr" + strconv.Itoa(i-1) + "&" + at + ".Value.DataType=String&" +
			at + ".Value.StringValue=value" + strconv.Itoa(i-1))
	}

	return b.String()
}

const (
	opSend  = "SendMessage"
	opBatch = "SendMessageBatch"
)

// queryHead is the start of a query call, up to the queue URL.
func queryHead(operation string) string {
	return "Action=" + operation + "&Version=2012-11-05&QueueUrl="
}

// writeSendFields adds the members of one message, at the start of a call
// (prefix "") or of a batch entry (prefix "SendMessageBatchRequestEntry.N."),
// and a place for the deduplication id of a FIFO queue.
func writeSendFields(w *bodyBuilder, c cell, prefix string, bodyBytes int) {
	body := benchBody(bodyBytes)

	if c.proto == protoJSONName {
		w.add(`"MessageBody":"` + body + `"`)

		if c.kind == queueFIFO {
			w.add(`,"MessageGroupId":"` + benchGroup + `"`)
		}

		if c.attrs > 0 {
			w.add(`,"MessageAttributes":` + jsonAttrs(c.attrs))
		}

		if c.kind == queueFIFO {
			w.add(`,"MessageDeduplicationId":"`)
			w.dyn()
			w.add(`"`)
		}

		return
	}

	w.add("&" + prefix + "MessageBody=" + body)

	if c.kind == queueFIFO {
		w.add("&" + prefix + "MessageGroupId=" + benchGroup)
	}

	w.add(queryAttrs(prefix, c.attrs))

	if c.kind == queueFIFO {
		w.add("&" + prefix + "MessageDeduplicationId=")
		w.dyn()
	}
}

func sendBody(c cell, queueURL string) *segmented {
	var w bodyBuilder

	if c.proto == protoJSONName {
		w.add(`{"QueueUrl":"` + queueURL + `",`)
		writeSendFields(&w, c, "", c.payload.bytes)
		w.add(`}`)

		return w.done()
	}

	w.add(queryHead(opSend) + url.QueryEscape(queueURL))
	writeSendFields(&w, c, "", c.payload.bytes)

	return w.done()
}

func sendBatchBody(c cell, queueURL string) *segmented {
	var w bodyBuilder

	entry := c.payload.entryBytes()

	if c.proto == protoJSONName {
		w.add(`{"QueueUrl":"` + queueURL + `","Entries":[`)

		for i := range benchBatch {
			if i > 0 {
				w.add(",")
			}

			w.add(`{"Id":"e` + strconv.Itoa(i) + `",`)
			writeSendFields(&w, c, "", entry)
			w.add(`}`)
		}

		w.add(`]}`)

		return w.done()
	}

	w.add(queryHead(opBatch) + url.QueryEscape(queueURL))

	for i := 1; i <= benchBatch; i++ {
		prefix := "SendMessageBatchRequestEntry." + strconv.Itoa(i) + "."
		w.add("&" + prefix + "Id=e" + strconv.Itoa(i))
		writeSendFields(&w, c, prefix, entry)
	}

	return w.done()
}

// dedupe gives every deduplication id of a call a value no earlier call used.
func dedupe(body *segmented, call int) {
	for i := range body.dyn {
		body.set(i, strconv.Itoa(call)+"-"+strconv.Itoa(i))
	}
}

func BenchmarkSendMessage(b *testing.B) {
	eachCell(b, cellOpts{payload: true, attrs: true}, func(b *testing.B, c cell) {
		env := newBenchEnv(b)
		queueURL := env.queue(c.kind)
		body := sendBody(c, queueURL)
		caller := env.caller(c.proto, opSend, body)

		dedupe(body, 0)
		expectCount(b, caller.probe(), "MessageId", 1)
		env.purge(queueURL)

		chunk := retainChunk(c.payload.bytes)

		b.ReportAllocs()
		b.SetBytes(int64(c.payload.bytes))

		for i := 1; b.Loop(); i++ {
			if i%chunk == 0 {
				b.StopTimer()
				env.purge(queueURL)
				b.StartTimer()
			}

			dedupe(body, i)
			caller.call()
		}
	})
}

func BenchmarkSendMessageBatch(b *testing.B) {
	eachCell(b, cellOpts{payload: true, attrs: true}, func(b *testing.B, c cell) {
		env := newBenchEnv(b)
		queueURL := env.queue(c.kind)
		body := sendBatchBody(c, queueURL)
		caller := env.caller(c.proto, opBatch, body)

		dedupe(body, 0)
		expectCount(b, caller.probe(), "MessageId", benchBatch)
		env.purge(queueURL)

		total := c.payload.entryBytes() * benchBatch
		chunk := retainChunk(total)

		b.ReportAllocs()
		b.SetBytes(int64(total))

		for i := 1; b.Loop(); i++ {
			if i%chunk == 0 {
				b.StopTimer()
				env.purge(queueURL)
				b.StartTimer()
			}

			dedupe(body, i)
			caller.call()
		}
	})
}

func receiveBody(c cell, queueURL string) *segmented {
	var w bodyBuilder

	if c.proto == protoJSONName {
		w.add(`{"QueueUrl":"` + queueURL + `","MaxNumberOfMessages":` + strconv.Itoa(c.max) + `,"VisibilityTimeout":0`)

		if c.attrs > 0 {
			w.add(`,"MessageAttributeNames":["All"]`)
		}

		w.add(`}`)

		return w.done()
	}

	w.add(queryHead("ReceiveMessage") + url.QueryEscape(queueURL) +
		"&MaxNumberOfMessages=" + strconv.Itoa(c.max) + "&VisibilityTimeout=0")

	if c.attrs > 0 {
		w.add("&MessageAttributeName.1=All")
	}

	return w.done()
}

// maxReceiveBytes is the most one ReceiveMessage cell returns in a call: ten
// messages of 1MiB are over it, and ten of 64KiB under it.
const maxReceiveBytes = 4 << 20

// BenchmarkReceiveMessage receives from a queue that holds ten messages, with a
// visibility timeout of 0, so that each call sees the same messages and the
// queue never runs dry. A queue keeps every receipt handle it has issued for a
// message until the message is deleted, so the queue is purged and filled again
// every benchRefillCalls calls, with the timer stopped, and the memory of a cell
// stays the same however long it runs.
func BenchmarkReceiveMessage(b *testing.B) {
	opts := cellOpts{
		payload: true, attrs: true, texts: true, maxes: []int{1, benchBatch}, maxCallBytes: maxReceiveBytes,
	}
	eachCell(b, opts, func(b *testing.B, c cell) {
		env := newBenchEnv(b)
		queueURL := env.queue(c.kind)
		body := c.text.body(c.payload.bytes)
		env.fillBody(queueURL, c.kind, benchBatch, body, c.attrs, 1)

		caller := env.caller(c.proto, "ReceiveMessage", receiveBody(c, queueURL))
		expectCount(b, caller.probe(), "MessageId", c.max)

		b.ReportAllocs()
		b.SetBytes(int64(c.payload.bytes * c.max))

		for i := 1; b.Loop(); i++ {
			if i%benchRefillCalls == 0 {
				b.StopTimer()
				env.purge(queueURL)
				env.fillBody(queueURL, c.kind, benchBatch, body, c.attrs, 1)
				b.StartTimer()
			}

			caller.call()
		}
	})
}

// deleteQueue is a queue and the receipt handles of its in-flight messages that
// the delete benchmarks have left to delete.
type deleteQueue struct {
	env  *benchEnv
	kind queueKind
	url  string
	left []string
}

// next returns the next count receipt handles, taking a new set while the timer
// is stopped when the last one is used up.
func (q *deleteQueue) next(b *testing.B, count int) []string {
	if len(q.left) < count {
		b.StopTimer()
		q.env.purge(q.url)
		q.env.fill(q.url, q.kind, benchHandles, payload256B.bytes, 0, benchHandles)
		q.left = q.env.handles(q.url, benchHandles)
		b.StartTimer()
	}

	got := q.left[:count]
	q.left = q.left[count:]

	return got
}

func BenchmarkDeleteMessage(b *testing.B) {
	eachCell(b, cellOpts{}, func(b *testing.B, c cell) {
		env := newBenchEnv(b)
		queue := &deleteQueue{env: env, kind: c.kind, url: env.queue(c.kind)}

		var w bodyBuilder

		if c.proto == protoJSONName {
			w.add(`{"QueueUrl":"` + queue.url + `","ReceiptHandle":"`)
			w.dyn()
			w.add(`"}`)
		} else {
			w.add(queryHead("DeleteMessage") + url.QueryEscape(queue.url) + "&ReceiptHandle=")
			w.dyn()
		}

		body := w.done()
		caller := env.caller(c.proto, "DeleteMessage", body)

		b.ReportAllocs()

		for b.Loop() {
			body.set(0, handleOf(c.proto, queue.next(b, 1)[0]))
			caller.call()
		}
	})
}

func BenchmarkDeleteMessageBatch(b *testing.B) {
	eachCell(b, cellOpts{}, func(b *testing.B, c cell) {
		env := newBenchEnv(b)
		queue := &deleteQueue{env: env, kind: c.kind, url: env.queue(c.kind)}

		var w bodyBuilder

		if c.proto == protoJSONName {
			w.add(`{"QueueUrl":"` + queue.url + `","Entries":[`)

			for i := range benchBatch {
				if i > 0 {
					w.add(",")
				}

				w.add(`{"Id":"e` + strconv.Itoa(i) + `","ReceiptHandle":"`)
				w.dyn()
				w.add(`"}`)
			}

			w.add(`]}`)
		} else {
			w.add(queryHead("DeleteMessageBatch") + url.QueryEscape(queue.url))

			for i := 1; i <= benchBatch; i++ {
				prefix := "&DeleteMessageBatchRequestEntry." + strconv.Itoa(i)
				w.add(prefix + ".Id=e" + strconv.Itoa(i) + prefix + ".ReceiptHandle=")
				w.dyn()
			}
		}

		body := w.done()
		caller := env.caller(c.proto, "DeleteMessageBatch", body)

		b.ReportAllocs()

		for b.Loop() {
			for i, handle := range queue.next(b, benchBatch) {
				body.set(i, handleOf(c.proto, handle))
			}

			caller.call()
		}
	})
}

// handleOf writes a receipt handle the way the protocol carries it: the query
// form escapes it.
func handleOf(proto, handle string) string {
	if proto == protoQueryName {
		return url.QueryEscape(handle)
	}

	return handle
}

// expectCount fails the benchmark unless the answer holds want elements named
// member, so a cell cannot measure a call that did something else.
func expectCount(b *testing.B, answer, member string, want int) {
	b.Helper()

	got := strings.Count(answer, `"`+member+`"`) + strings.Count(answer, "<"+member+">")
	if got != want {
		b.Fatalf("the answer has %d %s, want %d: %.300s", got, member, want, answer)
	}
}

// BenchmarkSendReceiveDelete measures a message's whole life in process: one
// SendMessage, one ReceiveMessage and one DeleteMessage on the provider, without
// the gateway or a codec.
func BenchmarkSendReceiveDelete(b *testing.B) {
	p := sqs.New(nil, nil)
	ctx := direct(b.Context())

	created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("bench")})
	if err != nil {
		b.Fatal(err)
	}

	send := &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: new("hello")}
	receive := &sqsapi.ReceiveMessageInput{QueueUrl: created.QueueUrl}

	b.ReportAllocs()

	for b.Loop() {
		_, err = p.SendMessage(ctx, send)
		if err != nil {
			b.Fatal(err)
		}

		out, err := p.ReceiveMessage(ctx, receive)
		if err != nil || len(out.Messages) != 1 {
			b.Fatal("receive", err, out)
		}

		_, err = p.DeleteMessage(ctx, &sqsapi.DeleteMessageInput{
			QueueUrl: created.QueueUrl, ReceiptHandle: out.Messages[0].ReceiptHandle,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
