package sns_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	typeConfirmation   = "SubscriptionConfirmation"
	typeUnsubscribe    = "UnsubscribeConfirmation"
	typeNotification   = "Notification"
	fieldType          = "Type"
	protocolHTTP       = "http"
	headerType         = "X-Amz-Sns-Message-Type"
	attrDeliveryPolicy = "DeliveryPolicy"
)

// hit is one request an endpoint got: its raw bytes up to the body, as they
// came over the wire, and its body.
type hit struct {
	head   string
	header http.Header
	body   string
}

func (h hit) envelope(t *testing.T) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.body), &out), h.body)

	return out
}

// field is the string member name of an envelope.
func field(t *testing.T, env map[string]any, name string) string {
	t.Helper()

	s, ok := env[name].(string)
	require.True(t, ok, "%s is not a string: %v", name, env[name])

	return s
}

// endpoint is an HTTP server SNS delivers to. It answers each request with
// answer, 200 when answer is nil, and passes it on to hits.
type endpoint struct {
	url    string
	hits   chan hit
	answer func(http.ResponseWriter, *http.Request)
	// wire, when not nil, may write the answer to a request itself, byte by
	// byte, and report that it did; the endpoint's usual answer is then not
	// sent.
	wire func(net.Conn, *http.Request) bool
	// open counts the connections the endpoint holds.
	open atomic.Int64
}

func newEndpoint(t *testing.T, answer func(http.ResponseWriter, *http.Request)) *endpoint {
	t.Helper()

	return newWireEndpoint(t, answer, nil)
}

func newWireEndpoint(
	t *testing.T, answer func(http.ResponseWriter, *http.Request), wire func(net.Conn, *http.Request) bool,
) *endpoint {
	t.Helper()

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	e := &endpoint{url: "http://" + ln.Addr().String() + "/sns", hits: make(chan hit, 2048), answer: answer, wire: wire}

	var wg sync.WaitGroup

	t.Cleanup(func() {
		_ = ln.Close()

		wg.Wait()
	})

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			wg.Go(func() { e.serve(conn) })
		}
	})

	return e
}

// serve reads requests off conn and keeps each one's raw head.
func (e *endpoint) serve(conn net.Conn) {
	e.open.Add(1)

	defer e.open.Add(-1)
	defer func() { _ = conn.Close() }()

	var raw bytes.Buffer

	r := bufio.NewReader(io.TeeReader(conn, &raw))

	for {
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}

		body, err := io.ReadAll(req.Body)
		if err != nil {
			return
		}

		head, _, _ := bytes.Cut(raw.Bytes(), []byte("\r\n\r\n"))
		raw.Reset()

		e.hits <- hit{head: string(head), header: req.Header, body: string(body)}

		if e.wire != nil && e.wire(conn, req) {
			continue
		}

		w := &answerWriter{header: http.Header{}, status: http.StatusOK}
		if e.answer != nil {
			e.answer(w, req)
		}

		_, err = io.WriteString(conn, "HTTP/1.1 "+strconv.Itoa(w.status)+" "+http.StatusText(w.status)+
			"\r\nContent-Length: 2\r\n\r\nok")
		if err != nil {
			return
		}
	}
}

// answerWriter takes the status an answer func sets.
type answerWriter struct {
	header http.Header
	status int
}

func (w *answerWriter) Header() http.Header       { return w.header }
func (*answerWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *answerWriter) WriteHeader(status int)    { w.status = status }

// next returns the next request the endpoint gets.
func (e *endpoint) next(t *testing.T) hit {
	t.Helper()

	select {
	case h := <-e.hits:
		return h
	case <-time.After(waitFor):
		require.FailNow(t, "no request reached the endpoint")

		return hit{}
	}
}

// none requires that no request reaches the endpoint for a moment.
func (e *endpoint) none(t *testing.T) {
	t.Helper()

	select {
	case h := <-e.hits:
		require.FailNow(t, "unexpected request", h.body)
	case <-time.After(200 * time.Millisecond):
	}
}

// subscribeHTTP subscribes e to topic with the attribute pairs attrs and
// returns the subscription ARN and the SubscriptionConfirmation it got.
func (s *stack) subscribeHTTP(topic string, e *endpoint, attrs ...string) (string, hit) {
	s.t.Helper()

	params := []string{paramTopicArn, topic, paramProtocol, protocolHTTP, paramEndpoint, e.url, paramReturn, textTrue}
	for i := 0; i+1 < len(attrs); i += 2 {
		n := strconv.Itoa(i/2 + 1)
		params = append(params, "Attributes.entry."+n+".key", attrs[i], "Attributes.entry."+n+".value", attrs[i+1])
	}

	arn := s.subscribe(params...)

	return arn, e.next(s.t)
}

// confirmHTTP subscribes e to topic and confirms the subscription.
func (s *stack) confirmHTTP(topic string, e *endpoint, attrs ...string) string {
	s.t.Helper()

	arn, confirmation := s.subscribeHTTP(topic, e, attrs...)
	token := field(s.t, confirmation.envelope(s.t), "Token")

	s.ok(nil, opConfirm, paramTopicArn, topic, "Token", token)

	return arn
}

// unsigned GETs an envelope URL against the stack, as a subscriber following
// it does: no signature, only the query.
func (s *stack) unsigned(link string) (int, string) {
	s.t.Helper()

	u, err := url.Parse(link)
	require.NoError(s.t, err)

	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, s.server.URL+"/?"+u.RawQuery, nil)
	require.NoError(s.t, err)

	resp, err := s.server.Client().Do(req)
	require.NoError(s.t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	return resp.StatusCode, string(body)
}

// headLines is the request head SNS sends: the request line, then the
// headers, each as it goes over the wire.
func headLines(host string, contentLength int, lines ...string) string {
	return strings.Join(append([]string{
		"POST /sns HTTP/1.1",
		"Host: " + host,
		"User-Agent: Amazon Simple Notification Service Agent",
		"Content-Length: " + strconv.Itoa(contentLength),
		"Accept-Encoding: gzip,deflate",
		"Connection: keep-alive",
	}, lines...), "\r\n")
}

func Test_Provider_HTTP_confirmationHeaders(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")

	arn := s.subscribe(paramTopicArn, topic, paramProtocol, protocolHTTP, paramEndpoint, e.url)
	assert.Equal(t, "pending confirmation", arn)

	h := e.next(t)
	env := h.envelope(t)
	host := strings.TrimPrefix(strings.TrimSuffix(e.url, "/sns"), "http://")

	assert.Equal(t, headLines(host, len(h.body),
		"Content-Type: text/plain; charset=UTF-8",
		"x-amz-sns-message-id: "+field(t, env, "MessageId"),
		"x-amz-sns-message-type: SubscriptionConfirmation",
		"x-amz-sns-topic-arn: "+topic,
	), h.head)

	assert.Equal(t, typeConfirmation, env[fieldType])
	assert.Equal(t, "You have chosen to subscribe to the topic "+topic+
		".\nTo confirm the subscription, visit the SubscribeURL included in this message.", env["Message"])
	assert.Equal(t, "1", env["SignatureVersion"])
	assert.NotEmpty(t, env["Signature"])

	// The SubscribeURL confirms when followed, unsigned.
	status, body := s.unsigned(field(t, env, "SubscribeURL"))
	require.Equal(t, http.StatusOK, status, body)

	var confirmed struct {
		ARN string `xml:"ConfirmSubscriptionResult>SubscriptionArn"`
	}

	require.NoError(t, xml.Unmarshal([]byte(body), &confirmed))
	assert.True(t, strings.HasPrefix(confirmed.ARN, topic+":"), confirmed.ARN)
	assert.Equal(t, "false", s.subAttributes(confirmed.ARN)["PendingConfirmation"])
}

func Test_Provider_HTTP_pendingGetsNoNotification(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	s.subscribeHTTP(topic, e)

	s.publish(paramTopicArn, topic, paramMessage, "not yet")
	e.none(t)
}

func Test_Provider_HTTP_notificationHeaders(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	arn := s.confirmHTTP(topic, e)

	params := append([]string{paramTopicArn, topic, paramMessage, "hi there", paramSubject, "subj"},
		attr(1, attrColor, valueRed)...)
	out := s.publish(params...)

	h := e.next(t)
	host := strings.TrimPrefix(strings.TrimSuffix(e.url, "/sns"), "http://")

	assert.Equal(t, headLines(host, len(h.body),
		"Content-Type: text/plain; charset=UTF-8",
		"x-amz-sns-message-id: "+out.MessageID,
		"x-amz-sns-message-type: Notification",
		"x-amz-sns-subscription-arn: "+arn,
		"x-amz-sns-topic-arn: "+topic,
	), h.head)

	env := parseEnvelope(t, h.body)
	assert.Equal(t, typeNotification, env.Type)
	assert.Equal(t, "hi there", env.Message)
	assert.Equal(t, "subj", env.Subject)
	assert.Equal(t, map[string]string{fieldType: typeString, "Value": valueRed}, env.MessageAttributes[attrColor])
	assert.Equal(t, "http://localhost.localstack.cloud:4566/?Action=Unsubscribe&SubscriptionArn="+arn,
		env.UnsubscribeURL)
}

func Test_Provider_HTTP_rawWithContentType(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	arn := s.confirmHTTP(topic, e, attrRaw, textTrue)
	s.ok(nil, opSetAttrs, paramSubArn, arn, "AttributeName", attrDeliveryPolicy,
		"AttributeValue", `{"requestPolicy":{"headerContentType":"text/csv"}}`)

	out := s.publish(append([]string{paramTopicArn, topic, paramMessage, "a,b"}, attr(1, attrColor, valueRed)...)...)

	h := e.next(t)
	host := strings.TrimPrefix(strings.TrimSuffix(e.url, "/sns"), "http://")

	assert.Equal(t, headLines(host, 3,
		"Content-Type: text/csv",
		"x-amz-sns-message-id: "+out.MessageID,
		"x-amz-sns-message-type: Notification",
		"x-amz-sns-rawdelivery: true",
		"x-amz-sns-subscription-arn: "+arn,
		"x-amz-sns-topic-arn: "+topic,
	), h.head)
	assert.Equal(t, "a,b", h.body)
}

func Test_Provider_HTTP_topicContentType(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	s.confirmHTTP(topic, e)
	s.ok(nil, "SetTopicAttributes", paramTopicArn, topic, "AttributeName", attrDeliveryPolicy,
		"AttributeValue", `{"http":{"defaultRequestPolicy":{"headerContentType":"application/json"}}}`)

	s.publish(paramTopicArn, topic, paramMessage, "m")

	assert.Equal(t, "application/json", e.next(t).header.Get("Content-Type"))
}

func Test_Provider_HTTP_jsonStructure(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	s.confirmHTTP(topic, e)
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn)

	s.publish(paramTopicArn, topic, "MessageStructure", "json",
		paramMessage, `{"default":"for all","http":"for http"}`)

	assert.Equal(t, "for http", parseEnvelope(t, e.next(t).body).Message)
	assert.Equal(t, "for all", parseEnvelope(t, q.await(1)[0].Body).Message)
}

func Test_Provider_HTTP_unsubscribe(t *testing.T) {
	s := newStack(t, nil)
	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	arn := s.confirmHTTP(topic, e)

	s.publish(paramTopicArn, topic, paramMessage, "m")
	env := parseEnvelope(t, e.next(t).body)

	// The UnsubscribeURL unsubscribes when followed, unsigned.
	status, body := s.unsigned(env.UnsubscribeURL)
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, "<UnsubscribeResponse")

	h := e.next(t)
	host := strings.TrimPrefix(strings.TrimSuffix(e.url, "/sns"), "http://")
	got := h.envelope(t)

	assert.Equal(t, headLines(host, len(h.body),
		"Content-Type: text/plain; charset=UTF-8",
		"x-amz-sns-message-id: "+field(t, got, "MessageId"),
		"x-amz-sns-message-type: UnsubscribeConfirmation",
		"x-amz-sns-subscription-arn: "+arn,
		"x-amz-sns-topic-arn: "+topic,
	), h.head)
	assert.Equal(t, typeUnsubscribe, got[fieldType])
	assert.Equal(t, "You have chosen to deactivate subscription "+arn+".\nTo cancel this operation and "+
		"restore the subscription, visit the SubscribeURL included in this message.", got["Message"])
	assert.Contains(t, got["SubscribeURL"], "/?Action=ConfirmSubscription&TopicArn="+topic+"&Token=")

	s.publish(paramTopicArn, topic, paramMessage, "gone")
	e.none(t)
}

func Test_Provider_HTTP_failureToDeadLetter(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run("raw="+strconv.FormatBool(raw), func(t *testing.T) {
			s := newStack(t, nil)
			e := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(headerType) == typeNotification {
					w.WriteHeader(http.StatusInternalServerError)
				}
			})
			topic := s.createTopic(paramName, "t")
			dlq := s.createQueue(account, region, "dlq", nil)

			attrs := []string{attrRedrive, `{"deadLetterTargetArn":"` + dlq.arn + `"}`}
			if raw {
				attrs = append(attrs, attrRaw, textTrue)
			}

			s.confirmHTTP(topic, e, attrs...)
			out := s.publish(append([]string{paramTopicArn, topic, paramMessage, "lost"},
				attr(1, attrColor, valueRed)...)...)

			sent := e.next(t)
			msg := dlq.await(1)[0]

			assert.Equal(t, sent.body, msg.Body, "the dead-letter queue gets the body the endpoint refused")
			assert.Empty(t, msg.MsgAttrs, "no attributes, as LocalStack's sns_error_to_dead_letter_queue")

			if !raw {
				assert.Equal(t, out.MessageID, parseEnvelope(t, msg.Body).MessageID)
			}

			assert.Zero(t, s.sns.Dropped())
		})
	}
}

func Test_Provider_HTTP_refusedToDeadLetter(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	dlq := s.createQueue(account, region, "dlq", nil)

	// A URL that names a port nobody listens on.
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	closed := "http://" + ln.Addr().String() + "/gone"
	require.NoError(t, ln.Close())

	gone := s.subscribeHTTPTo(topic, closed, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)

	assert.Equal(t, typeConfirmation, parseEnvelope(t, dlq.await(1)[0].Body).Type,
		"a SubscriptionConfirmation that fails is dead-lettered")

	// An unsubscribe that cannot be delivered is dropped and counted.
	s.ok(nil, opUnsub, paramSubArn, gone)
	s.awaitDropped(1)
	assert.Empty(t, dlq.receive(0))
}

// subscribeHTTPTo subscribes the URL link to topic, which no endpoint of the
// test serves.
func (s *stack) subscribeHTTPTo(topic, link string, attrs ...string) string {
	s.t.Helper()

	params := []string{paramTopicArn, topic, paramProtocol, protocolHTTP, paramEndpoint, link, paramReturn, textTrue}
	for i := 0; i+1 < len(attrs); i += 2 {
		n := strconv.Itoa(i/2 + 1)
		params = append(params, "Attributes.entry."+n+".key", attrs[i], "Attributes.entry."+n+".value", attrs[i+1])
	}

	return s.subscribe(params...)
}

func Test_Provider_HTTP_hungEndpointTimesOut(t *testing.T) {
	s := newStack(t, nil)
	s.sns.SetPostTimeout(300 * time.Millisecond)

	release := make(chan struct{})
	e := newEndpoint(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerType) == typeNotification {
			<-release
		}
	})
	// After the endpoint's cleanup in order, so before it in time.
	t.Cleanup(func() { close(release) })

	topic := s.createTopic(paramName, "t")
	dlq := s.createQueue(account, region, "dlq", nil)
	s.confirmHTTP(topic, e, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)

	out := s.publish(paramTopicArn, topic, paramMessage, "hung")

	assert.Equal(t, out.MessageID, parseEnvelope(t, dlq.await(1)[0].Body).MessageID,
		"the delivery the endpoint never answered reached the dead-letter queue")
	assert.Zero(t, s.sns.Dropped())
}

func Test_Provider_HTTP_slowEndpointDoesNotDelayQueue(t *testing.T) {
	const n = 1000

	s := newStack(t, nil)
	// The endpoint holds the first notification for the whole publish loop.
	// postTimeout would cut it when that loop runs past 10 s on a loaded
	// machine, fail it, and send the next while the endpoint's handler still
	// blocks: two in its hits, though the lane kept one at a time.
	s.sns.SetPostTimeout(time.Hour)

	release := make(chan struct{})
	e := newEndpoint(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerType) == typeNotification {
			<-release
		}
	})
	// After the endpoint's cleanup in order, so before it in time: a failed
	// assertion still lets the held delivery go.
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)

	topic := s.createTopic(paramName, "t")
	s.confirmHTTP(topic, e)

	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	for i := range n {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	// Every message reaches the queue while the endpoint still holds the
	// first of its thousand.
	got := q.await(n)
	assert.Len(t, got, n)
	assert.Len(t, e.hits, 1, "the endpoint has one delivery at a time")

	let()

	for range n {
		e.next(t)
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
	defer cancel()

	s.sns.AwaitDeliveries(ctx)
	require.NoError(t, ctx.Err())
	assert.Zero(t, s.sns.Dropped())
}

func Test_Provider_HTTP_inFlightBound(t *testing.T) {
	const endpoints = sns.MaxPosts + 6

	s := newStack(t, nil)

	var inFlight, peak atomic.Int64

	release := make(chan struct{})
	e := newEndpoint(t, func(http.ResponseWriter, *http.Request) {
		now := inFlight.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}

		<-release
		inFlight.Add(-1)
	})

	topic := s.createTopic(paramName, "t")

	// Each subscription is its own endpoint, and its confirmation its own
	// delivery in flight.
	for i := range endpoints {
		s.subscribe(paramTopicArn, topic, paramProtocol, protocolHTTP, paramEndpoint, e.url+"/"+strconv.Itoa(i))
	}

	for range sns.MaxPosts {
		e.next(t)
	}

	e.none(t)
	assert.Equal(t, int64(sns.MaxPosts), peak.Load())

	close(release)

	for range endpoints - sns.MaxPosts {
		e.next(t)
	}

	assert.LessOrEqual(t, peak.Load(), int64(sns.MaxPosts))
}

// Test_Provider_HTTP_goroutines: no goroutine is left once the HTTP
// deliveries are made and their connections have been idle for the timeout.
func Test_Provider_HTTP_goroutines(t *testing.T) {
	s := newStack(t, nil)
	s.sns.SetIdleConnTimeout(100 * time.Millisecond)

	e := newEndpoint(t, nil)
	topic := s.createTopic(paramName, "t")
	before := settledGoroutines(t, s, -1)

	s.confirmHTTP(topic, e)

	for i := range 50 {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	for range 50 {
		e.next(t)
	}

	s.sns.AwaitDeliveries(t.Context())
	settledGoroutines(t, s, before)
}

// closeBeforeEndpoint closes s's SNS when the test ends, before the cleanups
// registered earlier, which are the endpoints': an endpoint's cleanup waits for
// its connections to close, and SNS keeps the idle ones for a minute otherwise.
func closeBeforeEndpoint(t *testing.T, s *stack) {
	t.Helper()

	t.Cleanup(func() { s.sns.Close(context.WithoutCancel(t.Context())) })
}

// hungNotifications is an endpoint that holds every Notification and answers
// the rest at once. Its release is closed at the test's end, after the
// endpoint's own cleanup in order, so before it in time.
func hungNotifications(t *testing.T) *endpoint {
	t.Helper()

	release := make(chan struct{})
	e := newEndpoint(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerType) == typeNotification {
			<-release
		}
	})

	t.Cleanup(func() { close(release) })

	return e
}

// Test_Provider_Close_cutsHTTPAtOnce: Close does not wait for an HTTP delivery
// in flight, even with a context that never ends: it cuts it, and the one
// behind it, and counts both.
func Test_Provider_Close_cutsHTTPAtOnce(t *testing.T) {
	s := newStack(t, nil)
	e := hungNotifications(t)
	topic := s.createTopic(paramName, "t")
	s.confirmHTTP(topic, e)

	s.publish(paramTopicArn, topic, paramMessage, "in flight")
	s.publish(paramTopicArn, topic, paramMessage, "waiting")
	e.next(t)

	closed := make(chan struct{})

	go func() {
		s.sns.Close(context.WithoutCancel(t.Context()))
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Close waited for the HTTP delivery in flight")
	}

	assert.Equal(t, int64(2), s.sns.Dropped(), "the delivery in flight and the one behind it are cut")
}

// drainStack is a stack whose calls go through a hub, with a gate in front of
// the gateway that holds the calls naming the queue "held".
func drainStack(t *testing.T) (*stack, *inproc.Hub, *gate) {
	t.Helper()

	var hub *inproc.Hub

	g := newGate("/held")
	s := newStack(t, func(h http.Handler) http.Handler {
		hub = inproc.NewHub(g.wrap(h))

		return hub
	})

	return s, hub, g
}

// hungTopic is a topic of s with an HTTP subscriber that never answers a
// Notification.
func hungTopic(t *testing.T, s *stack) (string, *endpoint) {
	t.Helper()

	e := hungNotifications(t)

	closeBeforeEndpoint(t, s)

	topic := s.createTopic(paramName, "t")

	s.confirmHTTP(topic, e)

	return topic, e
}

// Test_Provider_HTTP_drainDoesNotWait: the hub's drain does not wait for an
// HTTP delivery, which changes no state that is saved and may hang for the
// delivery timeout: with one in flight to an endpoint that never answers, and
// none held for a queue, Drain returns at once, with a context that does not
// end, and cuts no call.
func Test_Provider_HTTP_drainDoesNotWait(t *testing.T) {
	s, hub, _ := drainStack(t)
	topic, e := hungTopic(t, s)

	s.publish(paramTopicArn, topic, paramMessage, "in flight")
	e.next(t)

	drained := make(chan int, 1)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	go func() { drained <- hub.Drain(ctx) }()

	select {
	case cut := <-drained:
		assert.Zero(t, cut, "calls cut")
	case <-time.After(waitFor):
		cancel()
		require.FailNow(t, "Drain waited for the HTTP delivery in flight")
	}
}

// Test_Provider_HTTP_drainWaitsForQueue: a delivery to an SQS queue is still
// waited for, with an HTTP delivery hung beside it. Drain does not return
// while the queue's call is held, returns once it is served, and the message
// is on the queue; the hung HTTP delivery adds nothing.
func Test_Provider_HTTP_drainWaitsForQueue(t *testing.T) {
	s, hub, g := drainStack(t)
	topic, e := hungTopic(t, s)
	open := opener(t, g)
	held := s.createQueue(account, region, "held", nil)

	s.subscribeQueue(topic, held.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "both")
	e.next(t)
	g.awaitEntered(t)

	drained := make(chan int, 1)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	go func() { drained <- hub.Drain(ctx) }()

	select {
	case <-drained:
		require.FailNow(t, "Drain returned with the queue's delivery held")
	case <-time.After(50 * time.Millisecond):
	}

	open()

	select {
	case cut := <-drained:
		assert.Zero(t, cut, "calls cut")
	case <-time.After(waitFor):
		cancel()
		require.FailNow(t, "Drain waited for the HTTP delivery in flight")
	}

	assert.Equal(t, 1, s.depth(held), "the queue's delivery arrived")
}

// stallingBody answers a Notification with a 200 and the start of a body that
// never ends, until release is closed.
func stallingBody(release <-chan struct{}) func(net.Conn, *http.Request) bool {
	return func(conn net.Conn, r *http.Request) bool {
		if r.Header.Get(headerType) != typeNotification {
			return false
		}

		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nab")
		<-release

		return true
	}
}

// Test_Provider_HTTP_bodyReadCutByTimeout: a 200 whose body the timeout cuts
// fails the delivery, as the read timeout of LocalStack's requests.post
// raises, and the message goes to the dead-letter queue.
func Test_Provider_HTTP_bodyReadCutByTimeout(t *testing.T) {
	s := newStack(t, nil)
	s.sns.SetPostTimeout(300 * time.Millisecond)

	release := make(chan struct{})
	e := newWireEndpoint(t, nil, stallingBody(release))

	t.Cleanup(func() { close(release) })

	topic := s.createTopic(paramName, "t")
	dlq := s.createQueue(account, region, "dlq", nil)
	s.confirmHTTP(topic, e, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)

	out := s.publish(paramTopicArn, topic, paramMessage, "cut")

	assert.Equal(t, out.MessageID, parseEnvelope(t, dlq.await(1)[0].Body).MessageID,
		"the delivery whose answer never finished reached the dead-letter queue")
	assert.Zero(t, s.sns.Dropped())
}

// padded answers a Notification with a 200 and a header of pad bytes.
func padded(pad int) func(net.Conn, *http.Request) bool {
	return func(conn net.Conn, r *http.Request) bool {
		if r.Header.Get(headerType) != typeNotification {
			return false
		}

		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nX-Pad: "+strings.Repeat("a", pad)+"\r\nContent-Length: 2\r\n\r\nok")

		return true
	}
}

// Test_Provider_HTTP_answerHeaderBound: an answer whose status line and
// headers pass 64 KiB fails the delivery; one just under it is taken.
func Test_Provider_HTTP_answerHeaderBound(t *testing.T) {
	const limit = 64 << 10

	for _, tc := range []struct {
		name   string
		pad    int
		failed bool
	}{
		{"over", limit, true},
		{"under", limit - 1<<10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStack(t, nil)
			e := newWireEndpoint(t, nil, padded(tc.pad))
			closeBeforeEndpoint(t, s)

			topic := s.createTopic(paramName, "t")
			dlq := s.createQueue(account, region, "dlq", nil)
			s.confirmHTTP(topic, e, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)

			s.publish(paramTopicArn, topic, paramMessage, "big header")
			e.next(t)

			if tc.failed {
				assert.Len(t, dlq.await(1), 1, "the delivery reached the dead-letter queue")
			} else {
				s.sns.AwaitDeliveries(t.Context())
				assert.Empty(t, dlq.receive(0), "an answer under the bound is taken")
			}

			assert.Zero(t, s.sns.Dropped())
		})
	}
}

// Test_Provider_HTTP_idleConnectionBound: connections kept idle are bounded
// across endpoints. maxPosts+6 endpoints each get one delivery; the oldest idle
// connections are closed past maxPosts, so no more than maxPosts stay open.
func Test_Provider_HTTP_idleConnectionBound(t *testing.T) {
	const endpoints = sns.MaxPosts + 6

	s := newStack(t, nil)
	s.sns.SetIdleConnTimeout(time.Minute)

	topic := s.createTopic(paramName, "t")
	served := make([]*endpoint, endpoints)

	for i := range served {
		served[i] = newEndpoint(t, nil)
		s.subscribeHTTP(topic, served[i])
	}

	closeBeforeEndpoint(t, s)
	s.sns.AwaitDeliveries(t.Context())

	open := func() int64 {
		var n int64
		for _, e := range served {
			n += e.open.Load()
		}

		return n
	}

	assert.Eventually(t, func() bool { return open() == sns.MaxPosts }, waitFor, 10*time.Millisecond,
		"connections open: %d", open())
	assert.Never(t, func() bool { return open() > sns.MaxPosts }, 300*time.Millisecond, 10*time.Millisecond)
}

// Test_Provider_HTTP_idleConnectionsPerHost: one host keeps as many idle
// connections as it had sends in flight, up to the bound, not net/http's
// default of two.
func Test_Provider_HTTP_idleConnectionsPerHost(t *testing.T) {
	const paths = 8

	s := newStack(t, nil)
	s.sns.SetIdleConnTimeout(time.Minute)

	var (
		inFlight atomic.Int64
		once     sync.Once
	)

	release := make(chan struct{})
	e := newEndpoint(t, func(http.ResponseWriter, *http.Request) {
		inFlight.Add(1)
		<-release
	})

	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	closeBeforeEndpoint(t, s)

	topic := s.createTopic(paramName, "t")

	// Each path is its own lane, so the confirmations are in flight together.
	for i := range paths {
		s.subscribe(paramTopicArn, topic, paramProtocol, protocolHTTP, paramEndpoint, e.url+"/"+strconv.Itoa(i))
	}

	require.Eventually(t, func() bool { return inFlight.Load() == paths }, waitFor, 5*time.Millisecond)
	finish()
	s.sns.AwaitDeliveries(t.Context())

	assert.Eventually(t, func() bool { return e.open.Load() == paths }, waitFor, 10*time.Millisecond,
		"connections open: %d", e.open.Load())
	assert.Never(t, func() bool { return e.open.Load() < paths }, 300*time.Millisecond, 10*time.Millisecond)
}
