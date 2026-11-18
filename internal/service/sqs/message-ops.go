package sqs

import (
	"context"
	"strconv"
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

	err = q.requireStandard(opSendMessage)
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

	err = q.requireStandard(opSendMessageBatch)
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

	// Only a FIFO queue takes a deduplication id.
	if req.dedupID != nil {
		return nil, invalidParameterValue("Value " + *req.dedupID + " for parameter MessageDeduplicationId is invalid. " +
			"Reason: The request includes a parameter that is not valid for this queue type.")
	}

	delay := q.intAttr(attrDelaySeconds)
	if req.delay != nil {
		delay = int(*req.delay)
	}

	m := newMessage(rc, req, time.Now())
	q.contents.Put(m, time.Duration(delay)*time.Second)

	return m, nil
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

	err = q.requireStandard(opReceiveMessage)
	if err != nil {
		return nil, err
	}

	params, err := q.receiveParams(rc, in)
	if err != nil {
		return nil, err
	}

	taken := q.poll(ctx, params)
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
func (q *Queue) receiveParams(rc *request.Context, in *sqsapi.ReceiveMessageInput) (receiveParams, *awserr.Error) {
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
	case count < 1 || count > maxReceiveCount:
		return params, invalidParameterValue("Value " + strconv.Itoa(count) +
			" for parameter MaxNumberOfMessages is invalid. Reason: Must be between 1 and 10, if provided.")
	default:
		// A count from 1 to 10 is as asked.
	}

	params.count = count

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

// poll receives messages, waiting as the parameters say. It never holds the
// queue's lock while it waits: it asks the contents for a channel that is closed
// when a message may be visible, and sleeps on that, on the end of the wait and
// on the request.
func (q *Queue) poll(ctx context.Context, params receiveParams) []delivery {
	var taken []delivery

	if params.wait > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, params.wait)
		defer cancel()
	}

	for {
		batch, wake := q.receiveOnce(params, params.count-len(taken))
		taken = append(taken, batch...)

		done := len(taken) >= params.count || (len(taken) > 0 && !params.fill)
		if done || wake == nil || params.wait <= 0 {
			return taken
		}

		select {
		case <-wake:
		case <-ctx.Done():
			return taken
		}
	}
}

// receiveOnce takes what is visible now, and returns the wake channel to wait
// on for more.
func (q *Queue) receiveOnce(params receiveParams, want int) ([]delivery, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()

	visibility := q.intAttr(attrVisibilityTimeout)
	if params.visibility != nil {
		visibility = int(*params.visibility)
	}

	taken := q.contents.Receive(want, time.Duration(visibility)*time.Second)

	var wake <-chan struct{}
	if params.wait > 0 {
		wake = q.contents.Wake()
	}

	return taken, wake
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
