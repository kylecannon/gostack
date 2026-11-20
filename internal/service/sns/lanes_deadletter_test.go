package sns_test

import (
	"context"
	"fmt"
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
// dead letter fails. 16 publishers send 20 PublishBatch each. Every publish
// answers 200, the lanes empty, and each delivery is dropped once, when its
// dead letter is refused too: a dead letter never goes to another dead-letter
// queue.
func Test_Provider_Publish_mutualDeadLetter(t *testing.T) {
	s := newStack(t, nil)

	topic := s.createTopic(paramName, "cycle")
	a := "arn:aws:sqs:" + region + ":" + account + ":missing-a"
	b := "arn:aws:sqs:" + region + ":" + account + ":missing-b"

	s.subscribeQueue(topic, a, attrRaw, textTrue, attrRedrive, redrive(b))
	s.subscribeQueue(topic, b, attrRaw, textTrue, attrRedrive, redrive(a))

	var wg sync.WaitGroup

	for p := range 16 {
		wg.Go(func() {
			for i := range 20 {
				r := s.call(opPublishBatch, batch(topic, tenEntries(fmt.Sprintf("p%d-%d", p, i))...)...)
				assert.Equal(t, 200, r.status)
			}
		})
	}

	wg.Wait()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	s.sns.AwaitDeliveries(ctx)
	require.NoError(t, ctx.Err(), "the lanes never emptied")
	assert.Equal(t, int64(16*20*10*2), s.sns.Dropped(), "every delivery failed, and its dead letter was dropped")
}

// Test_Provider_Publish_fifoDeadLetterOrder: a FIFO queue refuses every
// message of one group, more of them than the withdrawn bound held. The
// dead-letter queue receives the group in publish order (SQ-304 saw
// [4 5 0 1 2 3]), and nothing is dropped.
func Test_Provider_Publish_fifoDeadLetterOrder(t *testing.T) {
	g := newGate("/hold-q")
	s := newStack(t, g.refuse)

	fifo := map[string]string{attrFifoQueue: textTrue}
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	q := s.createQueue(account, region, "hold-q.fifo", fifo)
	d := s.createQueue(account, region, "dl.fifo", fifo)

	s.subscribeQueue(topic, q.arn, attrRaw, textTrue, attrRedrive, redrive(d.arn))

	want := make([]string, 0, pastOldBound)

	for i := range pastOldBound {
		id := strconv.Itoa(i)
		want = append(want, id)
		s.publish(paramTopicArn, topic, paramMessage, id, paramGroup, "g", paramDedup, id)
	}

	close(g.open)

	got := make([]string, 0, pastOldBound)
	for _, m := range d.await(pastOldBound) {
		got = append(got, m.Body)
	}

	assert.Equal(t, want, got)
	assert.Zero(t, s.sns.Dropped())
}
