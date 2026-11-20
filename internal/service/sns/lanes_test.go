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

// publishAsync publishes on a goroutine of its own and hands back the reply.
func (s *stack) publishAsync(params ...string) <-chan reply {
	out := make(chan reply, 1)

	go func() { out <- s.call("Publish", params...) }()

	return out
}

// burst publishes publishers x batches PublishBatch calls of ten at once to a
// standard topic with subscribers healthy raw queues, and returns how many
// messages reached the queues and how many deliveries were owed.
func burst(t *testing.T, s *stack, subscribers, publishers, batches int) (delivered, owed int) {
	t.Helper()

	topic := s.createTopic(paramName, "burst")
	queues := make([]*queue, subscribers)

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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	s.sns.AwaitDeliveries(ctx)

	for _, q := range queues {
		delivered += s.depth(q)
	}

	return delivered, subscribers * publishers * batches * 10
}

// Test_Provider_Publish_burst is the load that lost 49,466 of 160,000
// deliveries to one shared queue: 16 publishers, 10 batches of 10 each, 100
// healthy queues. Every delivery arrives; publishers wait for room instead.
func Test_Provider_Publish_burst(t *testing.T) {
	s := newStack(t, nil)

	delivered, owed := burst(t, s, 100, 16, 10)

	assert.Equal(t, 160_000, owed)
	assert.Equal(t, owed, delivered)
	assert.Zero(t, s.sns.Dropped())
}

// Test_Provider_Publish_stuckQueue holds every delivery to one queue and
// fills its lane, with one more publish waiting for room: a publish to another
// topic delivers all of its messages, since its queue's lane is its own. The
// waiting publish answers 200 at its deadline and its delivery is dropped.
func Test_Provider_Publish_stuckQueue(t *testing.T) {
	const depth, healthy = 4, 200

	g := newGate("/stuck")
	s := newStack(t, g.wrap)
	t.Cleanup(func() { close(g.open) })
	s.sns.SetLanes(depth, 500*time.Millisecond)

	stuckTopic := s.createTopic(paramName, "stuck-topic")
	stuck := s.createQueue(account, region, "stuck", nil)
	s.subscribeQueue(stuckTopic, stuck.arn, attrRaw, textTrue)

	for i := range depth {
		s.publish(paramTopicArn, stuckTopic, paramMessage, strconv.Itoa(i))
	}

	g.awaitEntered(t)

	waiting := s.publishAsync(paramTopicArn, stuckTopic, paramMessage, "waits")

	topic := s.createTopic(paramName, "healthy-topic")
	q := s.createQueue(account, region, "healthy", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	for i := range healthy {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	assert.Len(t, q.await(healthy), healthy)
	assert.Equal(t, http.StatusOK, (<-waiting).status)
	s.awaitDropped(1)
}

// Test_Provider_Publish_deadLetterNeverAnswers: a FIFO topic's two queues are
// stuck with full lanes, and their dead-letter queues never answer. A publish
// waits its deadline, hands its deliveries to the dead-letter lanes without
// waiting, and answers 200; a second publisher, sent while the first waits,
// waits its own deadline, not the first's and then its own.
func Test_Provider_Publish_deadLetterNeverAnswers(t *testing.T) {
	const wait = time.Second

	g := newGate("stuck-")
	s := newStack(t, g.wrap)
	t.Cleanup(func() { close(g.open) })
	s.sns.SetLanes(1, wait)

	fifo := map[string]string{attrFifoQueue: textTrue}
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)

	for i := range 2 {
		n := strconv.Itoa(i)
		q := s.createQueue(account, region, "stuck-q"+n+".fifo", fifo)
		dlq := s.createQueue(account, region, "stuck-d"+n+".fifo", fifo)
		s.subscribeQueue(topic, q.arn, attrRaw, textTrue, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)
	}

	s.publish(paramTopicArn, topic, paramMessage, "fills", paramGroup, "g", paramDedup, "fills")
	g.awaitEntered(t)

	start := time.Now()
	second := make(chan time.Duration, 1)

	go func() {
		time.Sleep(wait / 10)

		at := time.Now()
		r := s.call(opPublish, paramTopicArn, topic, paramMessage, "second", paramGroup, "g", paramDedup, "second")
		assert.Equal(t, http.StatusOK, r.status, r.body)

		second <- time.Since(at)
	}()

	s.publish(paramTopicArn, topic, paramMessage, "first", paramGroup, "g", paramDedup, "first")

	first := time.Since(start)
	limit := wait + wait/2

	assert.Less(t, first, limit, "the first publish waits its deadline and no dead-letter send")
	assert.Less(t, <-second, limit, "the second publish waits its own deadline, not the first's too")
}

// Test_Provider_Publish_deadLetterSubscribed: a dead-letter queue is also a
// subscriber of its topic, so it gets each message twice, once dead-lettered.
// Then two missing queues are each other's dead-letter queue, with lanes of
// one: every delivery is dropped once, nothing is left in a lane, and nothing
// deadlocks.
func Test_Provider_Publish_deadLetterSubscribed(t *testing.T) {
	const n = 200

	s := newStack(t, nil)
	s.sns.SetLanes(2, 100*time.Millisecond)

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

	s.publish(paramTopicArn, topic, paramMessage, "warm")
	q.await(1)

	before := settledGoroutines(t, s, -1)

	for i := range 300 {
		s.publish(paramTopicArn, topic, paramMessage, strconv.Itoa(i))
	}

	s.sns.AwaitDeliveries(t.Context())
	settledGoroutines(t, s, before)

	delivered, owed := burst(t, s, 20, 8, 5)
	assert.Equal(t, owed, delivered)
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
// SNS's queued in it, as a shutdown's drain stops it at its deadline: those
// are never answered, and Close counts them at once instead of waiting for
// its context.
func Test_Provider_Close_cut(t *testing.T) {
	const queues = 20

	g := newGate("/cut")
	s := newStack(t, g.wrap)
	t.Cleanup(func() { close(g.open) })

	topic := s.createTopic(paramName, "t")
	for i := range queues {
		s.subscribeQueue(topic, "arn:aws:sqs:"+region+":"+account+":cut"+strconv.Itoa(i))
	}

	s.publish(paramTopicArn, topic, paramMessage, "m")
	g.awaitEntered(t)

	s.sns.CutCalls()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	s.sns.Close(ctx)

	require.NoError(t, ctx.Err(), "Close waited for its context")
	assert.Equal(t, int64(queues), s.sns.Dropped())
}

// Test_Provider_Shutdown_releases: a publish waiting for room is let go when
// the shutdown begins, and its delivery is made.
func Test_Provider_Shutdown_releases(t *testing.T) {
	g := newGate("/held")
	s := newStack(t, g.wrap)
	s.sns.SetLanes(1, time.Hour)

	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "held", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	s.publish(paramTopicArn, topic, paramMessage, "one")
	g.awaitEntered(t)

	waiting := s.publishAsync(paramTopicArn, topic, paramMessage, "two")

	select {
	case <-waiting:
		t.Fatal("the publish did not wait for room")
	case <-time.After(50 * time.Millisecond):
	}

	s.sns.BeginShutdown()

	select {
	case r := <-waiting:
		assert.Equal(t, http.StatusOK, r.status)
	case <-time.After(waitFor):
		t.Fatal("the shutdown did not let the waiting publish go")
	}

	close(g.open)

	msgs := q.await(2)
	assert.Equal(t, "one", msgs[0].Body)
	assert.Equal(t, "two", msgs[1].Body)
	assert.Zero(t, s.sns.Dropped())
}
