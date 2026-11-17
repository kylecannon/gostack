package sqs

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	policyVersion = "2008-10-17"
	purgeInterval = 60 * time.Second
)

// AddPermission adds a statement to the queue's Policy attribute, which it
// creates when the queue has none.
func (p *Provider) AddPermission(
	ctx context.Context, in *sqsapi.AddPermissionInput,
) (*sqsapi.AddPermissionOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	if in.Label == nil {
		return nil, missingParameter("Label")
	}

	if len(in.AWSAccountIds) == 0 {
		return nil, missingParameter("AWSAccountIds")
	}

	if len(in.Actions) == 0 {
		return nil, missingParameter("Actions")
	}

	err = validateActions(in.Actions)
	if err != nil {
		return nil, err
	}

	err = q.addPermission(*in.Label, in.Actions, in.AWSAccountIds)
	if err != nil {
		return nil, err
	}

	return &sqsapi.AddPermissionOutput{}, nil
}

// RemovePermission removes the statement with the given label from the queue's
// Policy attribute.
func (p *Provider) RemovePermission(
	ctx context.Context, in *sqsapi.RemovePermissionInput,
) (*sqsapi.RemovePermissionOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	if in.Label == nil {
		return nil, missingParameter("Label")
	}

	err = q.removePermission(*in.Label)
	if err != nil {
		return nil, err
	}

	return &sqsapi.RemovePermissionOutput{}, nil
}

// PurgeQueue drops every message of a queue. With SQS_DELAY_PURGE_RETRY a queue
// purged less than a minute ago refuses.
func (p *Provider) PurgeQueue(ctx context.Context, in *sqsapi.PurgeQueueInput) (*sqsapi.PurgeQueueOutput, error) {
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

	now := time.Now()
	if p.settings().DelayPurgeRetry && !q.purgedAt.IsZero() && q.purgedAt.Add(purgeInterval).After(now) {
		return nil, purgeInProgress(q.name)
	}

	q.purgedAt = now
	q.contents.Clear()

	return &sqsapi.PurgeQueueOutput{}, nil
}

// validateActions refuses an action that is not an SQS operation or "*".
// LocalStack's check is as loose: ListQueues passes too.
func validateActions(actions []string) *awserr.Error {
	for _, action := range actions {
		if action != "*" && !slices.Contains(sqsapi.Operations, action) {
			return invalidParameterValue("Value SQS:" + action + " for parameter ActionName is invalid. " +
				"Reason: Please refer to the appropriate WSDL for a list of valid actions. ")
		}
	}

	return nil
}

// policy is a Policy attribute as a document. Keys this code does not know are
// kept as they are.
type policy = map[string]any

// loadPolicy reads the queue's Policy attribute, or starts the default policy
// of the queue when it has none. The caller holds the queue's lock.
func (q *Queue) loadPolicy() (policy, *awserr.Error) {
	doc := policy{}

	if raw := q.attrs[attrPolicy]; raw != "" {
		err := json.Unmarshal([]byte(raw), &doc)
		if err != nil {
			return nil, awserr.Internal("the Policy attribute of the queue is not a JSON object")
		}
	} else {
		doc["Version"] = policyVersion
		doc["Id"] = q.arn + "/SQSDefaultPolicy"
	}

	if _, ok := doc["Statement"].([]any); !ok {
		doc["Statement"] = []any{}
	}

	return doc, nil
}

func (q *Queue) addPermission(label string, actions, accountIDs []string) *awserr.Error {
	q.mu.Lock()
	defer q.mu.Unlock()

	doc, err := q.loadPolicy()
	if err != nil {
		return err
	}

	statements, _ := doc["Statement"].([]any)
	if slices.ContainsFunc(statements, func(s any) bool { return statementID(s) == label }) {
		return invalidParameterValue("Value " + label + " for parameter Label is invalid. Reason: Already exists.")
	}

	principals := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		principals = append(principals, "arn:"+partition(q.region)+":iam::"+id+":root")
	}

	qualified := make([]string, 0, len(actions))
	for _, action := range actions {
		qualified = append(qualified, "SQS:"+action)
	}

	doc["Statement"] = append(statements, map[string]any{
		"Sid":       label,
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": oneOrMany(principals)},
		"Action":    oneOrMany(qualified),
		"Resource":  q.arn,
	})

	return q.storePolicy(doc)
}

func (q *Queue) removePermission(label string) *awserr.Error {
	q.mu.Lock()
	defer q.mu.Unlock()

	doc, err := q.loadPolicy()
	if err != nil {
		return err
	}

	statements, _ := doc["Statement"].([]any)
	if !slices.ContainsFunc(statements, func(s any) bool { return statementID(s) == label }) {
		return invalidParameterValue("Value " + label + " for parameter Label is invalid. Reason: can't find label.")
	}

	statements = slices.DeleteFunc(slices.Clone(statements), func(s any) bool { return statementID(s) == label })
	if len(statements) == 0 {
		delete(q.attrs, attrPolicy)

		return nil
	}

	doc["Statement"] = statements

	return q.storePolicy(doc)
}

func (q *Queue) storePolicy(doc policy) *awserr.Error {
	encoded, err := json.Marshal(doc)
	if err != nil {
		return awserr.Internal("the Policy attribute cannot be written")
	}

	q.attrs[attrPolicy] = string(encoded)

	return nil
}

// statementID is the Sid of a policy statement, or "" when it has none.
func statementID(statement any) string {
	fields, _ := statement.(map[string]any)
	id, _ := fields["Sid"].(string)

	return id
}

// oneOrMany is a single value for a list of one and the list otherwise, which
// is how AWS writes principals and actions.
func oneOrMany(values []string) any {
	if len(values) == 1 {
		return values[0]
	}

	return values
}
