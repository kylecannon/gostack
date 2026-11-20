package sns_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func redrive(arn string) string { return `{"deadLetterTargetArn":"` + arn + `"}` }

// Test_Provider_Publish_mutualDeadLetter: two queues that do not exist, each
// the other subscription's dead-letter queue, so every delivery and every
// dead letter fails. 16 publishers send 20 PublishBatch each. No publish
// waits for room behind a dead letter that waits on its own lane, so none
// reaches its deadline (SQ-304 saw 293 of 320 wait the full wait). A publish
// still waits for room while the two lanes, one send at a time each, work
// through twice their deliveries: under -race and a loaded package that puts
// the median near 200 ms, so the median is held to a quarter of the wait.
func Test_Provider_Publish_mutualDeadLetter(t *testing.T) {
	const wait = 2 * time.Second

	s := newStack(t, nil)
	s.sns.SetLanes(256, wait)

	topic := s.createTopic(paramName, "cycle")
	a := "arn:aws:sqs:" + region + ":" + account + ":missing-a"
	b := "arn:aws:sqs:" + region + ":" + account + ":missing-b"

	s.subscribeQueue(topic, a, attrRaw, textTrue, attrRedrive, redrive(b))
	s.subscribeQueue(topic, b, attrRaw, textTrue, attrRedrive, redrive(a))

	var (
		mu        sync.Mutex
		latencies []time.Duration
		wg        sync.WaitGroup
	)

	for p := range 16 {
		wg.Go(func() {
			for i := range 20 {
				at := time.Now()
				r := s.call(opPublishBatch, batch(topic, tenEntries(fmt.Sprintf("p%d-%d", p, i))...)...)
				took := time.Since(at)

				assert.Equal(t, 200, r.status)

				mu.Lock()
				latencies = append(latencies, took)
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	s.sns.AwaitDeliveries(ctx)
	require.NoError(t, ctx.Err(), "the lanes never emptied")

	slices.Sort(latencies)

	median := latencies[len(latencies)/2]
	slowest := latencies[len(latencies)-1]
	assert.Less(t, slowest, wait, "a publish reached its deadline; median %v", median)
	assert.Less(t, median, wait/4, "median publish %v, slowest %v", median, slowest)
	assert.Equal(t, int64(16*20*10*2), s.sns.Dropped(), "every delivery failed, and its dead letter was dropped")
}

// Test_Provider_Publish_deadLetterAtDeadline: a stuck queue's lane is full and
// its subscription has a healthy, empty dead-letter queue. The deliveries that
// wait for room past the publish's deadline are handed to the dead-letter
// queue, past its lane's depth, and none is dropped.
func Test_Provider_Publish_deadLetterAtDeadline(t *testing.T) {
	for _, c := range []struct{ depth, publishers int }{{8, 1}, {256, 60}} {
		t.Run(strconv.Itoa(c.depth), func(t *testing.T) {
			g := newGate("/stuck")
			s := newStack(t, g.wrap)

			t.Cleanup(func() { close(g.open) })
			s.sns.SetLanes(c.depth, time.Second)

			topic := s.createTopic(paramName, "t")
			q := s.createQueue(account, region, "stuck", nil)
			d := s.createQueue(account, region, "dl", nil)

			s.subscribeQueue(topic, q.arn, attrRaw, textTrue, attrRedrive, redrive(d.arn))

			for i := range c.depth {
				s.publish(paramTopicArn, topic, paramMessage, "fill"+strconv.Itoa(i))
			}

			var wg sync.WaitGroup

			for p := range c.publishers {
				wg.Go(func() {
					r := s.call(opPublishBatch, batch(topic, tenEntries("late"+strconv.Itoa(p))...)...)
					assert.Equal(t, 200, r.status)
				})
			}

			wg.Wait()

			late := c.publishers * 10

			require.Eventually(t, func() bool { return s.depth(d) == late }, waitFor, 5*time.Millisecond,
				"the dead-letter queue holds %d of %d, dropped %d", s.depth(d), late, s.sns.Dropped())
			assert.Zero(t, s.sns.Dropped())
		})
	}
}

// Test_Provider_Publish_fifoDeadLetterOrder: a FIFO queue refuses every
// message of one group, and the publishes past its lane's depth reach their
// deadline first. The dead-letter queue still receives the group in publish
// order (SQ-304 saw [4 5 0 1 2 3]).
func Test_Provider_Publish_fifoDeadLetterOrder(t *testing.T) {
	const depth = 4

	g := newGate("/hold-q")
	s := newStack(t, g.refuse)
	s.sns.SetLanes(depth, 200*time.Millisecond)

	fifo := map[string]string{attrFifoQueue: textTrue}
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	q := s.createQueue(account, region, "hold-q.fifo", fifo)
	d := s.createQueue(account, region, "dl.fifo", fifo)

	s.subscribeQueue(topic, q.arn, attrRaw, textTrue, attrRedrive, redrive(d.arn))

	want := make([]string, 0, depth+2)

	for i := range depth + 2 {
		id := strconv.Itoa(i)
		want = append(want, id)
		s.publish(paramTopicArn, topic, paramMessage, id, paramGroup, "g", paramDedup, id)
	}

	close(g.open)

	got := make([]string, 0, depth+2)
	for _, m := range d.await(depth + 2) {
		got = append(got, m.Body)
	}

	assert.Equal(t, want, got)
	assert.Zero(t, s.sns.Dropped())
}
