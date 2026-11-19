package sns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

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
	dlqSendTimeout   = 10 * time.Second
	signatureVersion = "SignatureVersion"
)

// Bind hands the Provider the handler it delivers to SQS through, normally
// the gateway's.
func (p *Provider) Bind(h http.Handler) {
	p.client.Bind(h)
}

// Close waits for the deliveries still queued until ctx ends, so a publish
// that was acknowledged reaches its queue on a clean stop, then stops the
// delivery workers, dropping what is left.
func (p *Provider) Close(ctx context.Context) {
	p.pending.wait(ctx)
	p.client.Close()
}

// Dropped is the number of deliveries lost: refused or not queued, with no
// dead-letter queue that took them.
func (p *Provider) Dropped() int64 {
	return p.dropped.Load()
}

// subscriber is what delivery reads of a subscription, taken under its lock.
type subscriber struct {
	sub      *Subscription
	arn      string
	endpoint string
	raw      bool
	scope    string
	policy   *filter.Policy
	dlq      string
}

// subscribers are t's confirmed SQS subscriptions as they stand now. Delivery
// to the other protocols belongs to later tickets.
func (t *Topic) subscribers() []subscriber {
	t.mu.Lock()
	subs := slices.Clone(t.subs)
	t.mu.Unlock()

	out := make([]subscriber, 0, len(subs))

	for _, sub := range subs {
		if sub.Protocol != protocolSQS {
			continue
		}

		sub.mu.Lock()
		s := subscriber{
			sub: sub, arn: sub.ARN, endpoint: sub.Endpoint, raw: sub.RawMessageDelivery == valueTrue,
			scope: sub.FilterPolicyScope, policy: sub.policy, dlq: deadLetterTarget(sub.RedrivePolicy),
		}
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
		return s.policy.MatchBody(m.sqsBody())
	}

	return s.policy.MatchAttributes(m.filterAttributes())
}

// outgoing is a message on its way to the subscribers of one publish, with
// what is worked out once for all of them.
type outgoing struct {
	*message

	signed     *envelope.Signed
	signErr    error
	signedOnce bool
	body       *filter.Body
	bodyParsed bool
	attrs      map[string]filter.Attribute
}

func (m *outgoing) sqsBody() *filter.Body {
	if !m.bodyParsed {
		m.body, m.bodyParsed = filter.ParseBody(m.content(protocolSQS)), true
	}

	return m.body
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

// envelopeFor is the signed envelope of the message, signed on first use,
// once for every subscriber.
func (p *Provider) envelopeFor(m *outgoing, version string) (*envelope.Signed, error) {
	if !m.signedOnce {
		m.signedOnce = true
		m.signed, m.signErr = p.signer.Sign(m.envelopeMessage(version), p.baseURL())
	}

	return m.signed, m.signErr
}

func (m *message) envelopeMessage(version string) *envelope.Message {
	em := &envelope.Message{
		MessageID: m.id, TopicARN: m.topicARN, Subject: m.subject, Message: m.content(protocolSQS),
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

// delivery is one message on its way to one queue.
type delivery struct {
	sub  *Subscription
	call inproc.Call
	// dlq is the call that puts the message on the subscription's dead-letter
	// queue, nil without one.
	dlq *inproc.Call
}

// fanOut delivers msgs, published together to t, to every confirmed SQS
// subscription whose filter policy matches. The bodies are built with no
// lock held; the calls are queued under t's lock, after a check that each
// subscription is still the topic's, so a subscription removed, or a topic
// deleted, before the publish queues its calls gets none. A call queued before
// the removal answered is delivered.
func (p *Provider) fanOut(ctx context.Context, rc *request.Context, t *Topic, msgs []*message) {
	subs := t.subscribers()
	if len(subs) == 0 {
		return
	}

	version := t.attribute(signatureVersion)
	trace := traceOf(rc)
	out := make([]outgoing, len(msgs))

	for i, m := range msgs {
		out[i].message = m
	}

	var deliveries []delivery

	for i := range subs {
		s := &subs[i]

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

	p.queue(ctx, t, deliveries)
}

// deliveryOf is the SendMessage call that puts m on s's queue.
func (p *Provider) deliveryOf(s *subscriber, m *outgoing, version, trace string) (delivery, bool) {
	queue, ok := parseARN(s.endpoint)
	if !ok {
		slog.Error("SNS delivery skipped: unparseable queue ARN", "subscription", s.arn, "endpoint", s.endpoint)

		return delivery{}, false
	}

	body := m.content(protocolSQS)
	if !s.raw {
		signed, err := p.envelopeFor(m, version)
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

	d := delivery{sub: s.sub, call: sendCall(queue, s.endpoint, m.topicARN, &params)}

	if s.dlq != "" {
		if target, ok := parseARN(s.dlq); ok {
			params.trace = ""
			d.dlq = new(sendCall(target, s.dlq, s.arn, &params))
		}
	}

	return d, true
}

// queue queues each delivery whose subscription is still t's. A delivery the
// queue refuses goes to its dead-letter queue at once, after t's lock is let
// go.
func (p *Provider) queue(ctx context.Context, t *Topic, deliveries []delivery) {
	var refused []delivery

	t.mu.Lock()

	for _, d := range deliveries {
		if t.endpoints[d.sub.Endpoint] != d.sub {
			continue
		}

		if !p.enqueue(d) {
			refused = append(refused, d)
		}
	}

	t.mu.Unlock()

	for _, d := range refused {
		p.deadLetter(ctx, d.dlq, "the delivery queue is full")
	}
}

// enqueue queues d's call and reports whether it was taken. When SQS does
// not take the message, it goes to d's dead-letter queue.
func (p *Provider) enqueue(d delivery) bool {
	p.pending.add()

	queued := p.client.Enqueue(d.call, func(resp inproc.Response, err error) {
		defer p.pending.done()

		if err == nil && resp.OK() {
			return
		}

		slog.Info("Unable to forward SNS message to SQS", "queue", d.sub.Endpoint, "status", resp.Status, "error", err)

		if d.dlq == nil {
			p.dropped.Add(1)

			return
		}

		p.pending.add()

		if !p.client.Enqueue(*d.dlq, p.deadLetterDone) {
			p.pending.done()
			p.dropped.Add(1)
		}
	})
	if !queued {
		p.pending.done()
	}

	return queued
}

// deadLetterDone counts a message its dead-letter queue did not take.
func (p *Provider) deadLetterDone(resp inproc.Response, err error) {
	defer p.pending.done()

	if err != nil || !resp.OK() {
		slog.Info("Unable to send SNS message to its dead-letter queue", "status", resp.Status, "error", err)
		p.dropped.Add(1)
	}
}

// deadLetter sends a message the delivery queue refused to its dead-letter
// queue, on the caller's goroutine, since that queue is full; with none, the
// message is dropped and counted.
func (p *Provider) deadLetter(ctx context.Context, call *inproc.Call, reason string) {
	if call == nil {
		slog.Warn("SNS message dropped", "reason", reason)
		p.dropped.Add(1)

		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dlqSendTimeout)
	defer cancel()

	resp, err := p.client.Do(ctx, *call)
	if err != nil || !resp.OK() {
		slog.Warn("SNS message dropped: its dead-letter queue did not take it", "reason", reason,
			"status", resp.Status, "error", err)
		p.dropped.Add(1)
	}
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
// other.
func sendCall(queue arnParts, arn, source string, params *sendParams) inproc.Call {
	queueURL := queueScheme + "://sqs." + queue.region + "." + queueHost + "/" + queue.account + "/" + queue.resource

	b := make([]byte, 0, len(params.body)+len(queueURL)+callOverhead)
	b = append(b, `{"QueueUrl":`...)
	b = appendJSONString(b, queueURL)
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

	if params.trace != "" {
		b = append(b, `,"MessageSystemAttributes":{"`+traceAttribute+`":{"DataType":"String","StringValue":`...)
		b = appendJSONString(b, params.trace)
		b = append(b, "}}"...)
	}

	b = append(b, '}')

	meta := appendJSONString([]byte(`{"source_arn":`), source)
	meta = append(meta, `,"service_principal":"sns"}`...)

	return inproc.Call{
		Account: queue.account, Region: queue.region, Service: sqsService, Target: sqsSendMessage, Body: b,
		Header: http.Header{internalHeader: {string(meta)}}, Lane: arn,
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

// pending counts the deliveries queued and not yet answered, so Close can
// wait for them.
type pending struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

func (w *pending) add() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.n == 0 {
		w.idle = make(chan struct{})
	}

	w.n++
}

func (w *pending) done() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.n--
	if w.n == 0 {
		close(w.idle)
	}
}

// wait returns when no delivery is pending or ctx ends.
func (w *pending) wait(ctx context.Context) {
	w.mu.Lock()
	if w.n == 0 {
		w.mu.Unlock()

		return
	}

	idle := w.idle
	w.mu.Unlock()

	select {
	case <-idle:
	case <-ctx.Done():
	}
}
