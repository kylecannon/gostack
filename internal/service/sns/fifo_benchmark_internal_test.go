package sns

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// BenchmarkPublishFIFOWindow times a FIFO publish to a topic whose dedup
// window already holds live ids, each publish expiring the one id the window
// has just passed and taking one: the steady state of a topic published to at
// a constant rate. The window is built directly, so a million ids cost no
// setup publishes. A publish's cost
// must not grow with live, which a copy of every live id on each expiry made
// it do: 552 ns at a million ids became 2.34 ms.
//
//	BenchmarkPublishFIFOWindow/live=1000000
func BenchmarkPublishFIFOWindow(b *testing.B) {
	for _, live := range []int{1_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("live=%d", live), func(b *testing.B) { benchPublishFIFOWindow(b, live) })
	}
}

func benchPublishFIFOWindow(b *testing.B, live int) {
	p := fuzzProvider()
	topic := &Topic{Name: "t.fifo", ARN: "arn:aws:sns:us-east-1:000000000000:t.fifo"}

	// The ids are spread over the window, and each publish moves the clock on
	// by one gap, so each expires one id and takes one: live stays where it is.
	gap := dedupWindow / time.Duration(live)

	window, base := windowFixture(live, gap)
	topic.fifo.seen, topic.fifo.order = window.seen, window.order

	// The clock starts where id 0 has just left the window.
	step := 0

	p.now = func() time.Time { return base.Add(dedupWindow + time.Duration(step)*gap) }

	msgs := make([]*message, 1)
	msgs[0] = &message{id: "m", text: "x", groupID: "g", topicARN: topic.ARN}

	b.ReportAllocs()

	for b.Loop() {
		msgs[0].dedupID = "bench-id-of-a-usual-length-" + strconv.Itoa(step)
		step++

		p.publishFIFO(nil, topic, msgs)
	}
}
