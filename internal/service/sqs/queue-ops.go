package sqs

import (
	"context"
	"encoding/base64"
	"maps"
	"slices"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

// defaultPageSize is how many queue URLs ListQueues returns when the caller
// sets no MaxResults.
const defaultPageSize = 1000

// CreateQueue creates a queue, or returns the URL of the one that exists when
// the attributes agree with it.
func (p *Provider) CreateQueue(ctx context.Context, in *sqsapi.CreateQueueInput) (*sqsapi.CreateQueueOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.QueueName == nil {
		return nil, missingParameter("QueueName")
	}

	err = checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	attrs := maps.Clone(in.Attributes)
	if value, ok := attrs[attrPolicy]; ok && value == "" {
		// An empty policy is no policy.
		delete(attrs, attrPolicy)
	}

	q, err := p.addQueue(rc, *in.QueueName, attrs, in.Tags)
	if err != nil {
		return nil, err
	}

	settings := p.settings()

	return &sqsapi.CreateQueueOutput{QueueUrl: new(queueURL(&settings, rc.Request, q))}, nil
}

// addQueue adds the queue to the state of the caller's account and region,
// holding the state's lock so that two creates of one name cannot both win.
func (p *Provider) addQueue(rc *request.Context, name string, attrs, tags map[string]string) (*Queue, *awserr.Error) {
	state := p.states.Get(rc.AccountID, rc.Region)
	now := time.Now()

	state.mu.Lock()
	defer state.mu.Unlock()

	if existing, ok := state.queues[name]; ok {
		// Creating a queue that exists is fine when the attributes agree.
		return existing, existing.agreesWith(attrs)
	}

	if state.recentlyDeleted(name, now) && p.settings().DelayRecentlyDeleted {
		return nil, sqsapi.NewQueueDeletedRecently(
			"You must wait 60 seconds after deleting a queue before you can create another with the same name.")
	}

	state.expireDeleted(now)

	q, err := newQueue(name, rc.AccountID, rc.Region, attrs, tags, state.nextSeq(), now, p.contents)
	if err != nil {
		return nil, err
	}

	state.add(q)

	return q, nil
}

// agreesWith reports, as an error, the first attribute of a CreateQueue call
// that the queue has with another value.
func (q *Queue) agreesWith(attrs map[string]string) *awserr.Error {
	if len(attrs) == 0 {
		return nil
	}

	err := validateAttributes(q.kind, attrs)
	if err != nil {
		return err
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		if current, ok := q.attrs[name]; !ok || current != attrs[name] {
			return sqsapi.NewQueueNameExists(
				"A queue already exists with the same name and a different value for attribute " + name)
		}
	}

	return nil
}

// DeleteQueue deletes the queue the URL names and remembers its name, so that
// SQS_DELAY_RECENTLY_DELETED can hold the name back.
func (p *Provider) DeleteQueue(ctx context.Context, in *sqsapi.DeleteQueueInput) (*sqsapi.DeleteQueueOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	loc := locate(rc, in.QueueUrl)
	state := p.states.Get(loc.account, loc.region)

	state.mu.Lock()
	defer state.mu.Unlock()

	q, ok := state.queues[loc.name]
	if !ok {
		return nil, queueDoesNotExist(rc)
	}

	q.close()
	delete(state.queues, loc.name)
	state.deleted[loc.name] = time.Now()

	return &sqsapi.DeleteQueueOutput{}, nil
}

// GetQueueUrl returns the URL of a queue by its name, in the caller's account
// or in QueueOwnerAWSAccountId.
//
//nolint:revive // var-naming: the name is the operation's, fixed by the generated Handler interface.
func (p *Provider) GetQueueUrl(ctx context.Context, in *sqsapi.GetQueueUrlInput) (*sqsapi.GetQueueUrlOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.QueueName == nil {
		return nil, missingParameter("QueueName")
	}

	owner := rc.AccountID
	if in.QueueOwnerAWSAccountId != nil && *in.QueueOwnerAWSAccountId != "" {
		owner = *in.QueueOwnerAWSAccountId
	}

	q, found := p.states.Get(owner, rc.Region).find(*in.QueueName)
	if !found {
		return nil, queueDoesNotExist(rc)
	}

	settings := p.settings()

	return &sqsapi.GetQueueUrlOutput{QueueUrl: new(queueURL(&settings, rc.Request, q))}, nil
}

// ListQueues returns the URLs of the caller's queues in creation order, a page
// at a time. NextToken is the base64 of the URL the next page starts with.
func (p *Provider) ListQueues(ctx context.Context, in *sqsapi.ListQueuesInput) (*sqsapi.ListQueuesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	prefix := ""
	if in.QueueNamePrefix != nil {
		prefix = *in.QueueNamePrefix
	}

	settings := p.settings()
	queues := p.states.Get(rc.AccountID, rc.Region).list(prefix)

	urls := make([]string, 0, len(queues))
	for _, q := range queues {
		urls = append(urls, queueURL(&settings, rc.Request, q))
	}

	if len(urls) == 0 {
		return &sqsapi.ListQueuesOutput{}, nil
	}

	pageSize := defaultPageSize
	if in.MaxResults != nil && *in.MaxResults > 0 {
		pageSize = int(*in.MaxResults)
	}

	token := ""
	if in.NextToken != nil {
		token = *in.NextToken
	}

	page, next := paginate(urls, token, pageSize)

	return &sqsapi.ListQueuesOutput{QueueUrls: page, NextToken: next}, nil
}

// paginate cuts a page of pageSize items out of items. The page starts at the
// item whose token is token, or at the first when token names none. next is the
// token of the item after the page, or nil on the last page.
func paginate(items []string, token string, pageSize int) ([]string, *string) {
	if len(items) <= pageSize && token == "" {
		return items, nil
	}

	start := slices.IndexFunc(items, func(item string) bool { return pageToken(item) == token })
	start = max(start, 0)

	end := min(start+pageSize, len(items))
	if end >= len(items) {
		return items[start:end], nil
	}

	return items[start:end], new(pageToken(items[end]))
}

func pageToken(item string) string {
	return base64.StdEncoding.EncodeToString([]byte(item))
}

// close shuts the queue's contents down. The queue is not used afterwards.
func (q *Queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.contents.Close()
}
