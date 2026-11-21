package sns_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queueURLKey is SQS's QueueUrl member.
const queueURLKey = "QueueUrl"

// pastOldBound is more deliveries to one destination than the withdrawn
// bound held, 256 admitted and 256 more waiting, so a lane that still had a
// bound would make a publish wait or a delivery drop.
const pastOldBound = 600

// tenEntries are the ten entries of a PublishBatch, tagged so that every
// message of a test is its own.
func tenEntries(tag string) [][]string {
	out := make([][]string, 10)
	for i := range out {
		out[i] = entry(i+1, "e"+strconv.Itoa(i), tag+"-"+strconv.Itoa(i))
	}

	return out
}

// depth is how many messages q holds.
func (s *stack) depth(q *queue) int {
	out := s.sqsCall(account, region, "GetQueueAttributes", map[string]any{
		queueURLKey: q.url, "AttributeNames": []string{"ApproximateNumberOfMessages"},
	})

	attrs, _ := out["Attributes"].(map[string]any)
	text, _ := attrs["ApproximateNumberOfMessages"].(string)
	n, _ := strconv.Atoi(text)

	return n
}

// opener closes g.open once, from the test or its cleanup, whichever is
// first.
func opener(t *testing.T, g *gate) func() {
	t.Helper()

	var once sync.Once

	open := func() { once.Do(func() { close(g.open) }) }
	t.Cleanup(open)

	return open
}

// burst publishes publishers x batches PublishBatch calls of ten at once to a
// standard topic with subscribers healthy raw queues named b0, b1 and on, and
// returns once every publish has answered, with the queues and how many
// deliveries were owed.
func burst(t *testing.T, s *stack, subscribers, publishers, batches int) (queues []*queue, owed int) {
	t.Helper()

	topic := s.createTopic(paramName, "burst")
	queues = make([]*queue, subscribers)

	for i := range queues {
		queues[i] = s.createQueue(account, region, "b"+strconv.Itoa(i), nil)
		s.subscribeQueue(topic, queues[i].arn, attrRaw, textTrue)
	}

	var wg sync.WaitGroup

	for p := range publishers {
		wg.Go(func() {
			for b := range batches {
				r := s.call(opPublishBatch, batch(topic, tenEntries(fmt.Sprintf("p%d-b%d", p, b))...)...)
				assert.Equal(t, http.StatusOK, r.status, r.body)
			}
		})
	}

	wg.Wait()

	return queues, subscribers * publishers * batches * 10
}

// delivered waits for the lanes to empty and returns how many messages the
// queues hold.
func (s *stack) delivered(t *testing.T, queues []*queue) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	s.sns.AwaitDeliveries(ctx)
	require.NoError(t, ctx.Err(), "the lanes never emptied")

	n := 0
	for _, q := range queues {
		n += s.depth(q)
	}

	return n
}

// Test_Provider_Publish_burst is the load that lost 49,466 of 160,000
// deliveries: 16 publishers, 10 batches of 10 each, 100 healthy queues. One
// queue's first delivery is held until every publish has answered, so its
// lane takes all 1,600 of its deliveries while it is stalled: no publish
// waits for a delivery, none is dropped, and every delivery arrives.
func Test_Provider_Publish_burst(t *testing.T) {
	g := newGate(`/b0"`)
	s := newStack(t, g.wrap)
	open := opener(t, g)

	queues, owed := burst(t, s, 100, 16, 10)

	g.awaitEntered(t)
	assert.Zero(t, s.depth(queues[0]), "a delivery to the held queue arrived before every publish answered")
	assert.Zero(t, s.sns.Dropped(), "a delivery was dropped while running")

	open()

	assert.Equal(t, 160_000, owed)
	assert.Equal(t, owed, s.delivered(t, queues))
	assert.Zero(t, s.sns.Dropped())
}

// Test_Provider_Publish_stalledDestination holds every delivery to one queue
// and publishes more to it than the withdrawn bound held: every publish
// answers while none of its deliveries is made, and a publish to another
// topic delivers all of its messages meanwhile, since its queue's lane is its
// own. Once the queue answers, every delivery arrives in publish order and
// none was dropped. The same holds for a FIFO topic and queue.
func Test_Provider_Publish_stalledDestination(t *testing.T) {
	const healthy = 200

	for _, fifo := range []bool{false, true} {
		t.Run("fifo="+strconv.FormatBool(fifo), func(t *testing.T) {
			g := newGate("/stalled")
			s := newStack(t, g.wrap)
			open := opener(t, g)

			stalledTopic := s.createTopic(paramName, "stalled-topic")
			stalled := s.createQueue(account, region, "stalled", nil)

			if fifo {
				stalledTopic = s.createTopic(paramName, "stalled-topic.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
				stalled = s.createQueue(account, region, "stalled.fifo", map[string]string{attrFifoQueue: textTrue})
			}

			s.subscribeQueue(stalledTopic, stalled.arn, attrRaw, textTrue)

			want := make([]string, pastOldBound)

			for i := range pastOldBound {
				want[i] = strconv.Itoa(i)

				params := []string{paramTopicArn, stalledTopic, paramMessage, want[i]}
				if fifo {
					params = append(params, paramGroup, "g", paramDedup, want[i])
				}

				s.publish(params...)
			}

			g.awaitEntered(t)

			topic := s.createTopic(paramName, "healthy-topic")
			q := s.createQueue(account, region, "healthy", nil)
			s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

			for i := range healthy {
				s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
			}

			assert.Len(t, q.await(healthy), healthy)
			assert.Zero(t, s.depth(stalled), "a delivery to the stalled queue arrived before it was let go")
			assert.Zero(t, s.sns.Dropped(), "a delivery was dropped while running")

			open()

			got := make([]string, 0, pastOldBound)
			for _, m := range stalled.await(pastOldBound) {
				got = append(got, m.Body)
			}

			assert.Equal(t, want, got)
			assert.Zero(t, s.sns.Dropped())
		})
	}
}

// Test_Provider_Publish_deadLetterSubscribed: a dead-letter queue is also a
// subscriber of its topic, so it gets each message twice, once dead-lettered.
// Then two missing queues are each other's dead-letter queue: every delivery
// is dropped once, its dead letter refused too, nothing is left in a lane, and
// nothing deadlocks.
func Test_Provider_Publish_deadLetterSubscribed(t *testing.T) {
	const n = 200

	s := newStack(t, nil)

	topic := s.createTopic(paramName, "t")
	dlq := s.createQueue(account, region, "dlq", nil)
	missing := "arn:aws:sqs:" + region + ":" + account + ":missing"
	s.subscribeQueue(topic, missing, attrRaw, textTrue, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)
	s.subscribeQueue(topic, dlq.arn, attrRaw, textTrue)

	for i := range n {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitFor)
	defer cancel()

	s.sns.AwaitDeliveries(ctx)
	require.NoError(t, ctx.Err())
	assert.Equal(t, 2*n, s.depth(dlq))
	assert.Zero(t, s.sns.Dropped())

	cycle := s.createTopic(paramName, "cycle")
	a := "arn:aws:sqs:" + region + ":" + account + ":missing-a"
	b := "arn:aws:sqs:" + region + ":" + account + ":missing-b"
	s.subscribeQueue(cycle, a, attrRedrive, `{"deadLetterTargetArn":"`+b+`"}`)
	s.subscribeQueue(cycle, b, attrRedrive, `{"deadLetterTargetArn":"`+a+`"}`)

	for i := range n {
		s.publish(paramTopicArn, cycle, paramMessage, strconv.Itoa(i))
	}

	s.awaitDropped(2 * n)
	s.sns.AwaitDeliveries(ctx)
	require.NoError(t, ctx.Err())
}

// Test_Provider_goroutines: no goroutine is left once deliveries are made,
// after single publishes and after a burst.
func Test_Provider_goroutines(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	// CreateTopic starts the signing key in a goroutine of its own; a baseline
	// taken while it runs would let one leaked goroutine pass.
	require.Eventually(t, s.sns.SignerReady, keyWait, time.Millisecond)

	s.publish(paramTopicArn, topic, paramMessage, "warm")
	q.await(1)

	before := settledGoroutines(t, s, -1)

	for i := range 300 {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	s.sns.AwaitDeliveries(t.Context())
	settledGoroutines(t, s, before)

	queues, owed := burst(t, s, 20, 8, 5)
	assert.Equal(t, owed, s.delivered(t, queues))
	settledGoroutines(t, s, before)
}

// settledGoroutines waits until the goroutine count is want, or for it to
// settle when want is negative, and returns it. The server's idle connections
// are closed first, as they hold goroutines of their own.
func settledGoroutines(t *testing.T, s *stack, want int) int {
	t.Helper()

	s.server.Client().CloseIdleConnections()

	if want < 0 {
		time.Sleep(100 * time.Millisecond)

		return goroutines()
	}

	// Not require.Eventually: it checks on a goroutine of its own.
	for deadline := time.Now().Add(waitFor); goroutines() > want; {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d, want %d\n%s", goroutines(), want, buf[:runtime.Stack(buf, true)])
		}

		time.Sleep(10 * time.Millisecond)
	}

	return want
}

// goroutines counts the goroutines a stack dump shows. runtime.NumGoroutine
// also counts the runtime's own, which start on first use and stay.
func goroutines() int {
	buf := make([]byte, 1<<20)

	return bytes.Count(buf[:runtime.Stack(buf, true)], []byte("\n\ngoroutine ")) + 1
}

// Test_Provider_Close_cut: the in-process client is stopped with calls of
// SNS's queued in it, as a shutdown's drain stops it at its deadline, and
// more deliveries than the withdrawn bound held waiting behind each: the
// calls are never answered, and Close counts every delivery at once instead
// of waiting for its context.
func Test_Provider_Close_cut(t *testing.T) {
	const queues, messages = 20, pastOldBound

	g := newGate("/cut")
	s := newStack(t, g.wrap)
	t.Cleanup(func() { close(g.open) })

	topic := s.createTopic(paramName, "t")
	for i := range queues {
		s.subscribeQueue(topic, "arn:aws:sqs:"+region+":"+account+":cut"+strconv.Itoa(i))
	}

	for i := range messages {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	g.awaitEntered(t)

	s.sns.CutCalls()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	s.sns.Close(ctx)

	require.NoError(t, ctx.Err(), "Close waited for its context")
	assert.Equal(t, int64(queues*messages), s.sns.Dropped())
}
