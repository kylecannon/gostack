package sns

import (
	"context"
	"strings"
	"time"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/request"
)

// message is one published message, as delivery reads it.
type message struct {
	id        string
	topicARN  string
	text      string
	subject   string
	structure string
	// bodies are the string members of a MessageStructure=json message by
	// protocol, nil for any other message.
	bodies   map[string]string
	attrs    map[string]api.MessageAttributeValue
	groupID  string
	dedupID  string
	sequence string
	time     time.Time
}

// content is the message text a protocol gets: its own member of a
// MessageStructure=json message, else the default one (SnsMessage.message_content).
//
//nolint:unparam // HTTP, email and Lambda delivery read their own protocol member.
func (m *message) content(protocol string) string {
	if m.bodies == nil {
		return m.text
	}

	if body, ok := m.bodies[protocol]; ok {
		return body
	}

	return m.bodies[defaultKey]
}

// Publish sends a message to a topic's subscribers, to a platform endpoint or
// to a phone number, as LocalStack's publish, in its order of checks.
// LocalStack does not refuse a call that names more than one destination: a
// PhoneNumber wins, then TopicArn, then TargetArn.
func (p *Provider) Publish(ctx context.Context, in *api.PublishInput) (*api.PublishOutput, error) {
	rc, callErr := callOf(ctx)
	if callErr != nil {
		return nil, callErr
	}

	err := checkPublish(in)
	if err != nil {
		return nil, err
	}

	target := deref(in.TopicArn)
	if target == "" {
		target = deref(in.TargetArn)
	}

	fifo := strings.Contains(target, fifoSuffix)

	if fifo {
		_, err = p.checkFIFO(ctx, target, in.MessageGroupId, in.MessageDeduplicationId)
		if err != nil {
			return nil, err
		}
	} else if in.MessageDeduplicationId != nil {
		return nil, api.NewInvalidParameterException("Invalid parameter: MessageDeduplicationId Reason: " +
			"The request includes MessageDeduplicationId parameter that is not valid for this topic type")
	}

	endpointPublish := strings.Contains(deref(in.TargetArn), ":endpoint/")

	msg := &message{
		id: newUUID(), text: deref(in.Message), subject: deref(in.Subject), structure: deref(in.MessageStructure),
		attrs: in.MessageAttributes, groupID: deref(in.MessageGroupId), dedupID: deref(in.MessageDeduplicationId),
		time: time.Now(),
	}

	if msg.structure == structureJSON {
		msg.bodies, err = parseStructure(msg.text)
		if err != nil {
			return nil, err
		}

		if _, ok := msg.bodies[defaultKey]; !ok && !endpointPublish {
			return nil, api.NewInvalidParameterException(
				"Invalid parameter: Message Structure - No default entry in JSON message body")
		}
	}

	if phone := deref(in.PhoneNumber); phone != "" {
		recordSMS(p.regions.Get(rc.AccountID, rc.Region), phone, msg)

		return &api.PublishOutput{MessageId: new(msg.id)}, nil
	}

	if endpointPublish {
		return p.publishToEndpoint(rc, deref(in.TargetArn), msg)
	}

	return p.publishToTopic(ctx, rc, target, msg)
}

// publishToEndpoint records msg for the platform endpoint arn.
func (p *Provider) publishToEndpoint(rc *request.Context, arn string, msg *message) (*api.PublishOutput, error) {
	parts, err := parseTopicARN(arn)
	if err != nil {
		return nil, err
	}

	err = recordEndpointMessage(p.stateAt(parts.account, rc.Region), arn, msg)
	if err != nil {
		return nil, err
	}

	return &api.PublishOutput{MessageId: new(msg.id)}, nil
}

// publishToTopic delivers msg to the subscribers of the topic arn.
func (p *Provider) publishToTopic(
	ctx context.Context, rc *request.Context, arn string, msg *message,
) (*api.PublishOutput, error) {
	_, t, err := p.topicIn(ctx, &arn)
	if err != nil {
		return nil, err
	}

	msg.topicARN = t.ARN

	if strings.Contains(arn, fifoSuffix) {
		p.publishFIFO(ctx, rc, t, []*message{msg})

		return &api.PublishOutput{MessageId: new(msg.id), SequenceNumber: new(msg.sequence)}, nil
	}

	p.fanOut(ctx, rc, t, []*message{msg})

	return &api.PublishOutput{MessageId: new(msg.id)}, nil
}

// checkPublish is the part of LocalStack's publish checks that reads only
// the request: the subject, the message, the phone number, the attributes and
// the size.
func checkPublish(in *api.PublishInput) error {
	if in.Subject != nil && *in.Subject == "" {
		return api.NewInvalidParameterException("Invalid parameter: Subject")
	}

	if deref(in.Message) == "" {
		return api.NewInvalidParameterException("Invalid parameter: Empty message")
	}

	if phone := deref(in.PhoneNumber); phone != "" && !e164.MatchString(phone) {
		return api.NewInvalidParameterException("Invalid parameter: PhoneNumber Reason: " + phone +
			" is not valid to publish to")
	}

	err := checkAttributes(in.MessageAttributes, 0)
	if err != nil {
		return err
	}

	if publishSize(deref(in.Message), in.MessageAttributes) > maxPublishSize {
		return api.NewInvalidParameterException("Invalid parameter: Message too long")
	}

	return nil
}

// checkFIFO is LocalStack's check of a publish to a FIFO topic: a group id,
// the topic, and a deduplication id unless the topic deduplicates by content.
func (p *Provider) checkFIFO(ctx context.Context, arn string, groupID, dedupID *string) (*Topic, error) {
	if deref(groupID) == "" {
		return nil, api.NewInvalidParameterException(
			"Invalid parameter: The MessageGroupId parameter is required for FIFO topics")
	}

	t, err := p.findTopic(ctx, &arn)
	if err != nil {
		return nil, err
	}

	if deref(dedupID) == "" && !t.contentDeduplication() {
		return nil, api.NewInvalidParameterException("Invalid parameter: The topic should either have " +
			"ContentBasedDeduplication enabled or MessageDeduplicationId provided explicitly")
	}

	return t, nil
}

// contentDeduplication reports whether the topic's ContentBasedDeduplication
// is on.
func (t *Topic) contentDeduplication() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.Attributes["ContentBasedDeduplication"] == valueTrue
}

// recordSMS keeps a message sent to a phone number for
// /_aws/sns/sms-messages, as LocalStack's SmsPhoneNumberPublisher does. The
// SMS ticket owns what else a send does.
func recordSMS(st *State, phone string, msg *message) {
	rec := msg.recorded()
	rec.PhoneNumber = phone

	st.mu.Lock()
	st.smsMessages = append(st.smsMessages, rec)
	st.mu.Unlock()
}

// recordEndpointMessage keeps a message published to a platform endpoint for
// /_aws/sns/platform-endpoint-messages, after LocalStack's checks that the
// endpoint is there and enabled. The platform ticket owns what else a publish
// to an endpoint does.
func recordEndpointMessage(st *State, arn string, msg *message) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	endpoint, ok := st.endpoints[arn]
	if !ok {
		return api.NewInvalidParameterException(
			"Invalid parameter: TargetArn Reason: No endpoint found for the target arn specified")
	}

	if !strings.EqualFold(endpoint.Attributes["Enabled"], valueTrue) {
		return api.NewEndpointDisabledException("Endpoint is disabled")
	}

	rec := msg.recorded()
	rec.TargetARN = arn
	st.endpointMessages[arn] = append(st.endpointMessages[arn], rec)

	return nil
}

// recorded is the message as the developer endpoints list it.
func (m *message) recorded() RecordedMessage {
	rec := RecordedMessage{
		MessageID: m.id, Message: m.text, MessageStructure: m.structure, Subject: m.subject,
	}

	if len(m.attrs) > 0 {
		rec.MessageAttributes = make(map[string]MessageAttribute, len(m.attrs))
		for name, a := range m.attrs {
			rec.MessageAttributes[name] = MessageAttribute{
				DataType: deref(a.DataType), StringValue: deref(a.StringValue), BinaryValue: a.BinaryValue,
			}
		}
	}

	return rec
}
