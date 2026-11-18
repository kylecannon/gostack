package sqs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

// SendMessage puts a message on a queue, hidden for DelaySeconds when the call
// or the queue sets one.
func (p *Provider) SendMessage(ctx context.Context, in *sqsapi.SendMessageInput) (*sqsapi.SendMessageOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	req := &sendRequest{
		body: in.MessageBody, delay: in.DelaySeconds, attributes: in.MessageAttributes,
		system: in.MessageSystemAttributes, dedupID: in.MessageDeduplicationId, groupID: in.MessageGroupId,
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	m, err := q.sendLocked(rc, req)
	if err != nil {
		return nil, err
	}

	out := &sqsapi.SendMessageOutput{MessageId: &m.id, MD5OfMessageBody: &m.bodyMD5}
	if m.attributes != nil {
		out.MD5OfMessageAttributes = &m.attributesMD5
	}

	if m.sequence != "" {
		out.SequenceNumber = &m.sequence
	}

	if in.MessageSystemAttributes != nil {
		out.MD5OfMessageSystemAttributes = new(systemAttributesMD5(in.MessageSystemAttributes))
	}

	return out, nil
}

// SendMessageBatch sends up to ten messages. A problem with the batch refuses
// all of it, a problem with an entry only fails that entry.
func (p *Provider) SendMessageBatch(
	ctx context.Context, in *sqsapi.SendMessageBatchInput,
) (*sqsapi.SendMessageBatchOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(in.Entries))
	size := 0

	for i := range in.Entries {
		ids[i] = deref(in.Entries[i].Id)
		size += len(deref(in.Entries[i].MessageBody)) + attributesSize(in.Entries[i].MessageAttributes)
	}

	err = checkBatch(ids, maxBatchEntries)
	if err != nil {
		return nil, err
	}

	err = q.checkFIFOBatch(in.Entries)
	if err != nil {
		return nil, err
	}

	if size > maxBatchBytes {
		return nil, batchTooLong(size)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	out := &sqsapi.SendMessageBatchOutput{}

	for i := range in.Entries {
		result, failure := q.sendEntryLocked(rc, &in.Entries[i], ids[i])
		if failure != nil {
			out.Failed = append(out.Failed, *failure)
		} else {
			out.Successful = append(out.Successful, *result)
		}
	}

	return out, nil
}

// sendEntryLocked sends one entry of a batch. It returns the result entry, or
// the failure entry when the message was refused. The caller holds the queue's
// lock.
func (q *Queue) sendEntryLocked(
	rc *request.Context, entry *sqsapi.SendMessageBatchRequestEntry, id string,
) (*sqsapi.SendMessageBatchResultEntry, *sqsapi.BatchResultErrorEntry) {
	req := &sendRequest{
		body: entry.MessageBody, delay: entry.DelaySeconds, attributes: entry.MessageAttributes,
		system: entry.MessageSystemAttributes, dedupID: entry.MessageDeduplicationId, groupID: entry.MessageGroupId,
	}

	m, err := q.sendLocked(rc, req)
	if err != nil {
		failure := failedEntry(id, err, err.Fault == awserr.FaultSender)

		return nil, &failure
	}

	result := &sqsapi.SendMessageBatchResultEntry{Id: &id, MessageId: &m.id, MD5OfMessageBody: &m.bodyMD5}
	if m.attributes != nil {
		result.MD5OfMessageAttributes = &m.attributesMD5
	}

	if m.sequence != "" {
		result.SequenceNumber = &m.sequence
	}

	if entry.MessageSystemAttributes != nil {
		result.MD5OfMessageSystemAttributes = new(systemAttributesMD5(entry.MessageSystemAttributes))
	}

	return result, nil
}

// sendLocked validates a message and puts it on the queue. The caller holds the
// queue's lock.
func (q *Queue) sendLocked(rc *request.Context, req *sendRequest) (*message, *awserr.Error) {
	err := validateSend(req, q.intAttr(attrMaximumMessageSize))
	if err != nil {
		return nil, err
	}

	m := newMessage(rc, req, time.Now())

	err = q.putLocked(m, req.delay)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// putLocked checks a message against the type of the queue and puts it on the
// queue. delay is the DelaySeconds of the send, nil when it has none. The caller
// holds the queue's lock.
//
// A standard queue takes no deduplication id, and a message's delay is its own
// or else the queue's. A FIFO queue takes no delay on the message, wants a group
// and a deduplication id (the queue makes one from the body with
// ContentBasedDeduplication on), and always delays by its own DelaySeconds.
func (q *Queue) putLocked(m *message, delay *int32) *awserr.Error {
	seconds := q.intAttr(attrDelaySeconds)

	if q.kind == kindFIFO {
		err := q.admitFIFO(m, delay)
		if err != nil {
			return err
		}
	} else {
		if m.dedupID != "" {
			return invalidParameterValue("Value " + m.dedupID + " for parameter MessageDeduplicationId is invalid. " +
				"Reason: The request includes a parameter that is not valid for this queue type.")
		}

		if delay != nil {
			seconds = int(*delay)
		}
	}

	q.contents.Put(m, time.Duration(seconds)*time.Second)

	return nil
}

// admitFIFO runs LocalStack's checks of a message for a FIFO queue, in its order,
// and gives the message the deduplication id the queue deduplicates it on.
func (q *Queue) admitFIFO(m *message, delay *int32) *awserr.Error {
	if delay != nil && *delay != 0 {
		return invalidParameterValue("Value " + strconv.Itoa(int(*delay)) + " for parameter DelaySeconds is invalid. " +
			"Reason: The request include parameter that is not valid for this queue type.")
	}

	if m.groupID == "" {
		return missingParameter("MessageGroupId")
	}

	if m.dedupID == "" {
		if q.dedupIDRequired() {
			return invalidParameterValue(messageDedupIDRequired)
		}

		m.dedupID = sha256Hex(m.body)
	}

	return nil
}

// messageDedupIDRequired is the error of a send without a deduplication id to a
// queue that cannot make one.
const messageDedupIDRequired = "The queue should either have ContentBasedDeduplication enabled or " +
	"MessageDeduplicationId provided explicitly"

// dedupIDRequired reports whether a send has to name its deduplication id: the
// queue is FIFO and its ContentBasedDeduplication is "false", in any case. The
// caller holds the queue's lock.
func (q *Queue) dedupIDRequired() bool {
	value, found := q.attrs[attrContentBasedDeduplication]

	return q.kind == kindFIFO && (!found || strings.EqualFold(value, "false"))
}

// checkFIFOBatch is the part of LocalStack's batch check that depends on the
// queue: an entry of a FIFO queue has a group, and a deduplication id unless the
// queue makes one. It refuses the whole batch, not the entry.
func (q *Queue) checkFIFOBatch(entries []sqsapi.SendMessageBatchRequestEntry) *awserr.Error {
	if q.kind != kindFIFO {
		return nil
	}

	q.mu.Lock()
	required := q.dedupIDRequired()
	q.mu.Unlock()

	for i := range entries {
		if required && deref(entries[i].MessageDeduplicationId) == "" {
			return invalidParameterValue(messageDedupIDRequired)
		}

		if deref(entries[i].MessageGroupId) == "" {
			return invalidParameterValue("The request must contain the parameter MessageGroupId.")
		}
	}

	return nil
}

// sha256Hex is the checksum a FIFO queue deduplicates a body on.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))

	return hex.EncodeToString(sum[:])
}

// receiveParams are the settings of one ReceiveMessage call.
type receiveParams struct {
	count      int
	wait       time.Duration
	visibility *int32
	// fill keeps a long poll waiting until count messages came, not only until
	// the first one.
	fill bool
}

// Headers that change how ReceiveMessage reads its parameters, which LocalStack's
// own clients use to go past the API's limits.
const (
	headerOverrideCount = "X-Localstack-Sqs-Override-Message-Count"
	headerOverrideWait  = "X-Localstack-Sqs-Override-Wait-Time-Seconds"

	maxReceiveCount = 10
	maxWaitSeconds  = 20
)

// ReceiveMessage takes up to MaxNumberOfMessages visible messages, hiding them
// for the visibility timeout. With a wait time it blocks until a message is
// visible, the time is up or the request is cancelled.
func (p *Provider) ReceiveMessage(
	ctx context.Context, in *sqsapi.ReceiveMessageInput,
) (*sqsapi.ReceiveMessageOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	params, err := q.receiveParams(rc, in, p.settings().DisableMaxNumberOfMessageLimit)
	if err != nil {
		return nil, err
	}

	if params.wait > 0 {
		var release context.CancelFunc

		ctx, release = p.untilShutdown(ctx)
		defer release()
	}

	taken, changed, err := q.poll(ctx, params, &p.transfer, func(dead []deadLetter) *awserr.Error {
		return p.deadLetter(rc, q, dead)
	})

	// A receive that found nothing and dropped nothing leaves the state as it
	// was, so a client that polls an empty queue does not make persistence save.
	if changed {
		p.notifyChanged()
	}

	if err != nil {
		return nil, err
	}

	if len(taken) == 0 {
		return &sqsapi.ReceiveMessageOutput{}, nil
	}

	systemNames := in.MessageSystemAttributeNames
	if len(systemNames) == 0 {
		systemNames = in.AttributeNames
	}

	messages := make([]sqsapi.Message, len(taken))
	for i := range taken {
		messages[i] = toAPIMessage(&taken[i], systemNames, in.MessageAttributeNames)
	}

	return &sqsapi.ReceiveMessageOutput{Messages: messages}, nil
}

// receiveParams reads and checks the parameters of a ReceiveMessage call.
// noLimit lifts the check of MaxNumberOfMessages, as
// SQS_DISABLE_MAX_NUMBER_OF_MESSAGE_LIMIT does.
func (q *Queue) receiveParams(
	rc *request.Context, in *sqsapi.ReceiveMessageInput, noLimit bool,
) (receiveParams, *awserr.Error) {
	params := receiveParams{visibility: in.VisibilityTimeout}

	override := headerInt(rc, headerOverrideWait)

	switch {
	case override != 0:
		params.wait = time.Duration(override) * time.Second
		params.fill = true
	case in.WaitTimeSeconds == nil:
		q.mu.Lock()
		params.wait = time.Duration(q.intAttr(attrReceiveMessageWaitTimeSeconds)) * time.Second
		q.mu.Unlock()
	case *in.WaitTimeSeconds < 0 || *in.WaitTimeSeconds > maxWaitSeconds:
		return params, invalidParameterValue("Value " + strconv.Itoa(int(*in.WaitTimeSeconds)) +
			" for parameter WaitTimeSeconds is invalid. Reason: Must be >= 0 and <= 20, if provided.")
	default:
		params.wait = time.Duration(*in.WaitTimeSeconds) * time.Second
	}

	// An absent or zero count is one message, as in LocalStack.
	count := 1
	if in.MaxNumberOfMessages != nil && *in.MaxNumberOfMessages != 0 {
		count = int(*in.MaxNumberOfMessages)
	}

	switch override := headerInt(rc, headerOverrideCount); {
	case override != 0:
		count = override
	case count == -1:
		// Everything that is visible now.
		q.mu.Lock()
		count = q.contents.Counts().Visible
		q.mu.Unlock()
	case (count < 1 || count > maxReceiveCount) && !noLimit:
		return params, invalidParameterValue("Value " + strconv.Itoa(count) +
			" for parameter MaxNumberOfMessages is invalid. Reason: Must be between 1 and 10, if provided.")
	default:
		// A count from 1 to 10 is as asked.
	}

	params.count = count

	// Only the header can make the wait negative.
	if params.wait < 0 {
		return params, negativeWait()
	}

	return params, nil
}

// headerInt reads an integer header of the request, 0 when it is absent or not
// a number.
func headerInt(rc *request.Context, name string) int {
	if rc.Request == nil {
		return 0
	}

	value, err := strconv.Atoi(rc.Request.Header.Get(name))
	if err != nil {
		return 0
	}

	return value
}

// holdSlack is how much longer than the wait a long poll that fills its
// response keeps the messages it took hidden. The poll sets their real
// visibility when it ends, so the slack only matters to a state saved, or a
// process killed, in between.
const holdSlack = time.Minute

// poll receives messages, waiting as the parameters say. It never holds the
// queue's lock while it waits: it asks the contents for a channel that is closed
// when a message may be visible, and sleeps on that, on the end of the wait and
// on the request.
//
// A poll that fills its response keeps what it took hidden until it ends, and
// only then gives the messages their visibility timeout, as LocalStack does:
// otherwise a message that is visible again, or whose timeout is short, would be
// taken a second time into the same response.
//
// A wait that runs out looks at the queue once more before it gives up, as
// LocalStack's get does. A wait that the request ended does not.
//
// changed reports whether the receive changed state: it took a message, moved
// one to a dead-letter queue or dropped one past retention.
//
// Each look at the queue is a transfer when it finds a message past the
// redrive limit: the message leaves the queue here and is on the dead-letter
// queue only once deadLetter has put it there. transfer is held for reading from
// before the look until deadLetter is done, so a snapshot sees the message in
// one queue of the two.
func (q *Queue) poll(
	ctx context.Context, params receiveParams, transfer *sync.RWMutex, deadLetter func([]deadLetter) *awserr.Error,
) (taken []delivery, changed bool, _ *awserr.Error) {
	parent := ctx

	if params.wait > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, params.wait)
		defer cancel()
	}

	// held is what a poll that fills its response has to release when it ends,
	// even when a dead letter fails and the response is dropped.
	var held []delivery

	defer func() { q.release(params, held) }()

	last := false

	for {
		got, wake, err := q.lookAndMove(params, params.count-len(taken), transfer, deadLetter)
		taken = append(taken, got.taken...)
		changed = changed || len(got.taken) > 0 || len(got.dead) > 0 || got.expired

		if params.fill {
			held = append(held, got.taken...)
		}

		if err != nil {
			return nil, changed, err
		}

		done := len(taken) >= params.count || (len(taken) > 0 && !params.fill)
		if done || wake == nil || params.wait <= 0 || last {
			return taken, changed, nil
		}

		select {
		case <-wake:
		case <-ctx.Done():
			if parent.Err() != nil {
				return taken, changed, nil
			}

			// The wait ran out, not the request: look once more.
			last = true
		}
	}
}

// lookAndMove is one look at the queue of a poll, and the dead-letter moves it
// causes. The queue's lock is free by the time the dead letters go to a queue
// that has a lock of its own.
func (q *Queue) lookAndMove(
	params receiveParams, want int, transfer *sync.RWMutex, deadLetter func([]deadLetter) *awserr.Error,
) (receiveResult, <-chan struct{}, *awserr.Error) {
	transfer.RLock()
	defer transfer.RUnlock()

	got, wake := q.receiveOnce(params, want)
	if len(got.dead) == 0 {
		return got, wake, nil
	}

	return got, wake, deadLetter(got.dead)
}

// release gives the messages of a poll that filled its response the visibility
// timeout they were held back instead of.
func (q *Queue) release(params receiveParams, held []delivery) {
	if len(held) == 0 {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	visibility := q.receiveVisibility(params)

	for i := range held {
		q.contents.Release(held[i].handle, visibility)
	}
}

// receiveVisibility is how long a receive hides what it takes. The caller holds
// the queue's lock.
func (q *Queue) receiveVisibility(params receiveParams) time.Duration {
	if params.visibility != nil {
		return time.Duration(*params.visibility) * time.Second
	}

	return time.Duration(q.intAttr(attrVisibilityTimeout)) * time.Second
}

// receiveOnce takes what is visible now, and returns the wake channel to wait
// on for more.
func (q *Queue) receiveOnce(params receiveParams, want int) (receiveResult, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()

	visibility := q.receiveVisibility(params)
	if params.fill {
		visibility = params.wait + holdSlack
	}

	got := q.contents.Receive(want, visibility)

	if params.fill {
		for i := range got.taken {
			q.contents.Hold(got.taken[i].handle)
		}
	}

	var wake <-chan struct{}
	if params.wait > 0 {
		wake = q.contents.Wake()
	}

	return got, wake
}

// toAPIMessage builds the message a receiver sees from a delivery, with the
// attributes the receiver asked for.
func toAPIMessage(d *delivery, systemNames, attributeNames []string) sqsapi.Message {
	m := d.msg
	out := sqsapi.Message{
		MessageId:     &m.id,
		ReceiptHandle: &d.handle,
		MD5OfBody:     &m.bodyMD5,
		Body:          &m.body,
	}

	if wanted := filterSystemAttributes(m, d, systemNames); len(wanted) > 0 {
		out.Attributes = wanted
	}

	if wanted := filterMessageAttributes(m.attributes, attributeNames); len(wanted) > 0 {
		out.MessageAttributes = wanted
		out.MD5OfMessageAttributes = new(attributesMD5(wanted))
	}

	return out
}

// failedEntry is the entry of a batch result for an entry that failed.
func failedEntry(id string, err *awserr.Error, senderFault bool) sqsapi.BatchResultErrorEntry {
	return sqsapi.BatchResultErrorEntry{
		Id:          &id,
		SenderFault: &senderFault,
		Code:        &err.Code,
		Message:     &err.Message,
	}
}
