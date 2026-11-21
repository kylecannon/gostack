package sns

import (
	"strings"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

// Messages to a platform endpoint or a phone number are recorded, not sent, as
// in LocalStack, which keeps them for the developer endpoints: LocalStack's
// ApplicationTopicPublisher and SmsTopicPublisher for the subscribers of a
// topic, ApplicationEndpointPublisher and SmsPhoneNumberPublisher for a publish
// to an endpoint ARN or a number (publish.go).

// platformOf is the platform an endpoint ARN names, LocalStack's
// get_platform_type_from_endpoint_arn: the second of the last four
// slash-separated fields of arn:...:endpoint/<platform>/<application>/<id>. An
// ARN with fewer fields, which LocalStack crashes on, names none, so a message
// to it takes its default body.
func platformOf(arn string) string {
	fields := strings.Split(arn, "/")
	if len(fields) < endpointARNFields {
		return ""
	}

	return fields[len(fields)-endpointARNFields+1]
}

// endpointARNFields is the number of slash-separated fields of an endpoint ARN
// platformOf reads.
const endpointARNFields = 4

// deviceSubscribers are t's confirmed application and sms subscriptions as they
// stand now.
func (t *Topic) deviceSubscribers() []subscriber {
	t.mu.Lock()
	subs := make([]*Subscription, 0, len(t.subs))

	for _, sub := range t.subs {
		if sub.Protocol == protocolApp || sub.Protocol == protocolSMS {
			subs = append(subs, sub)
		}
	}
	t.mu.Unlock()

	out := make([]subscriber, 0, len(subs))

	for _, sub := range subs {
		sub.mu.Lock()
		s := subscriber{
			sub: sub, arn: sub.ARN, endpoint: sub.Endpoint, scope: sub.FilterPolicyScope, policy: sub.policy,
		}
		confirmed := sub.PendingConfirmation == valueFalse
		sub.mu.Unlock()

		if confirmed {
			out = append(out, s)
		}
	}

	return out
}

// matchesDevice is LocalStack's _should_publish for a subscriber that gets a
// message by protocol: the filter policy on the scope the subscription names,
// with the message body that protocol would get.
func (s *subscriber) matchesDevice(m *outgoing, protocol string) bool {
	if s.policy.Empty() {
		return true
	}

	if s.scope == string(filter.ScopeMessageBody) {
		return s.policy.MatchBody(filter.ParseBody(m.content(protocol)))
	}

	return s.policy.MatchAttributes(m.filterAttributes())
}

// recordDevices records msgs, published together to t, for each of its
// confirmed application and sms subscriptions whose filter policy matches, in
// the State that holds the topic.
func (p *Provider) recordDevices(t *Topic, msgs []*message) {
	subs := t.deviceSubscribers()
	if len(subs) == 0 {
		return
	}

	parts, ok := parseARN(t.ARN)
	if !ok {
		return
	}

	out := make([]outgoing, len(msgs))
	for i, m := range msgs {
		out[i].message = m
	}

	st := p.stateAt(parts.account, parts.region)

	var recorded []recordedFor

	for i := range subs {
		s := &subs[i]
		protocol := s.sub.Protocol

		for j := range out {
			m := &out[j]
			if !s.matchesDevice(m, protocol) {
				continue
			}

			recorded = append(recorded, s.record(m.message, t.ARN))
		}
	}

	if len(recorded) == 0 {
		return
	}

	st.mu.Lock()

	for i := range recorded {
		rec := &recorded[i]
		if rec.sms {
			st.smsMessages = append(st.smsMessages, rec.RecordedMessage)
		} else {
			st.endpointMessages[rec.TargetARN] = append(st.endpointMessages[rec.TargetARN], rec.RecordedMessage)
		}
	}

	st.mu.Unlock()

	p.markChanged()
}

// recordedFor is a message a topic delivered to a device subscription, and
// the list it is kept in.
type recordedFor struct {
	RecordedMessage

	sms bool
}

// record is the message as the subscriber's list keeps it. An endpoint's list
// holds no message ID for a message that came by a topic, as LocalStack's
// ApplicationTopicPublisher puts none in it.
func (s *subscriber) record(m *message, topicARN string) recordedFor {
	if s.sub.Protocol == protocolSMS {
		rec := m.recorded(m.content(protocolSMS))
		rec.PhoneNumber, rec.TopicARN, rec.SubscriptionARN = s.endpoint, topicARN, s.arn

		return recordedFor{RecordedMessage: rec, sms: true}
	}

	rec := m.recorded(m.content(platformOf(s.endpoint)))
	rec.MessageID = ""
	rec.TargetARN, rec.TopicARN, rec.SubscriptionARN = s.endpoint, topicARN, s.arn

	return recordedFor{RecordedMessage: rec}
}
