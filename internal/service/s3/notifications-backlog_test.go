package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
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

// The notification queue has no bound on the events waiting: an event is never
// dropped because others wait, as LocalStack's notifier executor never drops
// one. A PUT never waits on delivery either.

const (
	slowDirect = "slow-direct"
	slowSub    = "slow-sub"
	slowTopic  = "slow-topic"

	// backlogPuts is past inproc.MaxQueued, which the queue used to refuse at.
	backlogPuts = inproc.MaxQueued + 200
)

// logSink is slog's default handler's output for a test.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}

func captureNotifyLog(t *testing.T) *logSink {
	t.Helper()

	sink := &logSink{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(sink, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return sink
}

// putMany puts count one-byte objects into the bucket from 16 clients at once
// and returns how many were answered 200.
func (n *notifyStack) putMany(bucket string, count int) int {
	n.t.Helper()

	const workers = 16

	transport := &http.Transport{MaxIdleConnsPerHost: workers}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}

	var (
		next, ok atomic.Int64
		wg       sync.WaitGroup
	)

	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= count {
					return
				}

				req, err := http.NewRequestWithContext(n.t.Context(), http.MethodPut,
					n.url+"/"+bucket+"/key-"+strconv.Itoa(i), strings.NewReader("x"))
				if err != nil {
					return
				}

				req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, "+
					"SignedHeaders=host, Signature=0")

				resp, err := client.Do(req)
				if err != nil {
					continue
				}

				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode == http.StatusOK {
					ok.Add(1)
				}
			}
		})
	}

	wg.Wait()

	return int(ok.Load())
}

// depthOf is the number of messages on the queue.
func (n *notifyStack) depthOf(queue string) int {
	n.t.Helper()

	status, out := n.sqsCall("GetQueueAttributes", map[string]any{
		fieldQueueURL:    "http://sqs.us-east-1.localhost.localstack.cloud:4566/" + notifyAcct + "/" + queue,
		"AttributeNames": []string{"ApproximateNumberOfMessages"},
	})
	require.Equal(n.t, http.StatusOK, status, out)

	attrs, _ := out["Attributes"].(map[string]any)
	text, _ := attrs["ApproximateNumberOfMessages"].(string)

	depth, err := strconv.Atoi(text)
	require.NoError(n.t, err, out)

	return depth
}

// blockSlow holds, until release closes, every notification S3 sends to the slow
// queue and the slow topic, and answers the others at once. Only the test lets
// a held call go: neither its context ending, which inproc's 30 s call timeout
// and the shutdown's cut both do, nor the time the PUTs take, answers it, so a
// slow PUT phase cannot turn the head event of a held lane into a 500. A held
// call goes on to the service once let go, as a call that had never timed out.
func blockSlow(release <-chan struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			toSlowQueue := r.Header.Get("X-Amz-Target") == "AmazonSQS.SendMessage" && strings.Contains(string(body), slowDirect)
			toSlowTopic := strings.Contains(string(body), "Action=Publish") && strings.Contains(string(body), slowTopic)
			slow := toSlowQueue || toSlowTopic

			if slow {
				<-release

				r = r.WithContext(context.WithoutCancel(r.Context()))
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		})
	}
}

// setupBacklog creates the bucket, the fast queue the bucket's events go to, and
// the slow queue and the slow topic, with a queue subscribed to the topic, and
// puts a configuration that sends every event to all three destinations.
func (n *notifyStack) setupBacklog() {
	n.t.Helper()

	n.setup()

	for _, name := range []string{slowDirect, slowSub} {
		status, out := n.sqsCall("CreateQueue", map[string]any{fieldQueueName: name})
		require.Equal(n.t, http.StatusOK, status, out)
	}

	slowTopicARN := "arn:aws:sns:us-east-1:" + notifyAcct + ":" + slowTopic

	status, body := n.snsCall(snsForm("CreateTopic", url.Values{fieldTopicName: {slowTopic}}))
	require.Equal(n.t, http.StatusOK, status, body)

	subscription := url.Values{
		"TopicArn": {slowTopicARN}, "Protocol": {"sqs"},
		"Endpoint": {"arn:aws:sqs:us-east-1:" + notifyAcct + ":" + slowSub},
	}
	maps.Copy(subscription, rawDelivery)

	status, body = n.snsCall(snsForm("Subscribe", subscription))
	require.Equal(n.t, http.StatusOK, status, body)

	config := `<NotificationConfiguration>` +
		`<QueueConfiguration><Id>slow</Id><Queue>arn:aws:sqs:us-east-1:` + notifyAcct + `:` + slowDirect + `</Queue>` +
		createdEvents + `</QueueConfiguration>` +
		`<TopicConfiguration><Id>topic</Id><Topic>` + slowTopicARN + `</Topic>` + createdEvents + `</TopicConfiguration>` +
		`<QueueConfiguration><Id>fast</Id><Queue>` + notifyARN + `</Queue>` + createdEvents + `</QueueConfiguration>` +
		`</NotificationConfiguration>`

	put := n.s3Call(http.MethodPut, notificationQuery, config, skipDestinationCheck)
	require.Equal(n.t, http.StatusOK, put.status, string(put.body))
}

// A destination that is slow holds up no other, and no event is dropped for the
// number that wait: more than inproc.MaxQueued events wait for the slow queue
// and the slow topic, the fast queue gets all of its own meanwhile, and when the
// slow ones are let go they get theirs. The PUTs never waited.
func TestNotificationsAreNeverDroppedForTheNumberWaiting(t *testing.T) {
	log := captureNotifyLog(t)
	release := make(chan struct{})

	provider, serverURL := serveNotifyGateway(t, blockSlow(release), sns.New(nil, nil))
	n := &notifyStack{t: t, url: serverURL, s3: provider}

	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	n.setupBacklog()

	// A PUT that waited for delivery would never return: the two slow
	// destinations are held until the test lets them go, after this call.
	require.Equal(t, backlogPuts, n.putMany(strings.TrimPrefix(notifyBucket, "/"), backlogPuts),
		"every PUT is answered 200 while two destinations hold %d events each", backlogPuts)

	// The waits below are guards against a hang, not bounds on how long delivery
	// takes: each event is delivered as soon as its destination is free.
	require.Eventually(t, func() bool { return n.depthOf(notifyQueue) == backlogPuts },
		60*time.Second, 10*time.Millisecond, "the fast queue gets every event while the other two destinations are held")
	assert.Zero(t, n.depthOf(slowDirect))
	assert.Zero(t, n.depthOf(slowSub))

	close(release)

	require.Eventually(t, func() bool { return n.depthOf(slowDirect) == backlogPuts }, 60*time.Second, 10*time.Millisecond,
		"the slow queue got every event once let go")
	require.Eventually(t, func() bool { return n.depthOf(slowSub) == backlogPuts }, 60*time.Second, 10*time.Millisecond,
		"the queue subscribed to the slow topic got every event once let go")

	assert.NotContains(t, log.String(), "notification dropped", "no event was dropped")
}

// Past the shutdown's drain a backlog is cut, and the count of the events lost
// is logged, none of them dropped silently on the way to the shutdown.
func TestNotificationBacklogIsCountedWhenTheDrainCutsIt(t *testing.T) {
	log := captureNotifyLog(t)
	release := make(chan struct{})

	var hub *inproc.Hub

	n := newNotifyStackThrough(t, func(next http.Handler) http.Handler {
		hub = inproc.NewHub(blockSlow(release)(next))

		return hub
	})

	t.Cleanup(func() { close(release) })

	n.setup()

	status, out := n.sqsCall("CreateQueue", map[string]any{fieldQueueName: slowDirect})
	require.Equal(t, http.StatusOK, status, out)

	config := `<NotificationConfiguration><QueueConfiguration><Id>slow</Id><Queue>arn:aws:sqs:us-east-1:` + notifyAcct +
		`:` + slowDirect + `</Queue>` + createdEvents + `</QueueConfiguration></NotificationConfiguration>`

	put := n.s3Call(http.MethodPut, notificationQuery, config, skipDestinationCheck)
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	require.Equal(t, backlogPuts, n.putMany(strings.TrimPrefix(notifyBucket, "/"), backlogPuts))

	// A drain whose deadline has passed cuts at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.Equal(t, backlogPuts, hub.Drain(ctx), "the event running and every event waiting were cut")
	assert.Contains(t, log.String(), "in-process calls cut at shutdown")
	assert.Contains(t, log.String(), fmt.Sprintf("count=%d", backlogPuts))
	assert.NotContains(t, log.String(), "notification dropped: the delivery queue is full")
}
