package sns

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

const (
	sqsService     = "sqs"
	sqsSendMessage = "AmazonSQS.SendMessage"
	queueHost      = "localhost.localstack.cloud:4566"
	queueScheme    = "http"
	// callOverhead is the room a SendMessage call takes besides its body and
	// queue URL.
	callOverhead     = 256
	internalHeader   = "X-Localstack-Data"
	traceHeader      = "X-Amzn-Trace-Id"
	traceAttribute   = "AWSTraceHeader"
	signatureVersion = "SignatureVersion"
)

// Bind hands the Provider the handler it delivers to SQS through, normally
// the gateway's.
func (p *Provider) Bind(h http.Handler) {
	p.client.Bind(h)
}

// subscriber is what delivery reads of a subscription, taken under its lock.
type subscriber struct {
	sub      *Subscription
	arn      string
	protocol string
	endpoint string
	raw      bool
	scope    string
	policy   *filter.Policy
	dlq      string
	// deliveryPolicy is the subscription's DeliveryPolicy, and contentType
	// the Content-Type an HTTP Notification goes with, "" for the default.
	deliveryPolicy string
	contentType    string
}

// subscriberOf reads sub, under its lock.
func subscriberOf(sub *Subscription) subscriber {
	return subscriber{
		sub: sub, arn: sub.ARN, protocol: sub.Protocol, endpoint: sub.Endpoint,
		raw: sub.RawMessageDelivery == valueTrue, scope: sub.FilterPolicyScope, policy: sub.policy,
		dlq: deadLetterTarget(sub.RedrivePolicy), deliveryPolicy: deref(sub.DeliveryPolicy),
	}
}

// delivers reports whether SNS delivers to the protocol: SQS, HTTP and HTTPS.
// Delivery to the others belongs to later tickets.
func delivers(protocol string) bool {
	return protocol == protocolSQS || isHTTP(protocol)
}

func isHTTP(protocol string) bool {
	return protocol == protocolHTTP || protocol == protocolHTTPS
}

// subscribers are t's confirmed SQS, HTTP and HTTPS subscriptions as they
// stand now.
func (t *Topic) subscribers() []subscriber {
	t.mu.Lock()
	subs := slices.Clone(t.subs)
	t.mu.Unlock()

	out := make([]subscriber, 0, len(subs))

	for _, sub := range subs {
		if !delivers(sub.Protocol) {
			continue
		}

		sub.mu.Lock()
		s := subscriberOf(sub)
		confirmed := sub.PendingConfirmation == valueFalse
		sub.mu.Unlock()

		if confirmed {
			out = append(out, s)
		}
	}

	return out
}

// deadLetterTarget is the deadLetterTargetArn of a RedrivePolicy, "" when
// there is none.
func deadLetterTarget(policy string) string {
	if policy == "" {
		return ""
	}

	var doc struct {
		Target string `json:"deadLetterTargetArn"`
	}

	if json.Unmarshal([]byte(policy), &doc) != nil {
		return ""
	}

	return doc.Target
}

// matches is LocalStack's _should_publish less the confirmation, which
// subscribers checked: the filter policy on the scope the subscription names.
func (s *subscriber) matches(m *outgoing) bool {
	if s.policy.Empty() {
		return true
	}

	if s.scope == string(filter.ScopeMessageBody) {
		return s.policy.MatchBody(m.bodyFor(s.protocol))
	}

	return s.policy.MatchAttributes(m.filterAttributes())
}

// outgoing is a message on its way to the subscribers of one publish, with
// what is worked out once for all of them: once for each content, as a
// MessageStructure=json message can give each protocol its own.
type outgoing struct {
	*message

	signed map[string]signedResult
	parsed map[string]*filter.Body
	attrs  map[string]filter.Attribute
}

type signedResult struct {
	signed *envelope.Signed
	err    error
}

// contentKey names the content protocol gets: its own member of a
// MessageStructure=json message, or "" for the default one.
func (m *message) contentKey(protocol string) string {
	if _, ok := m.bodies[protocol]; ok {
		return protocol
	}

	return ""
}

// bodyFor is the content protocol gets, parsed for a MessageBody filter.
func (m *outgoing) bodyFor(protocol string) *filter.Body {
	key := m.contentKey(protocol)

	body, ok := m.parsed[key]
	if !ok {
		body = filter.ParseBody(m.content(protocol))

		if m.parsed == nil {
			m.parsed = make(map[string]*filter.Body, 1)
		}

		m.parsed[key] = body
	}

	return body
}

func (m *outgoing) filterAttributes() map[string]filter.Attribute {
	if m.attrs == nil {
		m.attrs = make(map[string]filter.Attribute, len(m.message.attrs))
		for name, a := range m.message.attrs {
			m.attrs[name] = filter.Attribute{DataType: deref(a.DataType), StringValue: deref(a.StringValue)}
		}
	}

	return m.attrs
}

// envelopeFor is the signed envelope of the message as protocol gets it,
// signed on first use, once for every subscriber that gets the same content.
func (p *Provider) envelopeFor(m *outgoing, version, protocol string) (*envelope.Signed, error) {
	key := m.contentKey(protocol)

	r, ok := m.signed[key]
	if !ok {
		r.signed, r.err = p.signer.Sign(m.envelopeMessage(version, protocol), p.baseURL())

		if m.signed == nil {
			m.signed = make(map[string]signedResult, 1)
		}

		m.signed[key] = r
	}

	return r.signed, r.err
}

func (m *message) envelopeMessage(version, protocol string) *envelope.Message {
	em := &envelope.Message{
		MessageID: m.id, TopicARN: m.topicARN, Subject: m.subject, Message: m.content(protocol),
		Timestamp: m.time, SequenceNumber: m.sequence, SignatureVersion: version,
	}

	if len(m.attrs) > 0 {
		em.Attributes = make([]envelope.Attribute, 0, len(m.attrs))
		for name, a := range m.attrs {
			em.Attributes = append(em.Attributes, envelope.Attribute{
				Name: name, DataType: deref(a.DataType), StringValue: a.StringValue, BinaryValue: a.BinaryValue,
			})
		}
	}

	return em
}

// delivery is one message on its way to one queue or endpoint.
type delivery struct {
	sub  *Subscription
	call inproc.Call
	// dlq makes the call that puts the message on the subscription's
	// dead-letter queue, nil without one. It runs only when the delivery
	// fails, on the body the delivery holds already, so a waiting delivery
	// holds the message once.
	dlq func() inproc.Call
	// post is the delivery to an HTTP or HTTPS endpoint, nil for a queue.
	post *post
}

// fanOut queues the deliveries of msgs, published together to t, to every
// confirmed subscription whose filter policy matches, and returns them
// for lanes.dispatch, which the caller runs once it holds no lock. The bodies
// are built with no lock held; the deliveries are queued under t's lock, after
// a check that each subscription is still the topic's, so a subscription
// removed, or a topic deleted, before the publish queues its deliveries gets
// none. A delivery queued before the removal answered is made.
func (p *Provider) fanOut(rc *request.Context, t *Topic, msgs []*message) []*job {
	p.recordDevices(t, msgs)

	subs := t.subscribers()
	if len(subs) == 0 {
		return nil
	}

	version := t.attribute(signatureVersion)
	trace := traceOf(rc)
	out := make([]outgoing, len(msgs))

	for i, m := range msgs {
		out[i].message = m
	}

	var (
		deliveries  []delivery
		topicPolicy string
		policyRead  bool
	)

	for i := range subs {
		s := &subs[i]

		if isHTTP(s.protocol) {
			if !policyRead {
				topicPolicy, policyRead = t.attribute(attrTopicDeliveryPolicy), true
			}

			s.contentType = contentType(s.deliveryPolicy, topicPolicy, s.protocol)
		}

		for j := range out {
			m := &out[j]
			if !s.matches(m) {
				continue
			}

			d, ok := p.deliveryOf(s, m, version, trace)
			if ok {
				deliveries = append(deliveries, d)
			}
		}
	}

	return p.queue(t, deliveries)
}

// deliveryOf is the delivery of m to s: the SendMessage call that puts m on
// s's queue, or the POST to s's endpoint.
func (p *Provider) deliveryOf(s *subscriber, m *outgoing, version, trace string) (delivery, bool) {
	if isHTTP(s.protocol) {
		return p.httpDeliveryOf(s, m, version)
	}

	queue, ok := parseARN(s.endpoint)
	if !ok {
		slog.Error("SNS delivery skipped: unparseable queue ARN", "subscription", s.arn, "endpoint", s.endpoint)

		return delivery{}, false
	}

	body := m.content(protocolSQS)
	if !s.raw {
		signed, err := p.envelopeFor(m, version, protocolSQS)
		if err != nil {
			slog.Error("SNS delivery skipped: cannot sign the message", "subscription", s.arn, "error", err)

			return delivery{}, false
		}

		body = string(signed.Body(s.arn))
	}

	params := sendParams{body: body, trace: trace}
	if s.raw {
		params.attrs = m.message.attrs
	}

	if strings.HasSuffix(m.topicARN, fifoSuffix) {
		if strings.HasSuffix(s.endpoint, fifoSuffix) {
			params.groupID, params.dedupID = m.groupID, m.dedupID
			if params.dedupID == "" {
				sum := sha256.Sum256([]byte(m.content(protocolSQS)))
				params.dedupID = hex.EncodeToString(sum[:])
			}
		}
	} else {
		params.groupID = m.groupID
	}

	call, span := sendCall(queue, s.endpoint, m.topicARN, &params)
	d := delivery{sub: s.sub, call: call}

	if s.dlq != "" {
		if _, ok := parseARN(s.dlq); ok {
			d.dlq = span.deadLetter(call.Body, s.dlq, s.arn)
		}
	}

	return d, true
}

// queue adds each delivery whose subscription is still t's to the lane of its
// queue, under t's lock, which fixes the deliveries' order in each lane. It
// sends nothing and waits for nothing: lanes.dispatch sends, with no lock
// held.
func (p *Provider) queue(t *Topic, deliveries []delivery) []*job {
	ls := &p.lanes
	jobs := make([]*job, 0, len(deliveries))

	t.mu.Lock()
	ls.mu.Lock()

	for _, d := range deliveries {
		if t.endpoints[d.sub.Endpoint] != d.sub {
			continue
		}

		j := &job{call: d.call, dlq: d.dlq, post: d.post}
		if ls.add(j) {
			jobs = append(jobs, j)
		}
	}

	ls.mu.Unlock()
	t.mu.Unlock()

	return jobs
}

// sendParams are the members of a SendMessage besides the queue.
type sendParams struct {
	body    string
	attrs   map[string]api.MessageAttributeValue
	groupID string
	dedupID string
	trace   string
}

// sendCall is the json SendMessage that puts a message on the queue arn names,
// as an internal call from source, on the lane of the queue so a queue's
// messages arrive in the order they were queued and a slow queue holds up no
// other. It also says which members of the call's body a dead-letter call
// repeats.
func sendCall(queue arnParts, arn, source string, params *sendParams) (inproc.Call, sendSpan) {
	queueURL := queueURLOf(queue)

	b := make([]byte, 0, len(params.body)+len(queueURL)+callOverhead)
	b = append(b, `{"QueueUrl":`...)
	b = appendJSONString(b, queueURL)

	from := len(b)

	b = append(b, `,"MessageBody":`...)
	b = appendJSONString(b, params.body)

	if len(params.attrs) > 0 {
		b = appendSQSAttributes(b, params.attrs)
	}

	if params.groupID != "" {
		b = append(b, `,"MessageGroupId":`...)
		b = appendJSONString(b, params.groupID)
	}

	if params.dedupID != "" {
		b = append(b, `,"MessageDeduplicationId":`...)
		b = appendJSONString(b, params.dedupID)
	}

	to := len(b)

	if params.trace != "" {
		b = append(b, `,"MessageSystemAttributes":{"`+traceAttribute+`":{"DataType":"String","StringValue":`...)
		b = appendJSONString(b, params.trace)
		b = append(b, "}}"...)
	}

	b = append(b, '}')

	return internalSend(queue, arn, source, b), sendSpan{from: from, to: to}
}

func queueURLOf(queue arnParts) string {
	return queueScheme + "://sqs." + queue.region + "." + queueHost + "/" + queue.account + "/" + queue.resource
}

// internalSend is the internal call that sends body, a SendMessage's, to the
// queue arn names, from source.
func internalSend(queue arnParts, arn, source string, body []byte) inproc.Call {
	meta := appendJSONString([]byte(`{"source_arn":`), source)
	meta = append(meta, `,"service_principal":"sns"}`...)

	return inproc.Call{
		Account: queue.account, Region: queue.region, Service: sqsService, Target: sqsSendMessage, Body: body,
		Header: http.Header{internalHeader: {string(meta)}}, Lane: arn,
	}
}

// sendSpan is where in the body of a SendMessage the members start and end
// that a dead-letter call to another queue repeats: the message body, its
// attributes and its FIFO ids, but not the queue or the trace header.
type sendSpan struct{ from, to int }

// deadLetter is the call that sends the message of primary, a SendMessage this
// span describes, to the dead-letter queue dlq for the subscription source. It
// splices the bytes primary holds, so the dead letter carries what the queue
// was sent, and it builds nothing until the delivery fails.
func (sp sendSpan) deadLetter(primary []byte, dlq, source string) func() inproc.Call {
	return func() inproc.Call {
		target, _ := parseARN(dlq) // it parsed when the delivery was built.
		queueURL := queueURLOf(target)

		b := make([]byte, 0, sp.to-sp.from+len(queueURL)+callOverhead)
		b = append(b, `{"QueueUrl":`...)
		b = appendJSONString(b, queueURL)
		b = append(b, primary[sp.from:sp.to]...)
		b = append(b, '}')

		return internalSend(target, dlq, source, b)
	}
}

// appendSQSAttributes writes a raw message's attributes as SQS message
// attributes.
func appendSQSAttributes(b []byte, attrs map[string]api.MessageAttributeValue) []byte {
	b = append(b, `,"MessageAttributes":{`...)

	for i, name := range slices.Sorted(maps.Keys(attrs)) {
		a := attrs[name]
		if i > 0 {
			b = append(b, ',')
		}

		b = appendJSONString(b, name)
		b = append(b, `:{"DataType":`...)
		b = appendJSONString(b, deref(a.DataType))

		if strings.HasPrefix(deref(a.DataType), typeBinary) {
			b = append(b, `,"BinaryValue":"`...)
			b = base64.StdEncoding.AppendEncode(b, a.BinaryValue)
			b = append(b, '"')
		} else {
			b = append(b, `,"StringValue":`...)
			b = appendJSONString(b, deref(a.StringValue))
		}

		b = append(b, '}')
	}

	return append(b, '}')
}

// traceOf is the X-Ray trace header of the publish, which SNS passes to SQS
// as the AWSTraceHeader system attribute.
func traceOf(rc *request.Context) string {
	if rc == nil || rc.Request == nil {
		return ""
	}

	return rc.Request.Header.Get(traceHeader)
}

// attribute is one of the topic's attributes.
func (t *Topic) attribute(name string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.Attributes[name]
}
