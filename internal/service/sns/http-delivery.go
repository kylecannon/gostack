package sns

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
)

const (
	// postTimeout bounds one HTTP delivery, redirects and the answer's body
	// included. LocalStack sets none (publisher.py HttpTopicPublisher._publish
	// calls requests.post without a timeout), so a hung endpoint holds one of
	// its ten executor threads for good; gostack cuts it here and the delivery
	// fails.
	postTimeout = 10 * time.Second
	// maxPosts is the most HTTP deliveries in flight at once, across every
	// endpoint. An endpoint has at most one, its lane's head, so one hung
	// endpoint holds one of them. LocalStack's executor runs ten threads
	// for every protocol together (publisher.py PublishDispatcher, num_thread).
	maxPosts = 64
	// maxAnswer is the most of an endpoint's answer read. The rest is not
	// read, and the connection is not reused.
	maxAnswer = 64 << 10
	// maxAnswerHeader is the most an endpoint's answer may have in its status
	// line and headers; an answer with more fails the delivery. net/http's
	// default is 10 MiB, which one send could be made to hold.
	maxAnswerHeader = 64 << 10
	// maxIdleConns is the most idle connections kept across every endpoint,
	// as many as sends in flight.
	maxIdleConns = maxPosts
	// idleConnTimeout is how long a connection to an endpoint is kept after
	// its last delivery.
	idleConnTimeout = 2 * time.Second

	// The headers LocalStack sends with every HTTP delivery
	// (HttpTopicPublisher._publish), and the ones requests adds that the AWS
	// snapshot shows (Connection; Accept is the one it adds that AWS does not
	// send).
	defaultContentType = "text/plain; charset=UTF-8"
	acceptEncoding     = "gzip,deflate"
	userAgent          = "Amazon Simple Notification Service Agent"
	keepAlive          = "keep-alive"

	headerMessageType  = "x-amz-sns-message-type"
	headerMessageID    = "x-amz-sns-message-id"
	headerTopicARN     = "x-amz-sns-topic-arn"
	headerSubscription = "x-amz-sns-subscription-arn"
	headerRawDelivery  = "x-amz-sns-rawdelivery"

	attrTopicDeliveryPolicy = "DeliveryPolicy"
)

// post is one HTTP delivery: a message of one type to one endpoint.
type post struct {
	url          string
	kind         envelope.Type
	messageID    string
	topicARN     string
	subscription string
	raw          bool
	contentType  string
	body         []byte
}

// header is the delivery's headers, as LocalStack sends them and in its
// casing: the x-amz-sns ones in lower case, which Header.Set would change.
// A SubscriptionConfirmation has no subscription ARN; the raw delivery mark
// and a delivery policy's content type are a Notification's only.
func (p *post) header() http.Header {
	contentType := defaultContentType
	if p.kind == envelope.Notification && p.contentType != "" {
		contentType = p.contentType
	}

	h := http.Header{
		"Content-Type":    {contentType},
		"Accept-Encoding": {acceptEncoding},
		"User-Agent":      {userAgent},
		"Connection":      {keepAlive},
		headerMessageType: {string(p.kind)},
		headerMessageID:   {p.messageID},
		headerTopicARN:    {p.topicARN},
	}

	if p.kind != envelope.SubscriptionConfirmation {
		h[headerSubscription] = []string{p.subscription}
	}

	if p.kind == envelope.Notification && p.raw {
		h[headerRawDelivery] = []string{valueTrue}
	}

	return h
}

// poster sends HTTP deliveries with a client of SNS's own. TLS certificates
// are not verified, as LocalStack's verify=False, and redirects are followed,
// as requests.post follows them, up to net/http's ten. A connection is kept
// for the endpoint's next delivery, and closed idleConnTimeout after its last,
// which ends the goroutines it holds.
type poster struct {
	client    *http.Client
	transport *http.Transport
	timeout   time.Duration
	// ctx ends when Close begins, which cuts the deliveries in flight.
	ctx    context.Context //nolint:containedctx // the lifetime of the deliveries, not a request's.
	cancel context.CancelFunc
	// running counts the goroutines of deliveries in flight, which Close
	// waits for.
	running sync.WaitGroup
}

func newPoster() *poster {
	ctx, cancel := context.WithCancel(context.Background())

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		//nolint:gosec // G402: LocalStack delivers with verify=False, and so does gostack (docs/compat.md).
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:           maxIdleConns,
		MaxIdleConnsPerHost:    maxIdleConns,
		IdleConnTimeout:        idleConnTimeout,
		MaxResponseHeaderBytes: maxAnswerHeader,
		DisableCompression:     true,
	}

	return &poster{
		client:    &http.Client{Transport: transport},
		transport: transport,
		timeout:   postTimeout,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// send makes one delivery and reports why it failed, nil when the endpoint
// took it: an answer below 400, as requests' raise_for_status accepts. Its
// context is the poster's, as a delivery outlives the publish that queued it.
//
//nolint:contextcheck,nolintlint // contextcheck reads this itself; golangci sees no issue to suppress.
func (ps *poster) send(p *post) error {
	ctx, cancel := context.WithTimeout(ps.ctx, ps.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(p.body))
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}

	req.Header = p.header()

	resp, err := ps.client.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	// The answer is read as requests reads it, before its status counts: a read
	// the timeout or Close cuts raises there, and fails the delivery here.
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("the endpoint answered %s", strconv.Itoa(resp.StatusCode))
	}

	return nil
}

// post hands l's head, an HTTP delivery, to a goroutine of its own, or to the
// back of the lanes waiting for one when maxPosts are in flight. It never
// blocks; called under ls.mu.
func (ls *lanes) post(l *lane) {
	if ls.posting >= maxPosts {
		ls.postWaiting = append(ls.postWaiting, l)

		return
	}

	ls.posting++
	ls.poster.running.Add(1)

	go ls.runPost(l, l.jobs[0].post)
}

// runPost makes one HTTP delivery, with no lock held, and answers it. A panic
// is logged, and one in the send fails the delivery, so the lane goes on.
func (ls *lanes) runPost(l *lane, p *post) {
	defer ls.poster.running.Done()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("SNS HTTP delivery panicked", "endpoint", p.url, "panic", r)
		}
	}()

	ls.posted(l, ls.poster.sendRecovered(p))
}

// sendRecovered is send, with a panic made its error.
func (ps *poster) sendRecovered(p *post) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("send panicked: %v", r)
		}
	}()

	return ps.send(p)
}

// posted is the answer to l's head, an HTTP delivery: the head leaves the
// lane, failed when err is not nil, and the next waiting HTTP delivery starts.
func (ls *lanes) posted(l *lane, err error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	ls.posting--

	if !ls.closed && len(ls.postWaiting) > 0 {
		next := ls.postWaiting[0]
		ls.postWaiting[0] = nil
		ls.postWaiting = ls.postWaiting[1:]
		ls.post(next)
	}

	var failure func() string
	if err != nil {
		failure = err.Error
	}

	ls.finish(l, failure)
}

// cutPosts cuts the HTTP deliveries in flight at once, each of which answers
// as cut, and waits until ctx ends for them to return, which they do as soon
// as the cut reaches them. Close calls it once nothing can start another.
func (ls *lanes) cutPosts(ctx context.Context) {
	ls.poster.cancel()

	done := make(chan struct{})

	go func() {
		ls.poster.running.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// httpDeliveryOf is the POST of m to s's endpoint: the signed envelope, or
// the message alone for raw delivery. Its dead-letter delivery carries the
// same body and no attributes, raw or not, as LocalStack's
// sns_error_to_dead_letter_queue gets only the body.
func (p *Provider) httpDeliveryOf(s *subscriber, m *outgoing, version string) (delivery, bool) {
	var body []byte

	if s.raw {
		body = []byte(m.content(s.protocol))
	} else {
		signed, err := p.envelopeFor(m, version, s.protocol)
		if err != nil {
			slog.Error("SNS delivery skipped: cannot sign the message", "subscription", s.arn, "error", err)

			return delivery{}, false
		}

		body = signed.Body(s.arn)
	}

	return delivery{
		sub: s.sub, call: endpointCall(s.endpoint),
		post: postOf(s, envelope.Notification, m.id, m.topicARN, body), dlq: deadLetterCall(s, body),
	}, true
}

// endpointCall is the call of an HTTP delivery: only its Lane, the endpoint, is
// set, which puts the delivery in the endpoint's lane. Each endpoint has its
// own lane, apart from every queue's, so a slow endpoint holds up no queue.
func endpointCall(endpoint string) inproc.Call {
	return inproc.Call{Lane: endpoint}
}

// deadLetterCall makes the SendMessage that puts body on s's dead-letter queue,
// nil without one. It holds body, which the delivery holds already, and builds
// the call, an escaped copy, only when the delivery fails.
func deadLetterCall(s *subscriber, body []byte) func() inproc.Call {
	if s.dlq == "" {
		return nil
	}

	if _, ok := parseARN(s.dlq); !ok {
		return nil
	}

	dlq, source := s.dlq, s.arn

	return func() inproc.Call {
		target, _ := parseARN(dlq) // it parsed when the delivery was built.
		call, _ := sendCall(target, dlq, source, &sendParams{body: string(body)})

		return call
	}
}

// postOf is the HTTP delivery of body, a message of type kind, to the
// subscription s of the topic topicARN.
func postOf(s *subscriber, kind envelope.Type, messageID, topicARN string, body []byte) *post {
	return &post{
		url: s.endpoint, kind: kind, messageID: messageID, topicARN: topicARN, subscription: s.arn,
		raw: s.raw, contentType: s.contentType, body: body,
	}
}

// contentType is the Content-Type a Notification to an HTTP subscription
// goes with, LocalStack's _get_content_type: the subscription's delivery
// policy's requestPolicy.headerContentType, else the topic's
// <protocol>.defaultRequestPolicy.headerContentType, else "" for the default.
// A policy that is not JSON names none.
func contentType(subscriptionPolicy, topicPolicy, protocol string) string {
	if subscriptionPolicy != "" {
		var doc struct {
			Request requestPolicy `json:"requestPolicy"`
		}

		if json.Unmarshal([]byte(subscriptionPolicy), &doc) == nil && doc.Request.ContentType != "" {
			return doc.Request.ContentType
		}
	}

	if topicPolicy == "" {
		return ""
	}

	var doc map[string]protocolPolicy

	if json.Unmarshal([]byte(topicPolicy), &doc) != nil {
		return ""
	}

	return doc[protocol].Default.ContentType
}

// requestPolicy is a delivery policy's request policy.
type requestPolicy struct {
	ContentType string `json:"headerContentType"`
}

// protocolPolicy is a topic delivery policy's member for one protocol.
type protocolPolicy struct {
	Default requestPolicy `json:"defaultRequestPolicy"`
}

// The messages Subscribe and Unsubscribe send an HTTP or HTTPS endpoint,
// as LocalStack words them.
const (
	subscribeText = "You have chosen to subscribe to the topic %s.\n" +
		"To confirm the subscription, visit the SubscribeURL included in this message."
	unsubscribeText = "You have chosen to deactivate subscription %s.\n" +
		"To cancel this operation and restore the subscription, visit the SubscribeURL included in this message."
)

// subscriptionMessage is the one place Subscribe and Unsubscribe hand an HTTP
// or HTTPS subscription its SubscriptionConfirmation or
// UnsubscribeConfirmation, with token in its SubscribeURL, as LocalStack's
// publish_to_topic_subscriber. It goes in the endpoint's lane, behind the
// deliveries already there, and is sent with no lock held. A
// SubscriptionConfirmation is signed with the topic's SignatureVersion, and
// one that fails goes to the subscription's dead-letter queue; an
// UnsubscribeConfirmation is signed with version 1 (LocalStack passes no topic
// attributes) and one that fails is dropped, as AWS does
// (test_dlq_external_http_endpoint).
func (p *Provider) subscriptionMessage(kind envelope.Type, sub *Subscription, token, version string) {
	sub.mu.Lock()
	s := subscriberOf(sub)
	sub.mu.Unlock()

	text := fmt.Sprintf(subscribeText, s.sub.TopicARN)
	if kind == envelope.UnsubscribeConfirmation {
		text = fmt.Sprintf(unsubscribeText, s.arn)
		version = ""
	}

	id := newUUID()

	body, err := p.Envelope(&envelope.Message{
		Type: kind, MessageID: id, TopicARN: s.sub.TopicARN, Token: token, Message: text,
		Timestamp: time.Now(), SignatureVersion: version,
	})
	if err != nil {
		slog.Error("SNS confirmation skipped: cannot sign the message", "subscription", s.arn, "error", err)

		return
	}

	j := &job{call: endpointCall(s.endpoint), post: postOf(&s, kind, id, s.sub.TopicARN, body)}
	if kind != envelope.UnsubscribeConfirmation {
		j.dlq = deadLetterCall(&s, body)
	}

	ls := &p.lanes

	ls.mu.Lock()
	added := ls.add(j)
	ls.mu.Unlock()

	if added {
		ls.dispatch([]*job{j})
	}
}
