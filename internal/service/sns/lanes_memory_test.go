package sns_test

import (
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const memoryMessages = 200

// heapHeld publishes memoryMessages copies of msg while the head of the lane
// they join is stuck, and returns the heap each one holds while it waits.
func heapHeld(s *stack, topic, msg string) int64 {
	var before, after runtime.MemStats

	// A heap measurement reads a collected heap, twice so the sweep has finished.
	runtime.GC() //nolint:revive // call-to-gc: see above.
	runtime.GC() //nolint:revive // call-to-gc: see above.
	runtime.ReadMemStats(&before)

	for range memoryMessages {
		s.publish(paramTopicArn, topic, paramMessage, msg)
	}

	runtime.GC() //nolint:revive // call-to-gc: see above.
	runtime.GC() //nolint:revive // call-to-gc: see above.
	runtime.ReadMemStats(&after)

	return (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / memoryMessages
}

// redriveAttrs are the subscription attributes that name a new queue dlq as
// the dead-letter queue, none for "".
func redriveAttrs(s *stack, dlq string) []string {
	if dlq == "" {
		return nil
	}

	return []string{attrRedrive, redrive(s.createQueue(account, region, dlq, nil).arn)}
}

// stuckHTTP is the heap one delivery holds while it waits behind a delivery
// to an endpoint that never answers, for a subscription whose dead-letter
// queue is dlq, none for "".
func stuckHTTP(t *testing.T, dlq, msg string) int64 {
	t.Helper()

	s := newStack(t, nil)

	release := make(chan struct{})
	e := newEndpoint(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerType) == typeNotification {
			<-release
		}
	})

	t.Cleanup(func() { close(release) })

	topic := s.createTopic(paramName, "t")

	s.confirmHTTP(topic, e, redriveAttrs(s, dlq)...)
	s.publish(paramTopicArn, topic, paramMessage, msg)
	e.next(t)

	return heapHeld(s, topic, msg)
}

// stuckQueue is stuckHTTP for a delivery waiting behind a SendMessage SQS has
// not answered.
func stuckQueue(t *testing.T, dlq, msg string) int64 {
	t.Helper()

	g := newGate("hold-q")
	s := newStack(t, g.wrap)

	t.Cleanup(func() { close(g.open) })

	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "hold-q", nil)

	s.subscribeQueue(topic, q.arn, redriveAttrs(s, dlq)...)
	s.publish(paramTopicArn, topic, paramMessage, msg)
	<-g.entered

	return heapHeld(s, topic, msg)
}

// Test_Provider_Publish_waitingDeliveryHoldsOneBody: a delivery waiting behind
// a stuck destination holds the message once, whether or not its subscription
// has a dead-letter queue. The dead-letter call, an escaped copy of the body,
// is built when the delivery fails (SQ-327); built at publish time it added
// more than the message to every waiting delivery (91 KB for a 32 KiB message
// to an HTTP endpoint, 91 KB to a queue).
func Test_Provider_Publish_waitingDeliveryHoldsOneBody(t *testing.T) {
	msg := strings.Repeat(`{"k":"v"},`, 3200)

	destinations := map[string]func(*testing.T, string, string) int64{protocolHTTP: stuckHTTP, protocolSQS: stuckQueue}

	for name, stuck := range destinations {
		t.Run(name, func(t *testing.T) {
			var without, with int64

			t.Run("without a dead-letter queue", func(t *testing.T) { without = stuck(t, "", msg) })
			t.Run("with a dead-letter queue", func(t *testing.T) { with = stuck(t, "dlq", msg) })

			t.Logf("%s: %d bytes held per waiting delivery without a dead-letter queue, %d with, for a message of %d",
				name, without, with, len(msg))

			assert.Less(t, with-without, int64(len(msg)/4),
				"the dead-letter queue added a copy of the message to every waiting delivery")
		})
	}
}
