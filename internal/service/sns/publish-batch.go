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
)

// PublishBatch publishes up to ten messages to a topic, as LocalStack's
// publish_batch, in its order of checks: the entry count, the topic, the
// entry ids, then each entry in turn and the total size. A failed check fails
// the whole call, so Failed is always empty, as LocalStack and, by the
// aws.validated test_publish_batch_exceptions, AWS answer.
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

	// LocalStack takes an empty batch and publishes nothing; AWS's API
	// reference names EmptyBatchRequest for it.
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

	if fifo {
		p.lanes.dispatch(p.publishFIFO(rc, t, msgs))
	} else {
		p.lanes.dispatch(p.fanOut(rc, t, msgs))
	}

	out := &api.PublishBatchOutput{
		Successful: make([]api.PublishBatchResultEntry, len(entries)),
		Failed:     []api.BatchResultErrorEntry{},
	}

	for i, m := range msgs {
		r := &out.Successful[i]
		r.Id, r.MessageId = entries[i].Id, new(m.id)

		if fifo {
			r.SequenceNumber = new(m.sequence)
		}
	}

	return out, nil
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
// size, and returns the entries' messages for t.
func batchMessages(t *Topic, entries []api.PublishBatchRequestEntry) ([]*message, error) {
	fifo := strings.HasSuffix(t.ARN, fifoSuffix)
	now := time.Now()
	msgs := make([]*message, len(entries))
	total := 0

	for i := range entries {
		e := &entries[i]

		msg, err := batchMessage(e, i+1)
		if err != nil {
			return nil, err
		}

		total += publishSize(msg.text, e.MessageAttributes)

		// LocalStack checks every entry's FIFO members while reading each
		// one, so the first entry's turn decides.
		if fifo && i == 0 {
			err = checkBatchFIFO(t, entries)
			if err != nil {
				return nil, err
			}
		}

		msg.topicARN, msg.time = t.ARN, now
		msgs[i] = msg
	}

	if total > maxPublishSize {
		return nil, api.NewBatchRequestTooLongException(
			"The length of all the messages put together is more than the limit.")
	}

	return msgs, nil
}

// batchMessage checks one entry's attributes and MessageStructure, as
// LocalStack does for each entry, and returns its message. position is the
// entry's 1-based place in the batch.
func batchMessage(e *api.PublishBatchRequestEntry, position int) (*message, error) {
	err := checkAttributes(e.MessageAttributes, position)
	if err != nil {
		return nil, err
	}

	msg := &message{
		id: newUUID(), text: deref(e.Message), subject: deref(e.Subject), structure: deref(e.MessageStructure),
		attrs: e.MessageAttributes, groupID: deref(e.MessageGroupId), dedupID: deref(e.MessageDeduplicationId),
	}

	if msg.structure == structureJSON {
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
