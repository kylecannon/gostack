package sns

import (
	"context"
	"encoding/base64"
	"maps"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	// fifoSuffix ends the name of a FIFO topic.
	fifoSuffix = ".fifo"
	// maxTopicName is the most characters of a topic name, before fifoSuffix.
	maxTopicName = 256
	// topicsPageSize is the most topics a ListTopics page holds.
	topicsPageSize = 100

	attrFifoTopic = "FifoTopic"
	valueTrue     = "true"
	valueFalse    = "false"
)

// CreateTopic creates a topic, or answers with the topic of that name when
// there is one and the call asks for nothing it lacks, as LocalStack's
// create_topic.
func (p *Provider) CreateTopic(ctx context.Context, in *api.CreateTopicInput) (*api.CreateTopicOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.Name)
	attrs := maps.Clone(in.Attributes)

	fifo := strings.ToLower(attrs[attrFifoTopic]) == valueTrue
	if !fifo {
		// AWS does not keep FifoTopic=false, so neither does a repeated call
		// compare it.
		delete(attrs, attrFifoTopic)
	}

	valid := validTopicName(name)
	if fifo {
		base, suffixed := strings.CutSuffix(name, fifoSuffix)
		valid = suffixed && validTopicName(base)
	}

	if !valid {
		return nil, api.NewInvalidParameterException("Invalid parameter: Topic Name")
	}

	arn := topicARN(rc.AccountID, rc.Region, name)
	tags := tagsOf(in.Tags)
	account := p.accounts.Get(rc.AccountID, accountRegion)
	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.Lock()
	defer st.mu.Unlock()

	if existing, ok := st.topics[arn]; ok {
		serr := sameTopic(existing, attrs, tags, account)
		if serr != nil {
			return nil, serr
		}

		return &api.CreateTopicOutput{TopicArn: new(arn)}, nil
	}

	st.add(newTopic(rc, name, arn, attrs, in.DataProtectionPolicy))

	if len(tags) > 0 {
		account.update(arn, tags)
	}

	return &api.CreateTopicOutput{TopicArn: new(arn)}, nil
}

// DeleteTopic deletes a topic, its subscriptions and its tags. A topic that does not exist is no
// error. Like LocalStack's delete_topic, it acts in the caller's account
// whatever account the ARN names.
func (p *Provider) DeleteTopic(ctx context.Context, in *api.DeleteTopicInput) (*api.DeleteTopicOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	arn := deref(in.TopicArn)

	parts, perr := parseTopicARN(arn)
	if perr != nil {
		return nil, perr
	}

	if parts.region != rc.Region {
		return nil, api.NewInvalidParameterException("Invalid parameter: TopicArn")
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.Lock()
	defer st.mu.Unlock()

	p.accounts.Get(rc.AccountID, accountRegion).drop(arn)
	st.remove(arn)

	return &api.DeleteTopicOutput{}, nil
}

// ListTopics lists the caller's topics in the region in creation order, a page
// of 100 at a time, as LocalStack's PaginatedList pages them: the NextToken is
// the base64 of the ARN of the next page's first topic, and a token that names
// no topic starts from the first.
func (p *Provider) ListTopics(ctx context.Context, in *api.ListTopicsInput) (*api.ListTopicsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	defer st.mu.RUnlock()

	start := 0

	if in.NextToken != nil {
		// Strict decoding takes only the canonical encoding, so the token names
		// a topic exactly when it is the base64 of that topic's ARN.
		arn, derr := base64.StdEncoding.Strict().DecodeString(*in.NextToken)
		if i, ok := st.position(string(arn)); derr == nil && ok {
			start = i
		}
	}

	end := min(start+topicsPageSize, len(st.order))
	out := &api.ListTopicsOutput{Topics: make([]api.Topic, 0, end-start)}

	for _, t := range st.order[start:end] {
		out.Topics = append(out.Topics, api.Topic{TopicArn: new(t.ARN)})
	}

	if end < len(st.order) {
		out.NextToken = new(base64.StdEncoding.EncodeToString([]byte(st.order[end].ARN)))
	}

	return out, nil
}

// GetTopicAttributes returns a topic's attributes.
func (p *Provider) GetTopicAttributes(
	ctx context.Context, in *api.GetTopicAttributesInput,
) (*api.GetTopicAttributesOutput, error) {
	t, err := p.findTopic(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	return &api.GetTopicAttributesOutput{Attributes: maps.Clone(t.Attributes)}, nil
}

// SetTopicAttributes sets one topic attribute. Like LocalStack, it takes any
// name but FifoTopic, which is fixed when the topic is made.
func (p *Provider) SetTopicAttributes(
	ctx context.Context, in *api.SetTopicAttributesInput,
) (*api.SetTopicAttributesOutput, error) {
	t, err := p.findTopic(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	if in.AttributeName == nil || *in.AttributeName == attrFifoTopic {
		return nil, api.NewInvalidParameterException("Invalid parameter: AttributeName")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.Attributes[*in.AttributeName] = deref(in.AttributeValue)

	return &api.SetTopicAttributesOutput{}, nil
}

// GetDataProtectionPolicy returns a topic's data protection policy, none when
// it was never set.
func (p *Provider) GetDataProtectionPolicy(
	ctx context.Context, in *api.GetDataProtectionPolicyInput,
) (*api.GetDataProtectionPolicyOutput, error) {
	t, err := p.findTopic(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	out := &api.GetDataProtectionPolicyOutput{}
	if t.DataProtectionPolicy != nil {
		out.DataProtectionPolicy = new(*t.DataProtectionPolicy)
	}

	return out, nil
}

// PutDataProtectionPolicy sets a topic's data protection policy. LocalStack
// keeps it as sent and does not check it.
func (p *Provider) PutDataProtectionPolicy(
	ctx context.Context, in *api.PutDataProtectionPolicyInput,
) (*api.PutDataProtectionPolicyOutput, error) {
	t, err := p.findTopic(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.DataProtectionPolicy = nil
	if in.DataProtectionPolicy != nil {
		t.DataProtectionPolicy = new(*in.DataProtectionPolicy)
	}

	return &api.PutDataProtectionPolicyOutput{}, nil
}

// findTopic is LocalStack's _get_topic: the topic an ARN names, in the ARN's
// account and region. The region has to be the caller's.
func (p *Provider) findTopic(ctx context.Context, topicArn *string) (*Topic, error) {
	_, t, err := p.topicIn(ctx, topicArn)

	return t, err
}

// topicIn is findTopic with the State that holds the topic, and its
// subscriptions.
func (p *Provider) topicIn(ctx context.Context, topicArn *string) (*State, *Topic, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, nil, err
	}

	arn := deref(topicArn)

	parts, perr := parseTopicARN(arn)
	if perr != nil {
		return nil, nil, perr
	}

	if parts.region != rc.Region {
		return nil, nil, api.NewInvalidParameterException("Invalid parameter: TopicArn")
	}

	st := p.stateAt(parts.account, parts.region)

	t, ok := st.topic(arn)
	if !ok {
		return nil, nil, api.NewNotFoundException("Topic does not exist")
	}

	return st, t, nil
}

// sameTopic checks a repeated CreateTopic against the topic it names, as
// LocalStack does: every attribute asked for has to be set on the topic, to a
// value that is not empty and is the same, and every tag asked for has to be
// on it.
func sameTopic(existing *Topic, attrs map[string]string, tags []tag, account *AccountState) error {
	existing.mu.Lock()

	for k, v := range attrs {
		if have := existing.Attributes[k]; have == "" || have != v {
			existing.mu.Unlock()

			return api.NewInvalidParameterException(
				"Invalid parameter: Attributes Reason: Topic already exists with different attributes")
		}
	}

	existing.mu.Unlock()

	if !account.covers(existing.ARN, tags) {
		return api.NewInvalidParameterException("Invalid parameter: Tags Reason: Topic already exists with different tags")
	}

	return nil
}

// newTopic is LocalStack's _create_topic: the default attributes, with the
// ones asked for over them, and the default effective delivery policy over
// those.
func newTopic(rc *request.Context, name, arn string, attrs map[string]string, dataProtection *string) *Topic {
	attributes := map[string]string{
		"DisplayName":            "",
		"Owner":                  rc.AccountID,
		"Policy":                 defaultTopicPolicy(arn, rc.AccountID),
		"SubscriptionsConfirmed": "0",
		"SubscriptionsDeleted":   "0",
		"SubscriptionsPending":   "0",
		"TopicArn":               arn,
	}

	if strings.HasSuffix(name, fifoSuffix) {
		attributes["ContentBasedDeduplication"] = valueFalse
		attributes[attrFifoTopic] = valueFalse
	}

	maps.Copy(attributes, attrs)
	attributes["EffectiveDeliveryPolicy"] = effectiveDeliveryPolicy

	t := &Topic{Name: name, ARN: arn, Attributes: attributes}
	if dataProtection != nil {
		t.DataProtectionPolicy = new(*dataProtection)
	}

	return t
}

// validTopicName reports whether name is a topic name less the ".fifo" of a
// FIFO topic: 1 to 256 letters, digits, hyphens and underscores.
func validTopicName(name string) bool {
	if name == "" || len(name) > maxTopicName {
		return false
	}

	for i := range len(name) {
		if c := name[i]; !isNameByte(c) {
			return false
		}
	}

	return true
}

func isNameByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}
