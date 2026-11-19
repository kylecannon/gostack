package sns_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/require"
)

const (
	otherAccount = "111111111111"
	otherRegion  = "us-west-2"
	sqsHost      = "localhost.localstack.cloud:4566"
	waitFor      = 10 * time.Second

	attrFifoQueue = "FifoQueue"
	attrColor     = "color"
	valueRed      = "red"

	attrEntryName   = "MessageAttributes.entry.1.Name"
	attrEntryString = "MessageAttributes.entry.1.Value.StringValue"
	typeString      = "String"
	protocolSQS     = "sqs"
	codeNotFound    = "NotFound"
	msgNoTopic      = "Topic does not exist"
)

// stack is SNS and SQS behind one gateway, with SNS delivering through it, as
// in the process.
type stack struct {
	*client

	sns *sns.Provider
}

// newStack builds the stack. SNS's own calls to SQS go through wrap(gateway
// handler) when wrap is not nil.
func newStack(t *testing.T, wrap func(http.Handler) http.Handler) *stack {
	t.Helper()

	provider := sns.New(nil)
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{} }, func(string) {})

	t.Cleanup(queues.Close)

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{listenAny}} },
		[]service.Registration{provider.Registration(), queues.Registration()}, provider.MountProtocol, nil)
	require.NoError(t, err)

	if wrap == nil {
		provider.Bind(g.Handler())
	} else {
		provider.Bind(wrap(g.Handler()))
	}

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { provider.Close(context.WithoutCancel(t.Context())) })

	return &stack{client: &client{t: t, server: server}, sns: provider}
}

// sqsCall makes a json SQS call as acct in regionName and returns the decoded
// answer, requiring a 200.
func (s *stack) sqsCall(acct, regionName, operation string, params map[string]any) map[string]any {
	s.t.Helper()

	encoded, err := json.Marshal(params)
	require.NoError(s.t, err)

	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.server.URL+"/",
		bytes.NewReader(encoded))
	require.NoError(s.t, err)

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+acct+"/20250101/"+regionName+
		"/sqs/aws4_request, SignedHeaders=host, Signature=0")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+operation)

	resp, err := s.server.Client().Do(req)
	require.NoError(s.t, err)

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	require.Equal(s.t, http.StatusOK, resp.StatusCode, string(data))

	var out map[string]any
	if len(data) > 0 {
		require.NoError(s.t, json.Unmarshal(data, &out), string(data))
	}

	return out
}

// queue is an SQS queue of the stack.
type queue struct {
	s       *stack
	account string
	region  string
	url     string
	arn     string
}

// createQueue creates the queue name as acct in regionName.
func (s *stack) createQueue(acct, regionName, name string, attrs map[string]string) *queue {
	s.t.Helper()

	params := map[string]any{"QueueName": name}
	if attrs != nil {
		params["Attributes"] = attrs
	}

	out := s.sqsCall(acct, regionName, "CreateQueue", params)

	queueURL, ok := out["QueueUrl"].(string)
	require.True(s.t, ok, "CreateQueue answered %v", out)

	return &queue{
		s: s, account: acct, region: regionName, url: queueURL,
		arn: "arn:aws:sqs:" + regionName + ":" + acct + ":" + name,
	}
}

// sqsMessage is a received SQS message.
type sqsMessage struct {
	Body       string                       `json:"Body"`
	Attributes map[string]string            `json:"Attributes"`
	MsgAttrs   map[string]map[string]string `json:"MessageAttributes"`
	Receipt    string                       `json:"ReceiptHandle"`
}

// receive returns up to ten messages and deletes them, waiting up to wait
// seconds for one.
func (q *queue) receive(wait int) []sqsMessage {
	q.s.t.Helper()

	out := q.s.sqsCall(q.account, q.region, "ReceiveMessage", map[string]any{
		"QueueUrl": q.url, "MaxNumberOfMessages": 10, "WaitTimeSeconds": wait,
		"MessageAttributeNames": []string{"All"}, "MessageSystemAttributeNames": []string{"All"},
	})

	raw, err := json.Marshal(out["Messages"])
	require.NoError(q.s.t, err)

	var msgs []sqsMessage
	require.NoError(q.s.t, json.Unmarshal(raw, &msgs))

	if len(msgs) == 0 {
		return nil
	}

	entries := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		entries[i] = map[string]any{"Id": strconv.Itoa(i), "ReceiptHandle": m.Receipt}
	}

	q.s.sqsCall(q.account, q.region, "DeleteMessageBatch", map[string]any{"QueueUrl": q.url, "Entries": entries})

	return msgs
}

// await receives until n messages have arrived, failing after waitFor.
func (q *queue) await(n int) []sqsMessage {
	q.s.t.Helper()

	var got []sqsMessage

	deadline := time.Now().Add(waitFor)
	for len(got) < n && time.Now().Before(deadline) {
		got = append(got, q.receive(1)...)
	}

	require.Len(q.s.t, got, n, "messages on %s", q.url)

	return got
}

// subscribeQueue subscribes q to topic with the attribute name and value
// pairs attrs, and returns the subscription ARN.
func (s *stack) subscribeQueue(topic, endpoint string, attrs ...string) string {
	s.t.Helper()

	params := []string{paramTopicArn, topic, paramProtocol, protocolSQS, paramEndpoint, endpoint, paramReturn, textTrue}
	for i := 0; i+1 < len(attrs); i += 2 {
		n := strconv.Itoa(i/2 + 1)
		params = append(params, "Attributes.entry."+n+".key", attrs[i], "Attributes.entry."+n+".value", attrs[i+1])
	}

	return s.subscribe(params...)
}

// published is the answer of a Publish.
type publishedReply struct {
	MessageID      string `xml:"PublishResult>MessageId"`
	SequenceNumber string `xml:"PublishResult>SequenceNumber"`
}

func (s *stack) publish(params ...string) publishedReply {
	s.t.Helper()

	var out publishedReply

	s.ok(&out, "Publish", params...)

	return out
}

// awaitDropped waits until Dropped reports n.
func (s *stack) awaitDropped(n int64) {
	s.t.Helper()

	require.Eventually(s.t, func() bool { return s.sns.Dropped() == n }, waitFor, 5*time.Millisecond,
		"dropped %d", s.sns.Dropped())
}

// gate holds SNS's calls whose body names match until open is closed. A call
// whose context ends first is answered 503 without reaching SQS.
type gate struct {
	match   string
	open    chan struct{}
	entered chan struct{}
}

func newGate(match string) *gate {
	return &gate{match: match, open: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (g *gate) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))

		if strings.Contains(string(body), g.match) {
			select {
			case g.entered <- struct{}{}:
			default:
			}

			select {
			case <-g.open:
			case <-r.Context().Done():
				http.Error(w, "cancelled", http.StatusServiceUnavailable)

				return
			}
		}

		h.ServeHTTP(w, r)
	})
}

// awaitEntered waits for a held call.
func (g *gate) awaitEntered(t *testing.T) {
	t.Helper()

	select {
	case <-g.entered:
	case <-time.After(waitFor):
		t.Fatal("no call reached the gate")
	}
}
