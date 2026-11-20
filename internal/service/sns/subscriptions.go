package sns

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
)

const (
	// subscriptionsPageSize is the most subscriptions a page of
	// ListSubscriptions or ListSubscriptionsByTopic holds.
	subscriptionsPageSize = 100
	// listedPendingARN is what ListSubscriptions shows for the ARN of a
	// subscription pending confirmation.
	listedPendingARN = "PendingConfirmation"
	// subscriptionIDFields is the number of fields of an ARN that names a
	// topic and not one of its subscriptions.
	subscriptionIDFields = 6
)

// Unsubscribe deletes a subscription, as LocalStack's unsubscribe: in the
// account and region its ARN names, so the unsigned UnsubscribeURL of a
// delivered message reaches it. A subscription pending confirmation is
// deleted like any other. A subscription that is not there is no error,
// unless the ARN has no subscription ID.
func (p *Provider) Unsubscribe(ctx context.Context, in *api.UnsubscribeInput) (*api.UnsubscribeOutput, error) {
	if in.SubscriptionArn == nil {
		return nil, api.NewInvalidParameterException(
			"Invalid parameter: SubscriptionArn Reason: no value for required parameter")
	}

	arn := *in.SubscriptionArn
	fields := strings.Count(arn, ":") + 1

	parts, ok := parseARN(arn)
	if !ok {
		return nil, api.NewInvalidParameterException(
			"Invalid parameter: SubscriptionArn Reason: An ARN must have at least 6 elements, not " +
				strconv.Itoa(fields))
	}

	st := p.stateAt(parts.account, parts.region)

	st.mu.Lock()

	sub, ok := st.subscriptions[arn]
	if ok {
		st.removeSubscription(sub)
	}

	st.mu.Unlock()

	if !ok {
		if fields == subscriptionIDFields {
			return nil, api.NewInvalidParameterException("Invalid parameter: SubscriptionId")
		}

		return &api.UnsubscribeOutput{}, nil
	}

	if sub.Protocol == protocolHTTP || sub.Protocol == protocolHTTPS {
		rc, err := callOf(ctx)
		if err != nil {
			return nil, err
		}

		p.subscriptionMessage(envelope.UnsubscribeConfirmation, sub, newToken(rc.Region))
	}

	return &api.UnsubscribeOutput{}, nil
}

// ConfirmSubscription confirms a subscription by its token, as LocalStack's
// confirm_subscription. The call may come unsigned, from the SubscribeURL of
// a confirmation message, so the account and region are the topic ARN's. The
// region has to be the one the token names; the topic is not looked up.
// AuthenticateOnUnsubscribe is taken and not acted on, as in LocalStack.
func (p *Provider) ConfirmSubscription(
	_ context.Context, in *api.ConfirmSubscriptionInput,
) (*api.ConfirmSubscriptionOutput, error) {
	parts, ok := parseARN(deref(in.TopicArn))
	if !ok {
		return nil, api.NewInvalidParameterException("Invalid parameter: Topic")
	}

	token := deref(in.Token)

	tokenRegion, err := regionOfToken(token)
	if err != nil {
		return nil, err
	}

	if tokenRegion != parts.region {
		return nil, api.NewInvalidParameterException("Invalid parameter: Topic")
	}

	st := p.stateAt(parts.account, parts.region)

	st.mu.RLock()
	sub, ok := st.subscriptions[st.tokens[token]]
	st.mu.RUnlock()

	if !ok {
		return nil, api.NewInvalidParameterException("Invalid parameter: Token")
	}

	sub.mu.Lock()
	sub.PendingConfirmation = valueFalse
	sub.ConfirmationWasAuthenticated = valueTrue
	sub.mu.Unlock()

	return &api.ConfirmSubscriptionOutput{SubscriptionArn: new(sub.ARN)}, nil
}

// ListSubscriptions lists the caller's subscriptions in the region in creation
// order, a page of 100 at a time, as LocalStack's list_subscriptions. A
// subscription pending confirmation shows PendingConfirmation for its ARN.
//
// The NextToken is the base64 of the ARN of the next page's first
// subscription. LocalStack makes it from the ARN it shows, so every pending
// subscription has the same token and a page that starts with one pages from
// the first; the token here is made from the real ARN.
func (p *Provider) ListSubscriptions(
	ctx context.Context, in *api.ListSubscriptionsInput,
) (*api.ListSubscriptionsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	defer st.mu.RUnlock()

	page, next := subscriptionsPage(st.subOrder, in.NextToken, st.subscriptions)

	out := &api.ListSubscriptionsOutput{Subscriptions: make([]api.Subscription, 0, len(page)), NextToken: next}
	for _, sub := range page {
		out.Subscriptions = append(out.Subscriptions, sub.listed(sub.shownARN()))
	}

	return out, nil
}

// ListSubscriptionsByTopic lists a topic's subscriptions in creation order, a
// page of 100 at a time, paged as ListSubscriptions. A subscription pending
// confirmation shows its ARN, as in LocalStack.
func (p *Provider) ListSubscriptionsByTopic(
	ctx context.Context, in *api.ListSubscriptionsByTopicInput,
) (*api.ListSubscriptionsByTopicOutput, error) {
	st, t, err := p.topicIn(ctx, in.TopicArn)
	if err != nil {
		return nil, err
	}

	st.mu.RLock()
	defer st.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	page, next := subscriptionsPage(t.subs, in.NextToken, st.subscriptions)

	out := &api.ListSubscriptionsByTopicOutput{Subscriptions: make([]api.Subscription, 0, len(page)), NextToken: next}
	for _, sub := range page {
		out.Subscriptions = append(out.Subscriptions, sub.listed(sub.ARN))
	}

	return out, nil
}

// subscriptionsPage is LocalStack's PaginatedList.get_page over subs, sorted
// by seq: the page that starts at the subscription token names, or at the
// first when it names none, and the token of the page after it. byARN finds
// the subscription a token names.
func subscriptionsPage(
	subs []*Subscription, token *string, byARN map[string]*Subscription,
) ([]*Subscription, *string) {
	if len(subs) <= subscriptionsPageSize && token == nil {
		return subs, nil
	}

	start := 0

	if token != nil {
		// Strict decoding takes only the canonical encoding, so the token names
		// a subscription exactly when it is the base64 of its ARN.
		arn, err := base64.StdEncoding.Strict().DecodeString(*token)
		if sub, ok := byARN[string(arn)]; err == nil && ok {
			if i, found := slices.BinarySearchFunc(subs, sub.seq, subBySeq); found && subs[i] == sub {
				start = i
			}
		}
	}

	end := min(start+subscriptionsPageSize, len(subs))
	if end < len(subs) {
		return subs[start:end], new(base64.StdEncoding.EncodeToString([]byte(subs[end].ARN)))
	}

	return subs[start:end], nil
}

// shownARN is the ARN ListSubscriptions shows: PendingConfirmation for a
// subscription pending confirmation.
func (sub *Subscription) shownARN() string {
	sub.mu.Lock()
	defer sub.mu.Unlock()

	if sub.PendingConfirmation == valueTrue {
		return listedPendingARN
	}

	return sub.ARN
}

// listed is the subscription as a listing shows it, with arn for its ARN.
func (sub *Subscription) listed(arn string) api.Subscription {
	return api.Subscription{
		SubscriptionArn: new(arn),
		Owner:           new(sub.Owner),
		Protocol:        new(sub.Protocol),
		Endpoint:        new(sub.Endpoint),
		TopicArn:        new(sub.TopicARN),
	}
}
