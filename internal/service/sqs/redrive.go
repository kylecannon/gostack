package sqs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// redrivePolicy is what a queue's RedrivePolicy says: where a message goes after
// too many receives, and how many that is.
type redrivePolicy struct {
	// target is the deadLetterTargetArn.
	target string
	// maxReceives is the maxReceiveCount. Zero or less switches the move off, as
	// in LocalStack.
	maxReceives int
}

// parseRedrive reads a RedrivePolicy attribute. It reports false for an absent
// policy, one that is not a JSON object and one without a deadLetterTargetArn:
// CreateQueue and SetQueueAttributes refuse those.
func parseRedrive(raw string) (redrivePolicy, bool) {
	if raw == "" {
		return redrivePolicy{}, false
	}

	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.UseNumber()

	var doc map[string]any

	err := decoder.Decode(&doc)
	if err != nil {
		return redrivePolicy{}, false
	}

	target, _ := doc["deadLetterTargetArn"].(string)

	count, shown := receiveCount(doc["maxReceiveCount"])
	if shown != "" {
		count = 0
	}

	return redrivePolicy{target: target, maxReceives: count}, target != ""
}

// redriveCache remembers the parsed RedrivePolicy of a queue, so a receive does
// not parse it again. It follows the attribute: SetQueueAttributes changes the
// string and the next lookup parses the new one.
type redriveCache struct {
	raw    string
	policy redrivePolicy
	found  bool
}

// of returns the queue's policy. The caller holds the queue's lock.
func (r *redriveCache) of(q *Queue) (redrivePolicy, bool) {
	if raw := q.attrs[attrRedrivePolicy]; raw != r.raw {
		r.raw = raw
		r.policy, r.found = parseRedrive(raw)
	}

	return r.policy, r.found
}

// ListDeadLetterSourceQueues returns the URLs of the caller's queues whose
// RedrivePolicy names the queue as their dead-letter queue, in creation order.
func (p *Provider) ListDeadLetterSourceQueues(
	ctx context.Context, in *sqsapi.ListDeadLetterSourceQueuesInput,
) (*sqsapi.ListDeadLetterSourceQueuesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	target, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	settings := p.settings()

	var urls []string

	for _, q := range p.stateAt(rc.AccountID, rc.Region).list("") {
		if q.redrivesTo(target.arn) {
			urls = append(urls, queueURL(&settings, rc.Request, q))
		}
	}

	if len(urls) == 0 {
		return &sqsapi.ListDeadLetterSourceQueuesOutput{}, nil
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

	return &sqsapi.ListDeadLetterSourceQueuesOutput{QueueUrls: page, NextToken: next}, nil
}

// redrivesTo reports whether the queue's RedrivePolicy names the ARN.
func (q *Queue) redrivesTo(arn string) bool {
	q.mu.Lock()
	policy, found := parseRedrive(q.attrs[attrRedrivePolicy])
	q.mu.Unlock()

	return found && policy.target == arn
}

// deadLetter puts the messages a receive found past the limit on their
// dead-letter queue, as LocalStack's ReceiveMessage does after it released the
// source queue: no call ever holds the lock of two queues. A message is deleted
// from the source only after the put, so a dead-letter queue that is gone or
// refuses it loses nothing: those messages are returned, still hidden on the
// source, for the receive to hand out as ordinary ones. LocalStack fails the
// receive and drops them.
func (p *Provider) deadLetter(source *Queue, dead []deadLetter) (kept []delivery) {
	now := time.Now()
	moved := make([]deadLetter, 0, len(dead))

	var failure *awserr.Error

	for _, d := range dead {
		err := p.putDeadLetter(source, d, now)
		if err != nil {
			failure = err
			kept = append(kept, d.delivery)

			continue
		}

		moved = append(moved, d)
	}

	source.mu.Lock()
	defer source.mu.Unlock()

	for _, d := range moved {
		err := source.contents.Delete(d.delivery.handle)
		if err != nil {
			failure = err
		}
	}

	if failure != nil && source.deadLetterWarned.CompareAndSwap(false, true) {
		slog.Warn("sqs: a message could not leave its queue for the dead-letter queue and stays on it",
			"queue", source.arn, "error", failure.Message)
	}

	return kept
}

// putDeadLetter puts one dead letter on its dead-letter queue.
func (p *Provider) putDeadLetter(source *Queue, d deadLetter, now time.Time) *awserr.Error {
	target, ok := p.queueByARN(d.target)
	if !ok {
		return sqsapi.NewQueueDoesNotExist("The specified queue does not exist.")
	}

	return target.put(d.delivery.msg.moved(source.arn, d.received, now), nil)
}

// queueByARN finds a queue by its ARN, in whichever account and region the ARN
// names.
func (p *Provider) queueByARN(arn string) (*Queue, bool) {
	parts, ok := parseARN(arn)
	if !ok {
		return nil, false
	}

	return p.stateAt(parts.account, parts.region).find(parts.resource)
}

// arnParts are the fields of an ARN that find a queue.
type arnParts struct {
	region, account, resource string
}

// parseARN splits an ARN into its fields. It wants the six parts of an ARN, the
// last of which is the resource and may hold colons itself.
func parseARN(arn string) (arnParts, bool) {
	const parts = 6

	fields := strings.SplitN(arn, ":", parts)
	if len(fields) != parts || fields[0] != "arn" {
		return arnParts{}, false
	}

	return arnParts{region: fields[3], account: fields[4], resource: fields[5]}, true
}

// put puts a message on the queue under the queue's lock.
func (q *Queue) put(m *message, delay *int32) *awserr.Error {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.putLocked(m, delay)
}
