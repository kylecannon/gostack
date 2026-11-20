package sns

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

const (
	protocolHTTP      = "http"
	protocolHTTPS     = "https"
	protocolEmail     = "email"
	protocolEmailJSON = "email-json"
	protocolSMS       = "sms"
	protocolSQS       = "sqs"
	protocolApp       = "application"

	// pendingSubscriptionARN is what Subscribe answers for an HTTP or HTTPS
	// subscription unless ReturnSubscriptionArn is set.
	pendingSubscriptionARN = "pending confirmation"

	// subscribePrefix starts every refusal of a Subscribe attribute;
	// SetSubscriptionAttributes refusals start with setAttributePrefix.
	subscribePrefix    = "Invalid parameter: Attributes Reason: "
	setAttributePrefix = "Invalid parameter: "

	// pyNone is how LocalStack prints a member the request left out.
	pyNone = "None"
)

// snsProtocols are the protocols Subscribe takes, LocalStack's SNS_PROTOCOLS.
var snsProtocols = []string{
	protocolHTTP, protocolHTTPS, protocolEmail, protocolEmailJSON, protocolSMS, protocolSQS, protocolApp,
	"lambda", "firehose",
}

// e164 is LocalStack's E164_REGEX, the phone numbers an sms subscription
// takes.
var e164 = regexp.MustCompile(`^\+?[1-9]\d{1,14}$`)

// Subscribe subscribes an endpoint to a topic, as LocalStack's subscribe. An
// endpoint is subscribed to a topic once: a repeated call answers the same
// subscription, unless it asks for an attribute the subscription has with a
// different value.
func (p *Provider) Subscribe(ctx context.Context, in *api.SubscribeInput) (*api.SubscribeOutput, error) {
	rc, cerr := callOf(ctx)
	if cerr != nil {
		return nil, cerr
	}

	st, t, err := p.topicIn(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	endpoint := deref(in.Endpoint)
	if endpoint == "" {
		return nil, api.NewNotFoundException("Endpoint not specified in subscription")
	}

	err = checkEndpoint(in.Protocol, endpoint, t.ARN)
	if err != nil {
		return nil, err
	}

	attrs, err := subscribeAttributes(in.Attributes, t.ARN, endpoint)
	if err != nil {
		return nil, err
	}

	sub, created, err := st.subscribe(rc, t, deref(in.Protocol), endpoint, attrs)
	if err != nil {
		return nil, err
	}

	arn := sub.ARN
	if created && (sub.Protocol == protocolHTTP || sub.Protocol == protocolHTTPS) {
		p.subscriptionMessage(envelope.SubscriptionConfirmation, sub, sub.Token)

		if in.ReturnSubscriptionArn == nil || !*in.ReturnSubscriptionArn {
			arn = pendingSubscriptionARN
		}
	}

	return &api.SubscribeOutput{SubscriptionArn: new(arn)}, nil
}

// subscriptionMessage is the one place Subscribe and Unsubscribe hand an HTTP
// or HTTPS subscription its SubscriptionConfirmation or
// UnsubscribeConfirmation, with token in its SubscribeURL. Delivery to HTTP
// endpoints is a later ticket's: it builds the message with p.Envelope and
// sends it here. Until then nothing is sent, and the token stays reachable
// through /_aws/sns/subscription-tokens.
func (*Provider) subscriptionMessage(envelope.Type, *Subscription, string) {}

// checkEndpoint is LocalStack's check of a Subscribe protocol and endpoint,
// in its order.
func checkEndpoint(protocol *string, endpoint, topicARN string) error {
	proto := deref(protocol)

	switch {
	case protocol == nil || !slices.Contains(snsProtocols, proto):
		name := proto
		if protocol == nil {
			name = pyNone
		}

		return api.NewInvalidParameterException(
			"Invalid parameter: Amazon SNS does not support this protocol string: " + name)
	case (proto == protocolHTTP || proto == protocolHTTPS) && !strings.HasPrefix(endpoint, proto+"://"):
		return api.NewInvalidParameterException("Invalid parameter: Endpoint must match the specified protocol")
	case proto == protocolSMS && !e164.MatchString(endpoint):
		return api.NewInvalidParameterException("Invalid SMS endpoint: " + endpoint)
	case proto == protocolSQS && !isARN(endpoint):
		return api.NewInvalidParameterException("Invalid parameter: SQS endpoint ARN")
	case proto == protocolApp && !isARN(endpoint):
		return api.NewInvalidParameterException("Invalid parameter: ApplicationEndpoint ARN")
	}

	if strings.Contains(endpoint, fifoSuffix) && !strings.Contains(topicARN, fifoSuffix) {
		return api.NewInvalidParameterException("Invalid parameter: Invalid parameter: Endpoint Reason: " +
			"FIFO SQS Queues can not be subscribed to standard SNS topics")
	}

	return nil
}

func isARN(s string) bool {
	_, ok := parseARN(s)

	return ok
}

// subscribeAttributes checks the attributes of a Subscribe call, each as
// SetSubscriptionAttributes would with the Subscribe prefix, and returns them
// with RawMessageDelivery in lower case. The request's order is lost in the
// decoded map, so the names are checked in sorted order: of two refused
// attributes, the one first in that order is reported.
func subscribeAttributes(attrs map[string]string, topicARN, endpoint string) (map[string]string, error) {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}

	slices.Sort(names)

	out := make(map[string]string, len(attrs))

	for _, name := range names {
		value := attrs[name]

		err := checkAttribute(subscribePrefix, name, &value, topicARN, endpoint)
		if err != nil {
			return nil, err
		}

		if name == attrRawMessageDelivery {
			value = strings.ToLower(value)
		}

		out[name] = value
	}

	return out, nil
}

// subscribe finds the topic's subscription to endpoint, or makes one. It
// reports whether it made it.
func (s *State) subscribe(
	rc *request.Context, t *Topic, protocol, endpoint string, attrs map[string]string,
) (*Subscription, bool, error) {
	scope := filter.ScopeMessageAttributes
	if v, ok := attrs[attrFilterPolicyScope]; ok {
		scope = filter.Scope(v)
	}

	// LocalStack reads the policy only for a new subscription, after the
	// idempotency check, so its refusal waits for that check.
	policy, policyErr := filter.Parse(attrs[attrFilterPolicy], scope)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.topics[t.ARN] != t {
		return nil, false, api.NewNotFoundException("Topic does not exist")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if existing, ok := t.endpoints[endpoint]; ok {
		return existing, false, existing.sameAttributes(attrs)
	}

	if policyErr != nil {
		return nil, false, filterRefusal(subscribePrefix, policyErr)
	}

	sub := newSubscription(rc, t.ARN, protocol, endpoint, attrs)
	if !policy.Empty() {
		sub.policy = policy
	}

	s.addSubscription(t, sub)

	return sub, true, nil
}

// newSubscription is a subscription LocalStack's subscribe makes: pending
// for HTTP, HTTPS and email, confirmed for every other protocol.
func newSubscription(rc *request.Context, topicARN, protocol, endpoint string, attrs map[string]string) *Subscription {
	sub := &Subscription{
		Token:                 newToken(rc.Region),
		TopicARN:              topicARN,
		Endpoint:              endpoint,
		Protocol:              protocol,
		ARN:                   topicARN + ":" + newUUID(),
		PendingConfirmation:   valueTrue,
		Owner:                 rc.AccountID,
		SubscriptionPrincipal: "arn:" + account.Partition(rc.Region) + ":iam::" + rc.AccountID + ":user/DummySNSPrincipal",
		FilterPolicyScope:     string(filter.ScopeMessageAttributes),
		RawMessageDelivery:    valueFalse,
	}

	for name, value := range attrs {
		sub.set(name, value)
	}

	switch protocol {
	case protocolHTTP, protocolHTTPS, protocolEmail, protocolEmailJSON:
	default:
		sub.PendingConfirmation = valueFalse
		sub.ConfirmationWasAuthenticated = valueTrue
	}

	return sub
}

// addSubscription stores sub as the newest subscription, of the State and of
// t. The caller holds the State's lock for writing and t's lock.
func (s *State) addSubscription(t *Topic, sub *Subscription) {
	sub.seq = s.subNext
	s.subNext++
	s.subscriptions[sub.ARN] = sub
	s.subOrder = append(s.subOrder, sub)
	s.tokens[sub.Token] = sub.ARN

	if t.endpoints == nil {
		t.endpoints = make(map[string]*Subscription)
	}

	t.endpoints[sub.Endpoint] = sub
	t.subs = append(t.subs, sub)
}

// dropSubscriptions removes every subscription of t, a topic being deleted,
// from the State. AWS's DeleteTopic reference: "Deletes a topic and all its
// subscriptions." LocalStack leaves them in its store, listed and readable;
// the AWS document wins. The caller holds the State's lock for writing.
func (s *State) dropSubscriptions(t *Topic) {
	t.mu.Lock()
	subs := t.subs
	t.subs, t.endpoints = nil, nil
	t.mu.Unlock()

	if len(subs) == 0 {
		return
	}

	gone := make(map[*Subscription]struct{}, len(subs))
	for _, sub := range subs {
		delete(s.subscriptions, sub.ARN)
		delete(s.tokens, sub.Token)
		gone[sub] = struct{}{}
	}

	s.subOrder = slices.DeleteFunc(s.subOrder, func(sub *Subscription) bool {
		_, ok := gone[sub]

		return ok
	})
}

// removeSubscription drops sub from the State and from its topic, when the
// topic is still there. The caller holds the State's lock for writing.
func (s *State) removeSubscription(sub *Subscription) {
	delete(s.subscriptions, sub.ARN)
	delete(s.tokens, sub.Token)
	s.subOrder = deleteBySeq(s.subOrder, sub)

	t, ok := s.topics[sub.TopicARN]
	if !ok {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.endpoints[sub.Endpoint] == sub {
		delete(t.endpoints, sub.Endpoint)
	}

	t.subs = deleteBySeq(t.subs, sub)
}

// deleteBySeq removes sub from subs, which is sorted by seq.
func deleteBySeq(subs []*Subscription, sub *Subscription) []*Subscription {
	i, found := slices.BinarySearchFunc(subs, sub.seq, subBySeq)
	if found && subs[i] == sub {
		return slices.Delete(subs, i, i+1)
	}

	return subs
}

func subBySeq(sub *Subscription, seq uint64) int {
	switch {
	case sub.seq < seq:
		return -1
	case sub.seq > seq:
		return 1
	default:
		return 0
	}
}

// sameAttributes is LocalStack's check of a repeated Subscribe: every
// attribute asked for with a value that is not empty has to have that value
// on the subscription, the defaults included.
func (sub *Subscription) sameAttributes(attrs map[string]string) error {
	sub.mu.Lock()
	defer sub.mu.Unlock()

	for _, name := range subscriptionAttributeNames {
		want := attrs[name]
		if want == "" {
			continue
		}

		if have, ok := sub.get(name); !ok || have != want {
			return api.NewInvalidParameterException(
				subscribePrefix + "Subscription already exists with different attributes")
		}
	}

	return nil
}
