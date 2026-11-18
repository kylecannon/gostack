package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// This file is the load generator's cell workload. Each worker loops one cycle
// of the cell's shape against one queue:
//
//	single    SendMessage, ReceiveMessage (one message), DeleteMessage
//	batch     SendMessageBatch of 10, ReceiveMessage (up to 10), DeleteMessageBatch
//	longpoll  ReceiveMessage with WaitTimeSeconds=1 started first, then the
//	          SendMessage that wakes it, then DeleteMessage. The receive's latency
//	          includes longPollHold, the pause before the send.
//
// The json protocol goes through aws-sdk-go-v2. The query protocol is a form
// post built here, with the headers the services route on and no signature
// check, which neither gostack nor LocalStack makes.

const (
	longPollHold = 2 * time.Millisecond
	formType     = "application/x-www-form-urlencoded"
	sqsVersion   = "2012-11-05"
	formAuth     = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/sqs/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"
	payloadAlpha  = "abcdefghijklmnopqrstuvwxyz0123456789"
	formErrorHead = 300
)

// message is one message a worker sends.
type message struct {
	body    string
	escaped string // body, as the query form carries it
	group   string
	dedup   string
}

// driver is the wire a cell speaks.
type driver interface {
	send(ctx context.Context, queueURL string, msgs []message) error
	// receive returns the receipt handles of the messages it took.
	receive(ctx context.Context, queueURL string, maxMessages, wait int) ([]string, error)
	remove(ctx context.Context, queueURL string, handles []string) error
}

// sdkDriver is the json protocol through the SDK.
type sdkDriver struct{ client *sqs.Client }

func (d sdkDriver) send(ctx context.Context, queueURL string, msgs []message) error {
	if len(msgs) == 1 {
		in := &sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: &msgs[0].body}
		if msgs[0].group != "" {
			in.MessageGroupId = &msgs[0].group
			in.MessageDeduplicationId = &msgs[0].dedup
		}

		_, err := d.client.SendMessage(ctx, in)
		if err != nil {
			return fmt.Errorf("send message: %w", err)
		}

		return nil
	}

	in := &sqs.SendMessageBatchInput{QueueUrl: &queueURL}

	for i := range msgs {
		entry := types.SendMessageBatchRequestEntry{Id: aws.String(strconv.Itoa(i)), MessageBody: &msgs[i].body}
		if msgs[i].group != "" {
			entry.MessageGroupId = &msgs[i].group
			entry.MessageDeduplicationId = &msgs[i].dedup
		}

		in.Entries = append(in.Entries, entry)
	}

	out, err := d.client.SendMessageBatch(ctx, in)
	if err != nil {
		return fmt.Errorf("send message batch: %w", err)
	}

	if len(out.Failed) > 0 {
		return fmt.Errorf("send message batch: %d entries failed: %s", len(out.Failed), aws.ToString(out.Failed[0].Message))
	}

	return nil
}

func (d sdkDriver) receive(ctx context.Context, queueURL string, maxMessages, wait int) ([]string, error) {
	out, err := d.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &queueURL, MaxNumberOfMessages: int32(maxMessages), WaitTimeSeconds: int32(wait), //nolint:gosec // 1 to 10
	})
	if err != nil {
		return nil, fmt.Errorf("receive message: %w", err)
	}

	handles := make([]string, 0, len(out.Messages))
	for i := range out.Messages {
		handles = append(handles, aws.ToString(out.Messages[i].ReceiptHandle))
	}

	return handles, nil
}

func (d sdkDriver) remove(ctx context.Context, queueURL string, handles []string) error {
	if len(handles) == 1 {
		_, err := d.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: &handles[0]})
		if err != nil {
			return fmt.Errorf("delete message: %w", err)
		}

		return nil
	}

	in := &sqs.DeleteMessageBatchInput{QueueUrl: &queueURL}
	for i := range handles {
		in.Entries = append(in.Entries, types.DeleteMessageBatchRequestEntry{
			Id: aws.String(strconv.Itoa(i)), ReceiptHandle: &handles[i],
		})
	}

	out, err := d.client.DeleteMessageBatch(ctx, in)
	if err != nil {
		return fmt.Errorf("delete message batch: %w", err)
	}

	if len(out.Failed) > 0 {
		return fmt.Errorf("delete message batch: %d entries failed", len(out.Failed))
	}

	return nil
}

// formDriver is the query protocol as a hand-built form post.
type formDriver struct {
	client   *http.Client
	endpoint string
}

func (d formDriver) post(ctx context.Context, operation, queueURL, fields string) ([]byte, error) {
	form := "Action=" + operation + "&Version=" + sqsVersion + "&QueueUrl=" + url.QueryEscape(queueURL) + fields

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint+"/", strings.NewReader(form))
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", operation, err)
	}

	req.Header.Set("Content-Type", formType)
	req.Header.Set("Authorization", formAuth)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read the answer: %w", operation, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d: %.*s", operation, resp.StatusCode, formErrorHead, body)
	}

	return body, nil
}

func (d formDriver) send(ctx context.Context, queueURL string, msgs []message) error {
	if len(msgs) == 1 {
		fields := "&MessageBody=" + msgs[0].escaped
		if msgs[0].group != "" {
			fields += "&MessageGroupId=" + msgs[0].group + "&MessageDeduplicationId=" + msgs[0].dedup
		}

		_, err := d.post(ctx, "SendMessage", queueURL, fields)

		return err
	}

	var fields strings.Builder

	for i := range msgs {
		prefix := "&SendMessageBatchRequestEntry." + strconv.Itoa(i+1) + "."
		fields.WriteString(prefix + "Id=" + strconv.Itoa(i) + prefix + "MessageBody=" + msgs[i].escaped)

		if msgs[i].group != "" {
			fields.WriteString(prefix + "MessageGroupId=" + msgs[i].group + prefix + "MessageDeduplicationId=" + msgs[i].dedup)
		}
	}

	body, err := d.post(ctx, "SendMessageBatch", queueURL, fields.String())
	if err != nil {
		return err
	}

	if bytes.Contains(body, []byte("<BatchResultErrorEntry>")) {
		return errors.New("send message batch: an entry failed")
	}

	return nil
}

var (
	handleOpen  = []byte("<ReceiptHandle>")
	handleClose = []byte("</ReceiptHandle>")
)

func (d formDriver) receive(ctx context.Context, queueURL string, maxMessages, wait int) ([]string, error) {
	body, err := d.post(ctx, "ReceiveMessage", queueURL,
		"&MaxNumberOfMessages="+strconv.Itoa(maxMessages)+"&WaitTimeSeconds="+strconv.Itoa(wait))
	if err != nil {
		return nil, err
	}

	var handles []string

	for {
		start := bytes.Index(body, handleOpen)
		if start < 0 {
			return handles, nil
		}

		body = body[start+len(handleOpen):]

		end := bytes.Index(body, handleClose)
		if end < 0 {
			return nil, errors.New("receive message: the answer has an unclosed ReceiptHandle")
		}

		handles = append(handles, string(body[:end]))
		body = body[end:]
	}
}

func (d formDriver) remove(ctx context.Context, queueURL string, handles []string) error {
	if len(handles) == 1 {
		_, err := d.post(ctx, "DeleteMessage", queueURL, "&ReceiptHandle="+url.QueryEscape(handles[0]))

		return err
	}

	var fields strings.Builder

	for i := range handles {
		prefix := "&DeleteMessageBatchRequestEntry." + strconv.Itoa(i+1) + "."
		fields.WriteString(prefix + "Id=" + strconv.Itoa(i) + prefix + "ReceiptHandle=" + url.QueryEscape(handles[i]))
	}

	body, err := d.post(ctx, "DeleteMessageBatch", queueURL, fields.String())
	if err != nil {
		return err
	}

	if bytes.Contains(body, []byte("<BatchResultErrorEntry>")) {
		return errors.New("delete message batch: an entry failed")
	}

	return nil
}

// cellRun is the result of one run of a cell.
type cellRun struct {
	Seconds       float64 `json:"seconds"`
	Cycles        int     `json:"cycles"`
	CyclesPerSec  float64 `json:"cycles_per_sec"`
	EmptyReceives int     `json:"empty_receives"`
	Send          opStats `json:"send"`
	Receive       opStats `json:"receive"`
	Delete        opStats `json:"delete"`
	Usage         usage   `json:"usage,omitzero"`
}

// samples are the timings one worker collected.
type samples struct {
	send, receive, del []time.Duration
	failed             [3]int
	empty              int
	cycles             int
}

// worker is one goroutine of a cell.
type worker struct {
	spec    cellSpec
	drv     driver
	queue   string
	id      int
	counter int
	entry   message
	out     *samples
}

// messages builds the messages of the next send: one, or a batch of ten. A FIFO
// queue gets a group of the worker's own and a deduplication id no earlier send
// used.
func (w *worker) messages() []message {
	count := 1
	if w.spec.Shape == shapeBatch {
		count = batchSize
	}

	msgs := make([]message, count)

	for i := range msgs {
		msgs[i] = w.entry

		if w.spec.Queue == queueFIFO {
			w.counter++
			msgs[i].group = "g" + strconv.Itoa(w.id)
			msgs[i].dedup = fmt.Sprintf("w%d-%d", w.id, w.counter)
		}
	}

	return msgs
}

// timed runs one call and records its duration under slot, or counts a failure.
func timed(slot *[]time.Duration, failures *int, call func() error) bool {
	began := time.Now()
	err := call()
	took := time.Since(began)

	if err != nil {
		*failures++

		return false
	}

	*slot = append(*slot, took)

	return true
}

// cycle is one pass of the cell's shape.
func (w *worker) cycle(ctx context.Context) {
	s := w.out
	received := make(chan []string, 1)

	msgs := w.messages()
	maxMessages := 1

	if w.spec.Shape == shapeBatch {
		maxMessages = batchSize
	}

	if w.spec.Shape == shapeLongPoll {
		// The receive starts first and waits for the message the send brings.
		var wg sync.WaitGroup

		wg.Go(func() {
			var handles []string

			timed(&s.receive, &s.failed[1], func() error {
				var err error

				handles, err = w.drv.receive(ctx, w.queue, maxMessages, longPollSeconds)

				return err
			})

			received <- handles
		})

		time.Sleep(longPollHold)

		sent := timed(&s.send, &s.failed[0], func() error { return w.drv.send(ctx, w.queue, msgs) })

		wg.Wait()

		handles := <-received
		if !sent {
			return
		}

		w.finish(ctx, handles)

		return
	}

	if !timed(&s.send, &s.failed[0], func() error { return w.drv.send(ctx, w.queue, msgs) }) {
		return
	}

	var handles []string

	if !timed(&s.receive, &s.failed[1], func() error {
		var err error

		handles, err = w.drv.receive(ctx, w.queue, maxMessages, 0)

		return err
	}) {
		return
	}

	w.finish(ctx, handles)
}

// finish deletes what a receive took, and counts the cycle.
func (w *worker) finish(ctx context.Context, handles []string) {
	if len(handles) == 0 {
		w.out.empty++

		return
	}

	if timed(&w.out.del, &w.out.failed[2], func() error { return w.drv.remove(ctx, w.queue, handles) }) {
		w.out.cycles++
	}
}

func payloadBody(n int) string {
	return strings.Repeat(payloadAlpha, n/len(payloadAlpha)+1)[:n]
}

// runCell runs the cell's cycle on spec.Concurrency workers for the duration,
// against a queue it creates and deletes. A call in flight at the deadline
// finishes and counts.
func runCell(ctx context.Context, endpoint string, spec cellSpec, duration time.Duration) (cellRun, error) {
	sdk := newSQSClient(endpoint, spec.Concurrency)

	var drv driver = sdkDriver{client: sdk}
	if spec.Proto == protoQuery {
		drv = formDriver{client: &http.Client{Transport: newTransport(spec.Concurrency)}, endpoint: endpoint}
	}

	name := fmt.Sprintf("bench-cell-%x", time.Now().UnixNano())
	in := &sqs.CreateQueueInput{QueueName: &name}

	if spec.Queue == queueFIFO {
		name += ".fifo"
		in.Attributes = map[string]string{"FifoQueue": "true"}
	}

	created, err := sdk.CreateQueue(ctx, in)
	if err != nil {
		return cellRun{}, fmt.Errorf("create queue: %w", err)
	}

	defer func() {
		_, _ = sdk.DeleteQueue(context.WithoutCancel(ctx), &sqs.DeleteQueueInput{QueueUrl: created.QueueUrl})
	}()

	body := payloadBody(spec.entryBytes())
	entry := message{body: body, escaped: url.QueryEscape(body)}
	results := make([]samples, spec.Concurrency)
	started := time.Now()
	end := started.Add(duration)

	var wg sync.WaitGroup

	for i := range spec.Concurrency {
		w := &worker{spec: spec, drv: drv, queue: *created.QueueUrl, id: i, entry: entry, out: &results[i]}

		wg.Go(func() {
			for time.Now().Before(end) && ctx.Err() == nil {
				if w.out.failed[0]+w.out.failed[1]+w.out.failed[2] > maxWorkerErrors {
					return
				}

				w.cycle(ctx)
			}
		})
	}

	wg.Wait()

	elapsed := time.Since(started)

	return summarize(results, elapsed)
}

func summarize(results []samples, elapsed time.Duration) (cellRun, error) {
	var send, receive, del []time.Duration

	run := cellRun{Seconds: elapsed.Seconds()}

	var failed [3]int

	for _, s := range results {
		send = append(send, s.send...)
		receive = append(receive, s.receive...)
		del = append(del, s.del...)
		run.EmptyReceives += s.empty
		run.Cycles += s.cycles

		for i, f := range s.failed {
			failed[i] += f
		}
	}

	run.Send = statsOf(send, failed[0])
	run.Receive = statsOf(receive, failed[1])
	run.Delete = statsOf(del, failed[2])
	run.CyclesPerSec = float64(run.Cycles) / elapsed.Seconds()

	if run.Cycles == 0 {
		return run, fmt.Errorf("no cycle completed: %d failed sends, %d failed receives, %d failed deletes, "+
			"%d empty receives",
			failed[0], failed[1], failed[2], run.EmptyReceives)
	}

	return run, nil
}
