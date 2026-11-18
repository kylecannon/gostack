package sqs

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
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

	q.mu.Lock()
	defer q.mu.Unlock()

	err = validateThroughputPairing(attrs, q.attrs)
	if err != nil {
		return err
	}

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
// whole number from 1 to 1000, read as Python's int() reads it ("5", " 5",
// "1_0", 5.9 and true are all numbers). Whether the target queue exists is not
// checked.
//
// Where LocalStack crashes with a 500 (text that is not json, a json value that
// is not an object, a count that is a list or an object) this answers a 400
// InvalidParameterValue, and a policy it refuses is never stored; LocalStack
// stores it and then fails.
func validateRedrivePolicy(policy string) *awserr.Error {
	decoder := json.NewDecoder(bytes.NewReader([]byte(policy)))
	decoder.UseNumber()

	var parsed any

	err := decoder.Decode(&parsed)

	doc, isObject := parsed.(map[string]any)
	if err != nil || !isObject {
		return invalidParameterValue("Invalid value for the parameter RedrivePolicy.")
	}

	if !pyTruthy(doc["deadLetterTargetArn"]) {
		return invalidParameterValue("The required parameter 'deadLetterTargetArn' is missing")
	}

	raw := doc["maxReceiveCount"]
	if raw == nil {
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

// pyTruthy is Python's truth value of a decoded json value, which is what
// LocalStack's "if not dl_target_arn" tests: null, false, zero, "", [] and {}
// are false.
func pyTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		f, err := typed.Float64()

		return err != nil || f != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// receiveCount reads maxReceiveCount the way Python's int() does: a number is
// truncated, true is 1, and a string has to hold an integer. When it cannot be
// read, shown is the value as LocalStack prints it in its error.
func receiveCount(raw any) (count int, shown string) {
	switch value := raw.(type) {
	case json.Number:
		whole, ok := protocol.ParsePyInt(value.String())
		if ok {
			return clampCount(whole), ""
		}

		fraction, err := value.Float64()
		if err != nil || math.IsInf(fraction, 0) {
			return 0, value.String()
		}

		return clampCount(int64(max(math.MinInt64, min(math.MaxInt64, math.Trunc(fraction))))), ""
	case bool:
		if value {
			return 1, ""
		}

		return 0, ""
	case string:
		whole, ok := protocol.ParsePyInt(value)
		if !ok {
			return 0, value
		}

		return clampCount(whole), ""
	default:
		return 0, "invalid"
	}
}

// clampCount keeps a count a range check can still refuse on every platform.
func clampCount(n int64) int {
	return int(max(math.MinInt32, min(math.MaxInt32, n)))
}
