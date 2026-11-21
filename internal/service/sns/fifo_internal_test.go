package sns

import (
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// windowFixture is a FIFO topic whose window holds n ids, id i taken i gaps
// after base.
func windowFixture(n int, gap time.Duration) (f *fifoTopic, base time.Time) {
	base = time.Unix(1_700_000_000, 0)
	f = &fifoTopic{seen: make(map[string]published, n), order: make([]string, 0, n)}

	for i := range n {
		key := "dedup-id-of-a-usual-length-" + strconv.Itoa(i)
		f.seen[key] = published{id: "m", sequence: strconv.Itoa(i), at: base.Add(time.Duration(i) * gap)}
		f.order = append(f.order, key)
	}

	return f, base
}

// Test_publishFIFO_window_boundary pins where the window ends: an id taken
// at t is a duplicate through t+4m59s and new at t+5m, the instant the
// window has passed. It holds for ids that expire one after the other.
func Test_publishFIFO_window_boundary(t *testing.T) {
	p := fuzzProvider()
	base := time.Unix(1_700_000_000, 0)
	now := base
	p.now = func() time.Time { return now }

	topic := &Topic{Name: "t.fifo", ARN: "arn:aws:sns:us-east-1:000000000000:t.fifo"}

	publish := func(dedupID string) *message {
		m := &message{id: newUUID(), text: "x", dedupID: dedupID, groupID: "g", topicARN: topic.ARN}
		p.publishFIFO(nil, topic, []*message{m})

		return m
	}

	a := publish("a")

	now = base.Add(time.Second)
	b := publish("b")

	now = base.Add(4*time.Minute + 59*time.Second)
	assert.Equal(t, a.id, publish("a").id, "4m59s after it was taken, a repeat is a duplicate")

	now = base.Add(5 * time.Minute)
	assert.NotEqual(t, a.id, publish("a").id, "5m after it was taken, the id is new")
	assert.Equal(t, b.id, publish("b").id, "b was taken a second later, so it is still inside the window")

	now = base.Add(5*time.Minute + time.Second)
	assert.NotEqual(t, b.id, publish("b").id, "the window has passed b too")
	assert.Len(t, topic.fifo.seen, 2, "a and b are in the window again, as the new ids")
	assert.Equal(t, []string{"a", "b"}, topic.fifo.order)
}

// Test_fifoTopic_expire_allocates_nothing: ids arrive in time order, so a
// publish that expires one id pops it from the front of the queue. It does
// not copy the live ids it keeps, which at a million of them cost 2.34 ms and
// 244 MiB a publish.
func Test_fifoTopic_expire_allocates_nothing(t *testing.T) {
	const live = 20_000

	f, base := windowFixture(live, time.Microsecond)
	step := 0

	allocs := testing.AllocsPerRun(100, func() {
		step++
		// Step i puts the clock where ids 0..i-1 have just left the window.
		f.expire(base.Add(dedupWindow + time.Duration(step-1)*time.Microsecond))
	})

	assert.Zero(t, allocs, "an expiry that drops an id allocates")
	assert.Len(t, f.seen, live-101)
	assert.Len(t, f.order, live-101)
	assert.Equal(t, "dedup-id-of-a-usual-length-101", f.order[0], "the oldest id left is the first one kept")
}

// Test_fifoTopic_expire_frees_what_it_expires: once the window has passed
// its ids, the memory they held is released, not kept until the topic's next
// publish or for good. A map keeps its table after its entries are deleted,
// so expire builds a smaller one.
func Test_fifoTopic_expire_frees_what_it_expires(t *testing.T) {
	const live = 200_000

	before := liveHeap()
	f, base := windowFixture(live, time.Microsecond)
	grown := int64(liveHeap()) - int64(before)

	if grown < 10<<20 {
		t.Fatalf("a window of %d ids grew the heap by only %d bytes", live, grown)
	}

	// Everything but the last ten ids has left the window.
	f.expire(base.Add(dedupWindow + time.Duration(live-11)*time.Microsecond))

	kept := int64(liveHeap()) - int64(before)

	assert.Len(t, f.seen, 10)
	assert.Len(t, f.order, 10)
	assert.LessOrEqual(t, cap(f.order), 64, "the queue keeps the room of the ids it dropped")
	assert.Less(t, kept, grown/4, "the heap still holds %d of the %d bytes the window grew by", kept, grown)

	runtime.KeepAlive(f)
}

// liveHeap is the bytes of heap in use once the collector has run.
func liveHeap() uint64 {
	runtime.GC() //nolint:revive // The heap is read after a collection, and the test needs the heap collected.

	var m runtime.MemStats

	runtime.ReadMemStats(&m)

	return m.HeapAlloc
}
