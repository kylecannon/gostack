package sns

import (
	"context"
	"strings"
	"time"

	api "github.com/kylecannon/gostack/internal/api/sns"
)

const (
	// maxBatchEntries is the most entries a PublishBatch takes.
	maxBatchEntries = 10
	// maxBatchIDSize is the longest batch entry Id.
	maxBatchIDSize = 80
	// batchEntryInvalid is the Code of a Failed entry that is refused as
	// InvalidParameter is.
	batchEntryInvalid = "InvalidParameter"
	// msgEmptyMessage is why a Publish with no message text is refused, and
	// the text of a batch entry that has no Message member.
	msgEmptyMessage = "Invalid parameter: Empty message"
)

// PublishBatch publishes up to ten messages to a topic, as LocalStack's
// publish_batch, in its order of checks: the entry count, the topic, the
// entry ids, then each entry in turn and the total size. A failed check fails
// the whole call, as LocalStack and, by the aws.validated
// test_publish_batch_exceptions, AWS answer. The one check that fails an
// entry alone is a missing Message member: the entry is answered in Failed and
// the others are published (docs/compat.md).
//
// Every delivery is sent as the queue's account, as a single Publish is:
// LocalStack sends a batch so and a single publish as its default account,
// but the sender shows only as the SQS SenderId, which every SNS snapshot
// replaces with <sender-id>, and gostack's SQS checks no queue policy
// against it.
func (p *Provider) PublishBatch(ctx context.Context, in *api.PublishBatchInput) (*api.PublishBatchOutput, error) {
	rc, callErr := callOf(ctx)
	if callErr != nil {
		return nil, callErr
	}

	entries := in.PublishBatchRequestEntries

	if len(entries) > maxBatchEntries {
		return nil, api.NewTooManyEntriesInBatchRequestException(
			"The batch request contains more entries than permissible.")
	}

	// AWS's API reference names EmptyBatchRequest for an empty batch.
	// LocalStack answers 500 InternalError: it takes len() of the missing list.
	if len(entries) == 0 {
		return nil, api.NewEmptyBatchRequestException("The batch request doesn't contain any entries.")
	}

	_, t, err := p.topicIn(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	err = checkBatchIDs(entries)
	if err != nil {
		return nil, err
	}

	fifo := strings.HasSuffix(t.ARN, fifoSuffix)

	msgs, err := batchMessages(t, entries)
	if err != nil {
		return nil, err
	}

	accepted := make([]*message, 0, len(msgs))

	for _, m := range msgs {
		if m != nil {
			accepted = append(accepted, m)
		}
	}

	if len(accepted) > 0 {
		if fifo {
			p.lanes.dispatch(p.publishFIFO(rc, t, accepted))
		} else {
			p.lanes.dispatch(p.fanOut(rc, t, accepted))
		}
	}

	return batchOutput(entries, msgs), nil
}

// batchOutput answers each entry: Successful with its message's id and, on a
// FIFO topic, where publishFIFO gave the message a sequence number, that
// number; or Failed, when the entry had no Message member and so no message to
// publish. msgs holds a message for each entry in the same order, nil for one
// that failed.
func batchOutput(entries []api.PublishBatchRequestEntry, msgs []*message) *api.PublishBatchOutput {
	out := &api.PublishBatchOutput{
		Successful: make([]api.PublishBatchResultEntry, 0, len(msgs)),
		Failed:     []api.BatchResultErrorEntry{},
	}

	for i, m := range msgs {
		if m == nil {
			out.Failed = append(out.Failed, api.BatchResultErrorEntry{
				Id: entries[i].Id, Code: new(batchEntryInvalid), Message: new(msgEmptyMessage), SenderFault: new(true),
			})

			continue
		}

		r := api.PublishBatchResultEntry{Id: entries[i].Id, MessageId: new(m.id)}

		if m.sequence != "" {
			r.SequenceNumber = new(m.sequence)
		}

		out.Successful = append(out.Successful, r)
	}

	return out
}

// checkBatchIDs refuses a batch whose entry ids repeat, then an id that is too
// long or holds a character other than a letter, digit, '-' or '_'.
func checkBatchIDs(entries []api.PublishBatchRequestEntry) error {
	seen := make(map[string]struct{}, len(entries))

	for i := range entries {
		seen[deref(entries[i].Id)] = struct{}{}
	}

	if len(seen) != len(entries) {
		return api.NewBatchEntryIdsNotDistinctException("Two or more batch entries in the request have the same Id.")
	}

	for i := range entries {
		id := deref(entries[i].Id)

		if len(id) > maxBatchIDSize {
			return api.NewInvalidBatchEntryIdException(
				"The Id of a batch entry in the batch request is too long: " + id)
		}

		if !validBatchID(id) {
			return api.NewInvalidBatchEntryIdException(
				"The Id of a batch entry in the batch request contains an impermissible character: " + id)
		}
	}

	return nil
}

// validBatchID is BATCH_ENTRY_ID_REGEX: one or more letters, digits, '-' or
// '_'.
func validBatchID(id string) bool {
	if id == "" {
		return false
	}

	for i := range len(id) {
		c := id[i]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}

	return true
}

// batchMessages checks each entry in turn, as LocalStack does, then the total
// size, and returns the entries' messages for t, in entry order. An entry with
// no Message member has no message: its place is nil.
func batchMessages(t *Topic, entries []api.PublishBatchRequestEntry) ([]*message, error) {
	fifo := strings.HasSuffix(t.ARN, fifoSuffix)
	now := time.Now()
	msgs := make([]*message, len(entries))
	total := 0

	for i := range entries {
		e := &entries[i]

		err := checkAttributes(e.MessageAttributes, i+1)
		if err != nil {
			return nil, err
		}

		// An entry with no Message member has no message and fails alone:
		// the model marks Message required and LocalStack answers the whole
		// call 500. An empty Message is not that. LocalStack takes it as it
		// does one space, so it counts its attributes in the total, reads it
		// as a body that does not parse under MessageStructure json, and
		// sends the envelope "Message": "" to a subscriber without raw
		// delivery (only SQS refuses an empty body, for raw delivery).
		if e.Message != nil {
			msgs[i], err = batchMessage(e)
			if err != nil {
				return nil, err
			}

			msgs[i].topicARN, msgs[i].time = t.ARN, now
			total += publishSize(msgs[i].text, e.MessageAttributes)
		}

		// LocalStack checks every entry's FIFO members while reading each
		// one, so the first entry's turn decides.
		if fifo && i == 0 {
			err = checkBatchFIFO(t, entries)
			if err != nil {
				return nil, err
			}
		}
	}

	if total > maxPublishSize {
		return nil, api.NewBatchRequestTooLongException(
			"The length of all the messages put together is more than the limit.")
	}

	return msgs, nil
}

// batchMessage checks one entry's MessageStructure, as LocalStack does for
// each entry, and returns its message.
func batchMessage(e *api.PublishBatchRequestEntry) (*message, error) {
	msg := &message{
		id: newUUID(), text: deref(e.Message), subject: deref(e.Subject), structure: deref(e.MessageStructure),
		attrs: e.MessageAttributes, groupID: deref(e.MessageGroupId), dedupID: deref(e.MessageDeduplicationId),
	}

	if msg.structure == structureJSON {
		var err error

		msg.bodies, err = parseStructure(msg.text)
		if err != nil {
			return nil, err
		}

		if _, ok := msg.bodies[defaultKey]; !ok {
			return nil, api.NewInvalidParameterException(
				"Invalid parameter: Message Structure - No default entry in JSON message body")
		}
	}

	return msg, nil
}

// checkBatchFIFO is LocalStack's check of a batch to the FIFO topic t: each
// entry needs a group id and, unless t deduplicates by content, a
// deduplication id.
func checkBatchFIFO(t *Topic, entries []api.PublishBatchRequestEntry) error {
	for i := range entries {
		if deref(entries[i].MessageGroupId) == "" {
			return api.NewInvalidParameterException(
				"Invalid parameter: The MessageGroupId parameter is required for FIFO topics")
		}
	}

	if t.contentDeduplication() {
		return nil
	}

	for i := range entries {
		if deref(entries[i].MessageDeduplicationId) == "" {
			return api.NewInvalidParameterException("Invalid parameter: The topic should either have " +
				"ContentBasedDeduplication enabled or MessageDeduplicationId provided explicitly")
		}
	}

	return nil
}
