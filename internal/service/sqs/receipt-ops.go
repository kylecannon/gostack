package sqs

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// receiptHandleParts is the number of space separated fields of a decoded
// receipt handle: id, queue ARN, message id and time of the receive.
const receiptHandleParts = 4

// DeleteMessage deletes the message a receipt handle names. A handle that
// names no message, because the message is gone, is not an error.
func (p *Provider) DeleteMessage(
	ctx context.Context, in *sqsapi.DeleteMessageInput,
) (*sqsapi.DeleteMessageOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	err = q.deleteLocked(deref(in.ReceiptHandle))
	if err != nil {
		return nil, err
	}

	return &sqsapi.DeleteMessageOutput{}, nil
}

// DeleteMessageBatch deletes up to ten messages, and reports each entry that
// failed in the result.
func (p *Provider) DeleteMessageBatch(
	ctx context.Context, in *sqsapi.DeleteMessageBatchInput,
) (*sqsapi.DeleteMessageBatchOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(in.Entries))
	for i := range in.Entries {
		ids[i] = deref(in.Entries[i].Id)
	}

	// LocalStack's own clients lift the limit with a header.
	limit := maxBatchEntries
	if override := headerInt(rc, headerOverrideCount); override != 0 {
		limit = override
	}

	err = checkBatch(ids, limit)
	if err != nil {
		return nil, err
	}

	out := &sqsapi.DeleteMessageBatchOutput{
		Successful: []sqsapi.DeleteMessageBatchResultEntry{}, Failed: []sqsapi.BatchResultErrorEntry{},
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for i := range in.Entries {
		entryErr := q.deleteLocked(deref(in.Entries[i].ReceiptHandle))
		if entryErr != nil {
			out.Failed = append(out.Failed, failedEntry(ids[i], entryErr, false))

			continue
		}

		out.Successful = append(out.Successful, sqsapi.DeleteMessageBatchResultEntry{Id: &ids[i]})
	}

	return out, nil
}

// ChangeMessageVisibility sets how much longer a received message stays hidden.
func (p *Provider) ChangeMessageVisibility(
	ctx context.Context, in *sqsapi.ChangeMessageVisibilityInput,
) (*sqsapi.ChangeMessageVisibilityOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	if in.VisibilityTimeout == nil {
		return nil, missingParameter("VisibilityTimeout")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	err = q.changeVisibilityLocked(deref(in.ReceiptHandle), *in.VisibilityTimeout)
	if err != nil {
		return nil, err
	}

	return &sqsapi.ChangeMessageVisibilityOutput{}, nil
}

// ChangeMessageVisibilityBatch changes up to ten messages, and reports each
// entry that failed in the result.
func (p *Provider) ChangeMessageVisibilityBatch(
	ctx context.Context, in *sqsapi.ChangeMessageVisibilityBatchInput,
) (*sqsapi.ChangeMessageVisibilityBatchOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(in.Entries))
	for i := range in.Entries {
		ids[i] = deref(in.Entries[i].Id)
	}

	err = checkBatch(ids, maxBatchEntries)
	if err != nil {
		return nil, err
	}

	out := &sqsapi.ChangeMessageVisibilityBatchOutput{
		Successful: []sqsapi.ChangeMessageVisibilityBatchResultEntry{}, Failed: []sqsapi.BatchResultErrorEntry{},
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for i := range in.Entries {
		entry := &in.Entries[i]

		entryErr := missingParameter("VisibilityTimeout")
		if entry.VisibilityTimeout != nil {
			entryErr = q.changeVisibilityLocked(deref(entry.ReceiptHandle), *entry.VisibilityTimeout)
		}

		if entryErr != nil {
			out.Failed = append(out.Failed, failedEntry(ids[i], entryErr, false))

			continue
		}

		out.Successful = append(out.Successful, sqsapi.ChangeMessageVisibilityBatchResultEntry{Id: &ids[i]})
	}

	return out, nil
}

// deleteLocked deletes the message of a receipt handle, after checking that the
// handle is one of this queue's.
func (q *Queue) deleteLocked(handle string) *awserr.Error {
	err := q.checkHandle(handle)
	if err != nil {
		return err
	}

	return q.contents.Delete(handle)
}

// changeVisibilityLocked changes the visibility of the message of a receipt
// handle. A handle that names no message is an error here, unlike for a delete.
func (q *Queue) changeVisibilityLocked(handle string, seconds int32) *awserr.Error {
	err := q.checkHandle(handle)
	if err != nil {
		return err
	}

	if !q.contents.ChangeVisibility(handle, time.Duration(seconds)*time.Second) {
		return invalidParameterValue("Value " + handle + " for parameter ReceiptHandle is invalid. " +
			"Reason: Message does not exist or is not available for visibility timeout change.")
	}

	return nil
}

// checkHandle refuses a receipt handle that is not made of the four fields of
// LocalStack's, or that names another queue.
func (q *Queue) checkHandle(handle string) *awserr.Error {
	decoded, err := base64.StdEncoding.DecodeString(handle)
	if err == nil {
		if parts := strings.Split(string(decoded), " "); len(parts) == receiptHandleParts && parts[1] == q.arn {
			return nil
		}
	}

	return sqsapi.NewReceiptHandleIsInvalid(`The input receipt handle "` + handle + `" is not a valid receipt handle.`)
}
