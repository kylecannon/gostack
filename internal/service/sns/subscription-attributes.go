package sns

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

// The subscription attributes a caller sets, LocalStack's
// VALID_SUBSCRIPTION_ATTR_NAME.
const (
	attrDeliveryPolicy      = "DeliveryPolicy"
	attrFilterPolicy        = "FilterPolicy"
	attrFilterPolicyScope   = "FilterPolicyScope"
	attrRawMessageDelivery  = "RawMessageDelivery"
	attrRedrivePolicy       = "RedrivePolicy"
	attrSubscriptionRoleARN = "SubscriptionRoleArn"
)

// subscriptionAttributeNames are the attributes a caller sets, in
// LocalStack's order.
var subscriptionAttributeNames = []string{
	attrDeliveryPolicy, attrFilterPolicy, attrFilterPolicyScope,
	attrRawMessageDelivery, attrRedrivePolicy, attrSubscriptionRoleARN,
}

// GetSubscriptionAttributes returns a subscription's attributes, as
// LocalStack's get_subscription_attributes: FilterPolicyScope only with a
// FilterPolicy that is not empty. The subscription is looked up in the
// caller's account and region.
func (p *Provider) GetSubscriptionAttributes(
	ctx context.Context, in *api.GetSubscriptionAttributesInput,
) (*api.GetSubscriptionAttributesOutput, error) {
	sub, err := p.callerSubscription(ctx, in.SubscriptionArn)
	if err != nil {
		return nil, err
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()

	return &api.GetSubscriptionAttributesOutput{Attributes: sub.attributes()}, nil
}

// SetSubscriptionAttributes sets one subscription attribute after checking it
// as LocalStack's set_subscription_attributes does.
func (p *Provider) SetSubscriptionAttributes(
	ctx context.Context, in *api.SetSubscriptionAttributesInput,
) (*api.SetSubscriptionAttributesOutput, error) {
	sub, err := p.callerSubscription(ctx, in.SubscriptionArn)
	if err != nil {
		return nil, err
	}

	name := deref(in.AttributeName)

	err = checkAttribute(setAttributePrefix, name, in.AttributeValue, sub.TopicARN, sub.Endpoint)
	if err != nil {
		return nil, err
	}

	value := deref(in.AttributeValue)

	sub.mu.Lock()
	defer sub.mu.Unlock()

	switch name {
	case attrRawMessageDelivery:
		value = strings.ToLower(value)
	case attrFilterPolicy:
		// The policy is checked against the scope the subscription has now.
		// LocalStack does not check it again when the scope changes.
		policy, perr := filter.Parse(value, filter.Scope(sub.FilterPolicyScope))
		if perr != nil {
			return nil, filterRefusal(setAttributePrefix, perr)
		}

		sub.policy = nil
		if !policy.Empty() {
			sub.policy = policy
		}
	default:
	}

	sub.set(name, value)

	return &api.SetSubscriptionAttributesOutput{}, nil
}

// callerSubscription is the subscription arn names in the caller's account and
// region, where LocalStack looks for it whatever account the ARN names.
func (p *Provider) callerSubscription(ctx context.Context, arn *string) (*Subscription, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	sub, ok := st.subscriptions[deref(arn)]
	st.mu.RUnlock()

	if !ok {
		return nil, api.NewNotFoundException("Subscription does not exist")
	}

	return sub, nil
}

// checkAttribute is LocalStack's validate_subscription_attribute: name has to
// be one a caller sets, and its value one that attribute takes. prefix starts
// the refusal: subscribePrefix or setAttributePrefix. Where LocalStack fails
// on a value that is missing, or a RedrivePolicy that is JSON but not an
// object, this refuses the value instead.
func checkAttribute(prefix, name string, value *string, topicARN, endpoint string) error {
	v := deref(value)

	switch name {
	case attrDeliveryPolicy, attrSubscriptionRoleARN:
		return nil
	case attrFilterPolicy:
		err := filter.CheckJSON(v)
		if err != nil {
			return filterRefusal(prefix, err)
		}

		return nil
	case attrFilterPolicyScope:
		if v != string(filter.ScopeMessageAttributes) && v != string(filter.ScopeMessageBody) {
			return api.NewInvalidParameterException(prefix + "FilterPolicyScope: Invalid value [" + pyStr(value) +
				"]. Please use either MessageBody or MessageAttributes")
		}

		return nil
	case attrRawMessageDelivery:
		if lower := strings.ToLower(v); lower != valueTrue && lower != valueFalse {
			return api.NewInvalidParameterException(prefix + "RawMessageDelivery: Invalid value [" + pyStr(value) +
				"]. Must be true or false.")
		}

		return nil
	case attrRedrivePolicy:
		return checkRedrivePolicy(prefix, v, topicARN, endpoint)
	default:
		return api.NewInvalidParameterException(prefix + "AttributeName")
	}
}

// checkRedrivePolicy checks that a RedrivePolicy is JSON and names a
// dead-letter queue by ARN, a FIFO queue for a FIFO queue subscribed to a FIFO
// topic.
func checkRedrivePolicy(prefix, v, topicARN, endpoint string) error {
	var policy any

	err := json.Unmarshal([]byte(v), &policy)
	if err != nil {
		return api.NewInvalidParameterException(prefix + "RedrivePolicy: failed to parse JSON.")
	}

	object, _ := policy.(map[string]any)
	target, _ := object["deadLetterTargetArn"].(string)

	parts, ok := parseARN(target)
	if !ok {
		return api.NewInvalidParameterException(prefix + "RedrivePolicy: deadLetterTargetArn is an invalid arn")
	}

	if strings.HasSuffix(topicARN, fifoSuffix) && strings.HasSuffix(endpoint, fifoSuffix) &&
		(!strings.HasSuffix(parts.resource, fifoSuffix) || !strings.Contains(parts.service, protocolSQS)) {
		return api.NewInvalidParameterException(
			prefix + "RedrivePolicy: must use a FIFO queue as DLQ for a FIFO Subscription to a FIFO Topic.")
	}

	return nil
}

// filterRefusal is a filter policy refusal as InvalidParameter, after prefix.
func filterRefusal(prefix string, err error) error {
	if ferr, ok := errors.AsType[*filter.Error](err); ok {
		return api.NewInvalidParameterException(prefix + ferr.Reason)
	}

	return err
}

// pyStr is a member as LocalStack prints it in a message: its value, or None
// when the request left it out.
func pyStr(s *string) string {
	if s == nil {
		return pyNone
	}

	return *s
}

// attributes are the subscription's attributes as GetSubscriptionAttributes
// returns them. The caller holds the subscription's lock.
func (sub *Subscription) attributes() map[string]string {
	out := map[string]string{
		"TopicArn":              sub.TopicARN,
		"Endpoint":              sub.Endpoint,
		"Protocol":              sub.Protocol,
		"SubscriptionArn":       sub.ARN,
		"PendingConfirmation":   sub.PendingConfirmation,
		"Owner":                 sub.Owner,
		"SubscriptionPrincipal": sub.SubscriptionPrincipal,
		attrRawMessageDelivery:  sub.RawMessageDelivery,
	}

	if sub.ConfirmationWasAuthenticated != "" {
		out["ConfirmationWasAuthenticated"] = sub.ConfirmationWasAuthenticated
	}

	for _, name := range subscriptionAttributeNames {
		if name == attrRawMessageDelivery {
			continue
		}

		if v, ok := sub.get(name); ok {
			out[name] = v
		}
	}

	if sub.FilterPolicy == "" {
		delete(out, attrFilterPolicyScope)
	}

	return out
}

// get is the value of an attribute a caller sets, and whether the
// subscription has it. FilterPolicyScope and RawMessageDelivery always have
// a value; an empty FilterPolicy or RedrivePolicy is no value. The caller
// holds the subscription's lock, or owns it alone.
func (sub *Subscription) get(name string) (string, bool) {
	switch name {
	case attrFilterPolicy:
		return sub.FilterPolicy, sub.FilterPolicy != ""
	case attrFilterPolicyScope:
		return sub.FilterPolicyScope, true
	case attrRawMessageDelivery:
		return sub.RawMessageDelivery, true
	case attrRedrivePolicy:
		return sub.RedrivePolicy, sub.RedrivePolicy != ""
	case attrDeliveryPolicy:
		return deref(sub.DeliveryPolicy), sub.DeliveryPolicy != nil
	case attrSubscriptionRoleARN:
		return deref(sub.SubscriptionRoleARN), sub.SubscriptionRoleARN != nil
	default:
		return "", false
	}
}

// set sets an attribute a caller sets, checked already. The caller holds the
// subscription's lock, or owns it alone.
func (sub *Subscription) set(name, value string) {
	switch name {
	case attrFilterPolicy:
		sub.FilterPolicy = value
	case attrFilterPolicyScope:
		sub.FilterPolicyScope = value
	case attrRawMessageDelivery:
		sub.RawMessageDelivery = value
	case attrRedrivePolicy:
		sub.RedrivePolicy = value
	case attrDeliveryPolicy:
		sub.DeliveryPolicy = new(value)
	case attrSubscriptionRoleARN:
		sub.SubscriptionRoleARN = new(value)
	default:
	}
}
