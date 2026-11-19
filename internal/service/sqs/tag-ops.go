package sqs

import (
	"context"
	"maps"
	"unicode/utf8"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// TagQueue adds tags to a queue. Tagging with no tags is a no-op, and does not
// look for the queue.
func (p *Provider) TagQueue(ctx context.Context, in *sqsapi.TagQueueInput) (*sqsapi.TagQueueOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Tags) == 0 {
		return &sqsapi.TagQueueOutput{}, nil
	}

	err = checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.tags == nil {
		q.tags = make(map[string]string, len(in.Tags))
	}

	maps.Copy(q.tags, in.Tags)

	return &sqsapi.TagQueueOutput{}, nil
}

// checkTags refuses a tag whose key or value is not UTF-8. Two keys that differ
// only in invalid bytes would be one key in a snapshot, and fail every save.
// Neither protocol's reader lets such a tag through (a query "%FF" stays the
// text "%FF", as in LocalStack, which answers 200; a json body that is not UTF-8
// is refused first), so this guards callers that skip the readers. The error is
// the message-body one, and no wire request meets it.
func checkTags(tags map[string]string) *awserr.Error {
	for key, value := range tags {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return sqsapi.NewInvalidMessageContents(messageInvalidContents)
		}
	}

	return nil
}

// UntagQueue removes tags from a queue. A key the queue does not have is
// ignored.
func (p *Provider) UntagQueue(ctx context.Context, in *sqsapi.UntagQueueInput) (*sqsapi.UntagQueueOutput, error) {
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

	for _, key := range in.TagKeys {
		delete(q.tags, key)
	}

	return &sqsapi.UntagQueueOutput{}, nil
}

// ListQueueTags returns the tags of a queue, none at all when it has none.
func (p *Provider) ListQueueTags(
	ctx context.Context, in *sqsapi.ListQueueTagsInput,
) (*sqsapi.ListQueueTagsOutput, error) {
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

	if len(q.tags) == 0 {
		return &sqsapi.ListQueueTagsOutput{}, nil
	}

	return &sqsapi.ListQueueTagsOutput{Tags: maps.Clone(q.tags)}, nil
}
