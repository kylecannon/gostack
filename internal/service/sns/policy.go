package sns

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
)

const (
	attrPolicy = "Policy"
	// effectiveDeliveryPolicy is the EffectiveDeliveryPolicy attribute of every
	// new topic, LocalStack's _create_default_effective_delivery_policy.
	effectiveDeliveryPolicy = `{"http":{"defaultHealthyRetryPolicy":{"minDelayTarget":20,"maxDelayTarget":20,` +
		`"numRetries":3,"numMaxDelayRetries":0,"numNoDelayRetries":0,"numMinDelayRetries":0,` +
		`"backoffFunction":"linear"},"disableSubscriptionOverrides":false,` +
		`"defaultRequestPolicy":{"headerContentType":"text/plain; charset=UTF-8"}}}`
)

// policyActions are the actions AddPermission grants, LocalStack's
// VALID_POLICY_ACTIONS.
var policyActions = []string{
	"GetTopicAttributes", "SetTopicAttributes", "AddPermission", "RemovePermission", "DeleteTopic",
	"Subscribe", "ListSubscriptionsByTopic", "Publish", "Receive",
}

// AddPermission adds a statement to a topic's policy that lets the accounts
// call the actions, as LocalStack's add_permission. The label is the
// statement's Sid and must be new.
func (p *Provider) AddPermission(ctx context.Context, in *api.AddPermissionInput) (*api.AddPermissionOutput, error) {
	t, err := p.findTopic(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	rc, cerr := callOf(ctx)
	if cerr != nil {
		return nil, cerr
	}

	label := deref(in.Label)

	t.mu.Lock()
	defer t.mu.Unlock()

	doc, statements, perr := decodePolicy(t.Attributes[attrPolicy])
	if perr != nil {
		return nil, perr
	}

	if slices.ContainsFunc(statements, func(s any) bool { return sidOf(s) == label }) {
		return nil, api.NewInvalidParameterException("Invalid parameter: Statement already exists")
	}

	for _, action := range in.ActionName {
		if !slices.Contains(policyActions, action) {
			return nil, api.NewInvalidParameterException("Invalid parameter: Policy statement action out of service scope!")
		}
	}

	statement, serr := grant(rc.Region, t.ARN, label, in.AWSAccountId, in.ActionName)
	if serr != nil {
		return nil, serr
	}

	doc["Statement"] = append(statements, statement)

	err = setPolicy(t, doc)
	if err != nil {
		return nil, err
	}

	return &api.AddPermissionOutput{}, nil
}

// RemovePermission removes the statements whose Sid is the label from a
// topic's policy. A label no statement has is no error.
func (p *Provider) RemovePermission(
	ctx context.Context, in *api.RemovePermissionInput,
) (*api.RemovePermissionOutput, error) {
	t, err := p.findTopic(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	label := deref(in.Label)

	t.mu.Lock()
	defer t.mu.Unlock()

	doc, statements, perr := decodePolicy(t.Attributes[attrPolicy])
	if perr != nil {
		return nil, perr
	}

	doc["Statement"] = slices.DeleteFunc(statements, func(s any) bool { return sidOf(s) == label })

	err = setPolicy(t, doc)
	if err != nil {
		return nil, err
	}

	return &api.RemovePermissionOutput{}, nil
}

// grant is the statement AddPermission adds: the accounts' root principals may
// call the actions on the topic arn.
func grant(region, arn, label string, accounts, actionNames []string) (map[string]any, error) {
	// LocalStack fails on an empty list here; AWS requires both lists.
	if len(accounts) == 0 {
		return nil, api.NewInvalidParameterException("Invalid parameter: AWSAccountId")
	}

	if len(actionNames) == 0 {
		return nil, api.NewInvalidParameterException("Invalid parameter: ActionName")
	}

	principals := make([]string, len(accounts))
	for i, account := range accounts {
		principals[i] = "arn:" + partition(region) + ":iam::" + account + ":root"
	}

	actions := make([]string, len(actionNames))
	for i, action := range actionNames {
		actions[i] = "SNS:" + action
	}

	return map[string]any{
		"Sid":       label,
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": oneOrMany(principals)},
		"Action":    oneOrMany(actions),
		"Resource":  arn,
	}, nil
}

// decodePolicy reads a topic policy and its statements. A policy that is not a
// JSON object with a list of statements, which SetTopicAttributes lets through
// as LocalStack's does, is an error where LocalStack fails.
func decodePolicy(policy string) (map[string]any, []any, error) {
	dec := json.NewDecoder(strings.NewReader(policy))
	dec.UseNumber()

	var doc map[string]any

	err := dec.Decode(&doc)
	if err != nil || doc == nil {
		return nil, nil, api.NewInvalidParameterException("Invalid parameter: Policy")
	}

	statements, ok := doc["Statement"].([]any)
	if !ok {
		return nil, nil, api.NewInvalidParameterException("Invalid parameter: Policy")
	}

	return doc, statements, nil
}

// setPolicy writes doc as the topic's policy. The caller holds the topic's
// lock.
func setPolicy(t *Topic, doc map[string]any) error {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	err := enc.Encode(doc)
	if err != nil {
		return api.NewInvalidParameterException("Invalid parameter: Policy")
	}

	t.Attributes[attrPolicy] = strings.TrimSuffix(buf.String(), "\n")

	return nil
}

// sidOf returns a statement's Sid, or "" when it has none.
func sidOf(statement any) string {
	m, ok := statement.(map[string]any)
	if !ok {
		return ""
	}

	sid, _ := m["Sid"].(string)

	return sid
}

// oneOrMany is a policy value: the one string of values alone, or the list.
func oneOrMany(values []string) any {
	if len(values) == 1 {
		return values[0]
	}

	return values
}

// defaultTopicPolicy is the Policy attribute of a new topic, LocalStack's
// create_default_topic_policy.
func defaultTopicPolicy(arn, account string) string {
	var b strings.Builder

	b.Grow(len(defaultPolicyHead) + len(defaultPolicyMiddle) + len(defaultPolicyTail) + 2*len(arn) + len(account))
	b.WriteString(defaultPolicyHead)
	writeJSONString(&b, arn)
	b.WriteString(defaultPolicyMiddle)
	writeJSONString(&b, account)
	b.WriteString(defaultPolicyTail)

	return b.String()
}

const (
	defaultPolicyHead = `{"Version":"2008-10-17","Id":"__default_policy_ID","Statement":[{"Effect":"Allow",` +
		`"Sid":"__default_statement_ID","Principal":{"AWS":"*"},"Action":["SNS:GetTopicAttributes",` +
		`"SNS:SetTopicAttributes","SNS:AddPermission","SNS:RemovePermission","SNS:DeleteTopic",` +
		`"SNS:Subscribe","SNS:ListSubscriptionsByTopic","SNS:Publish"],"Resource":`
	defaultPolicyMiddle = `,"Condition":{"StringEquals":{"AWS:SourceOwner":`
	defaultPolicyTail   = `}}}]}`
)

// writeJSONString writes s as a JSON string. The ARN and account of a topic
// are plain text almost always; the escapes are for the rest.
func writeJSONString(b *strings.Builder, s string) {
	const (
		hex = "0123456789abcdef"
		// space is the first character JSON does not escape.
		space = ' '
	)

	b.WriteByte('"')

	for i := range len(s) {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < space:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}

	b.WriteByte('"')
}
