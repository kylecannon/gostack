package sqs

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// GetQueueAttributes returns the attributes the caller names, or all of them
// for "All". Asking for none returns none.
func (p *Provider) GetQueueAttributes(
	ctx context.Context, in *sqsapi.GetQueueAttributesInput,
) (*sqsapi.GetQueueAttributesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	attrs, err := q.attributes(in.AttributeNames)
	if err != nil {
		return nil, err
	}

	return &sqsapi.GetQueueAttributesOutput{Attributes: attrs}, nil
}

// SetQueueAttributes changes attributes of a queue. The ones that LocalStack
// computes or fixes at creation are refused.
func (p *Provider) SetQueueAttributes(
	ctx context.Context, in *sqsapi.SetQueueAttributesInput,
) (*sqsapi.SetQueueAttributesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	q, err := p.resolve(rc, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	err = q.setAttributes(in.Attributes)
	if err != nil {
		return nil, err
	}

	return &sqsapi.SetQueueAttributesOutput{}, nil
}

// attributes returns the named attributes, reading the three Approximate ones
// from the queue's contents. An attribute the queue does not have is left out.
func (q *Queue) attributes(names []string) (map[string]string, *awserr.Error) {
	if len(names) == 0 {
		return nil, nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if slices.Contains(names, attrAll) {
		names = append(slices.Collect(maps.Keys(q.attrs)), dynamicAttributes...)
	}

	counts := q.contents.Counts()
	result := make(map[string]string, len(names))

	for _, name := range names {
		var value string

		found := true

		switch name {
		case attrApproximateNumberOfMessages:
			value = strconv.Itoa(counts.Visible)
		case attrApproximateNumberOfMessagesDelayed:
			value = strconv.Itoa(counts.Delayed)
		case attrApproximateNumberOfMessagesNotVisible:
			value = strconv.Itoa(counts.NotVisible)
		default:
			if !slices.Contains(knownAttributes, name) {
				return nil, sqsapi.NewInvalidAttributeName("Unknown Attribute " + name + ".")
			}

			value, found = q.attrs[name]
		}

		if found {
			result[name] = lowerBool(value)
		}
	}

	if len(result) == 0 {
		return nil, nil
	}

	return result, nil
}

// lowerBool lowercases the two spellings of a Python boolean, which is what
// LocalStack does to every attribute it returns.
func lowerBool(value string) string {
	if value == "True" || value == "False" {
		return strings.ToLower(value)
	}

	return value
}

// setAttributes validates attrs and applies them, or applies none of them.
func (q *Queue) setAttributes(attrs map[string]string) *awserr.Error {
	if len(attrs) == 0 {
		return nil
	}

	err := validateAttributes(q.kind, attrs)
	if err != nil {
		return err
	}

	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		if slices.Contains(internalAttributes, name) {
			return sqsapi.NewInvalidAttributeName("Unknown Attribute " + name + ".")
		}
	}

	if policy := attrs[attrRedrivePolicy]; policy != "" {
		err = validateRedrivePolicy(policy)
		if err != nil {
			return err
		}
	}

	err = validateRedriveAllowPolicy(attrs[attrRedriveAllowPolicy])
	if err != nil {
		return err
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for name, value := range attrs {
		if def, ok := deleteIfDefault[name]; ok && value == def {
			delete(q.attrs, name)

			continue
		}

		q.attrs[name] = value
	}

	// An empty policy is no policy.
	for _, name := range []string{attrPolicy, attrRedrivePolicy, attrRedriveAllowPolicy} {
		if q.attrs[name] == "" {
			delete(q.attrs, name)
		}
	}

	return nil
}

// validateRedrivePolicy checks a RedrivePolicy the way LocalStack's
// SetQueueAttributes does: the target ARN is there, and maxReceiveCount is a
// whole number from 1 to 1000. Whether the target queue exists is not checked.
func validateRedrivePolicy(policy string) *awserr.Error {
	decoder := json.NewDecoder(bytes.NewReader([]byte(policy)))
	decoder.UseNumber()

	var doc map[string]any

	err := decoder.Decode(&doc)
	if err != nil {
		return invalidParameterValue("Invalid value for the parameter RedrivePolicy.")
	}

	if target, _ := doc["deadLetterTargetArn"].(string); target == "" {
		return invalidParameterValue("The required parameter 'deadLetterTargetArn' is missing")
	}

	raw, present := doc["maxReceiveCount"]
	if !present || raw == nil {
		return invalidParameterValue("The required parameter 'maxReceiveCount' is missing")
	}

	count, shown := receiveCount(raw)
	if shown != "" || count < 1 || count > maxReceiveCountLimit {
		if shown == "" {
			shown = strconv.Itoa(count)
		}

		return invalidParameterValue("Value " + policy + " for parameter RedrivePolicy is invalid. " +
			"Reason: Invalid value for maxReceiveCount: " + shown +
			", valid values are from 1 to 1000 both inclusive.")
	}

	return nil
}

// receiveCount reads maxReceiveCount the way Python's int() does: a number is
// truncated, and a string has to hold an integer. When it cannot be read,
// shown is the value as LocalStack prints it in its error.
func receiveCount(raw any) (int, string) {
	switch value := raw.(type) {
	case json.Number:
		whole, err := value.Int64()
		if err == nil {
			return int(whole), ""
		}

		fraction, err := value.Float64()
		if err != nil {
			return 0, value.String()
		}

		return int(fraction), ""
	case string:
		whole, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, value
		}

		return whole, ""
	default:
		return 0, "invalid"
	}
}
