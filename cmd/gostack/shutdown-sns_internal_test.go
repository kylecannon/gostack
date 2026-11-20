package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	snsAuth = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/sns/aws4_request, " +
		"SignedHeaders=host, Signature=0"
	headerContentType = "Content-Type"
	queueNameKey      = "QueueName"
)

var (
	topicARN     = regexp.MustCompile(`<TopicArn>([^<]+)</TopicArn>`)
	snsDropped   = regexp.MustCompile(`msg="SNS deliveries dropped" count=(\d+)`)
	queueARNBase = "arn:aws:sqs:us-east-1:000000000000:"
)

// snsOK makes an SNS query call on g and requires a 200.
func (g *running) snsOK(t *testing.T, action string, params url.Values) string {
	t.Helper()

	params.Set("Action", action)
	params.Set("Version", "2010-03-31")

	status, data := g.send(t, http.MethodPost, "/", params.Encode(), map[string]string{
		headerContentType: "application/x-www-form-urlencoded", "Authorization": snsAuth,
	})
	require.Equal(t, http.StatusOK, status, "%s: %s", action, data)

	return data
}

// snsTopicToQueues creates topic name and a queue for each of queues,
// subscribed with raw delivery, and returns the topic's ARN.
func (g *running) snsTopicToQueues(t *testing.T, name string, queues []string) string {
	t.Helper()

	created := topicARN.FindStringSubmatch(g.snsOK(t, "CreateTopic", url.Values{"Name": {name}}))
	require.Len(t, created, 2)

	for _, q := range queues {
		g.sqsOK(t, "CreateQueue", map[string]any{queueNameKey: q})
		g.snsOK(t, "Subscribe", url.Values{
			"TopicArn": {created[1]}, "Protocol": {"sqs"}, "Endpoint": {queueARNBase + q},
			"Attributes.entry.1.key": {"RawMessageDelivery"}, "Attributes.entry.1.value": {"true"},
		})
	}

	return created[1]
}

// publishBatch publishes ten messages to topic on g.
func (g *running) publishBatch(t *testing.T, topic, tag string) {
	t.Helper()

	params := url.Values{"TopicArn": {topic}}

	for i := 1; i <= 10; i++ {
		member := "PublishBatchRequestEntries.member." + strconv.Itoa(i)
		params.Set(member+".Id", "e"+strconv.Itoa(i))
		params.Set(member+".Message", tag+"-"+strconv.Itoa(i))
	}

	g.snsOK(t, "PublishBatch", params)
}

// messages is the number of messages in each of queues on g.
func (g *running) messages(t *testing.T, queues []string) int {
	t.Helper()

	total := 0

	for _, q := range queues {
		queueURL := g.sqsOK(t, "GetQueueUrl", map[string]any{queueNameKey: q})["QueueUrl"]
		got := g.sqsOK(t, "GetQueueAttributes", map[string]any{
			"QueueUrl": queueURL, "AttributeNames": []string{"ApproximateNumberOfMessages"},
		})

		attributes, _ := got["Attributes"].(map[string]any)
		text, _ := attributes["ApproximateNumberOfMessages"].(string)
		n, err := strconv.Atoi(text)
		require.NoError(t, err, "queue %s: %v", q, got)

		total += n
	}

	return total
}

// statusWriter records the status a handler answers with.
type statusWriter struct {
	http.ResponseWriter

	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // the handler's own writer
}

// Test_start_snsDeliversAcknowledgedPublishes: 16 publishers send 10
// PublishBatch of 10 each to a topic with 100 healthy queues, and every
// publish is answered. Then gostack stops; the deliveries still in SNS's
// lanes are held until the shutdown has begun and take a millisecond each
// after that, so they are left to the shutdown. It drains them before SNS
// closes: SQS took all 160,000 messages, and SNS logs none dropped. On
// 9960b2c, with no drain of the calls, Close dropped 18,802 of them.
func Test_start_snsDeliversAcknowledgedPublishes(t *testing.T) {
	const publishers, batches, subscribers = 16, 10, 100

	t.Setenv("GATEWAY_LISTEN", anyLoopbackPort)
	t.Setenv("SHUTDOWN_TIMEOUT", "120")

	log := captureLog(t)

	var (
		armed           atomic.Bool
		held, delivered atomic.Int64
		release         = make(chan struct{})
		previous        = callHandler
	)

	callHandler = func(gw http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Amz-Target") != sendMessageTarget {
				gw.ServeHTTP(w, r)

				return
			}

			if armed.Load() {
				held.Add(1)

				select {
				case <-release:
				case <-r.Context().Done():
				}

				select {
				case <-time.After(time.Millisecond):
				case <-r.Context().Done():
				}
			}

			recorded := &statusWriter{ResponseWriter: w}
			gw.ServeHTTP(recorded, r)

			if recorded.status == http.StatusOK {
				delivered.Add(1)
			}
		})
	}

	t.Cleanup(func() { callHandler = previous })

	first := startInProcess(t, log, 1)

	queues := make([]string, subscribers)
	for i := range queues {
		queues[i] = "q" + strconv.Itoa(i)
	}

	topic := first.snsTopicToQueues(t, "wide", queues)

	var wg sync.WaitGroup

	for p := range publishers {
		wg.Go(func() {
			for b := range batches {
				first.publishBatch(t, topic, "p"+strconv.Itoa(p)+"-b"+strconv.Itoa(b))
			}
		})
	}

	wg.Wait()
	armed.Store(true)

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		first.stop()
	}()

	require.Eventually(t, func() bool { return first.refusing(t) }, 10*time.Second, time.Millisecond,
		"the shutdown never began")
	close(release)
	<-stopped

	require.Positive(t, held.Load(), "every delivery was made before the stop; the test proves nothing")
	assert.NotRegexp(t, snsDropped, log.String())
	assert.Equal(t, int64(publishers*batches*10*subscribers), delivered.Load(), "held at the stop: %d", held.Load())
}

// Test_start_snsBacklogServedOrCut: 400 deliveries wait behind a slow queue,
// 10 ms a message, when gostack stops with a 2 s timeout. The drain serves
// what it can until its deadline and cuts the rest: the messages a new process
// finds on the queue and the deliveries SNS logs as dropped add up to 400.
func Test_start_snsBacklogServedOrCut(t *testing.T) {
	const backlog = 400

	t.Setenv("PERSIST_BASE_DIR", t.TempDir())
	t.Setenv("PERSIST_SQS", "1")
	t.Setenv("GATEWAY_LISTEN", anyLoopbackPort)
	t.Setenv("SHUTDOWN_TIMEOUT", "2")

	log := captureLog(t)

	previous := callHandler
	callHandler = func(gw http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))

			if r.Header.Get("X-Amz-Target") == sendMessageTarget && strings.Contains(string(body), "/slow") {
				select {
				case <-time.After(10 * time.Millisecond):
				case <-r.Context().Done():
					http.Error(w, "cut", http.StatusServiceUnavailable)

					return
				}
			}

			gw.ServeHTTP(w, r)
		})
	}

	t.Cleanup(func() { callHandler = previous })

	first := startInProcess(t, log, 1)
	topic := first.snsTopicToQueues(t, "slow-topic", []string{"slow"})

	for b := range backlog / 10 {
		first.publishBatch(t, topic, "b"+strconv.Itoa(b))
	}

	first.stop()

	callHandler = previous

	found := snsDropped.FindStringSubmatch(log.String())
	require.Len(t, found, 2, "SNS logged no dropped count:\n%s", log.String())

	dropped, err := strconv.Atoi(found[1])
	require.NoError(t, err)

	second := startInProcess(t, log, 2)
	served := second.messages(t, []string{"slow"})
	t.Logf("served %d, dropped %d", served, dropped)

	assert.Positive(t, dropped, "the drain's deadline cut nothing")
	assert.Positive(t, served)
	assert.Equal(t, backlog, served+dropped, "served %d, dropped %d", served, dropped)
}
