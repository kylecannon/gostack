package sqs

import (
	"context"
	"maps"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
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
