package sqs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
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
// SetQueueAttributes refuses those, and CreateQueue does not look at the policy.
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

// maxAllowedSources is how many source queue ARNs a byQueue RedriveAllowPolicy
// can name.
const maxAllowedSources = 10

// validateRedriveAllowPolicy checks the shape of a RedriveAllowPolicy: a JSON
// object whose redrivePermission is allowAll, denyAll or byQueue, and which for
// byQueue lists up to ten sourceQueueArns. An empty policy is none. LocalStack
// stores the attribute without a look, so this follows AWS's documentation and
// no recorded response. Nothing enforces the policy, as in LocalStack.
func validateRedriveAllowPolicy(raw string) *awserr.Error {
	if raw == "" {
		return nil
	}

	var doc struct {
		Permission string   `json:"redrivePermission"`
		Sources    []string `json:"sourceQueueArns"`
	}

	if json.Unmarshal([]byte(raw), &doc) != nil {
		return sqsapi.NewInvalidAttributeValue("Invalid value for the parameter RedriveAllowPolicy.")
	}

	const prefix = "Invalid value for the parameter RedriveAllowPolicy. Reason: "

	switch doc.Permission {
	case "allowAll", "denyAll":
		return nil
	case "byQueue":
		if len(doc.Sources) == 0 || len(doc.Sources) > maxAllowedSources {
			return sqsapi.NewInvalidAttributeValue(prefix + "sourceQueueArns must list from 1 to 10 queue ARNs.")
		}

		return nil
	default:
		return sqsapi.NewInvalidAttributeValue(prefix + "redrivePermission must be allowAll, denyAll or byQueue.")
	}
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
// source queue: no call ever holds the lock of two queues. A dead-letter queue
// that is gone fails the call, and the messages with it.
func (p *Provider) deadLetter(rc *request.Context, source *Queue, dead []deadLetter) *awserr.Error {
	now := time.Now()

	for _, d := range dead {
		target, ok := p.queueByARN(d.target)
		if !ok {
			return queueDoesNotExist(rc)
		}

		err := target.put(d.msg.moved(source.arn, d.received, now), nil)
		if err != nil {
			return err
		}
	}

	return nil
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
