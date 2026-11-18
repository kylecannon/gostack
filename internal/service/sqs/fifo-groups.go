package sqs

import "time"

// dedupWindow is how long a FIFO queue remembers a deduplication id.
const dedupWindow = 5 * time.Minute

// fifoState is what an entry of a FIFO queue has beyond a standard one's.
//
// A message entry knows its group and its neighbours in the group's list of
// in-flight messages. A deduplication record is an entry too, so that its expiry
// is a deadline in the queue's one heap and needs no timer of its own: it has
// the key it is kept under, the id and sequence number of the message that made
// it, and the end of its window.
type fifoState struct {
	group      *group
	prev, next *entry

	dedupKey string
	original string
	sequence string
	expires  time.Time
}

// group is a message group of a FIFO queue.
//
// Its visible messages wait in a heap in sending order. A group that has some
// and is not locked sits in the queue's ready list. Receiving from a group locks
// it, and it opens again when a message of it becomes visible again or when the
// last message in flight is deleted. Sending to a locked group does not open it.
type group struct {
	id      string
	visible entryHeap
	// locked is set when a receive took from the group, until it opens again.
	locked bool
	// queued is whether the group is in the ready list.
	queued bool
	// head and tail are the ends of the list of the group's in-flight messages
	// in the order they were received, linked through their fifoState. Only the
	// head is in the deadline heap: a message that is past its deadline stays
	// hidden while one received before it is not, so the group's messages come
	// back in order.
	head, tail *entry
	inflight   int
	// delayed counts the messages that are still waiting out a delay.
	delayed int
}

func lessBySeq(a, b *entry) bool { return a.seq < b.seq }

func visibleIndex(e *entry) *int { return &e.visibleAt }

func newGroup(id string) *group {
	return &group{id: id, visible: entryHeap{less: lessBySeq, index: visibleIndex}}
}

// idle reports whether the group holds nothing and nothing refers to it, so the
// queue can forget it.
func (g *group) idle() bool {
	return g.visible.len() == 0 && g.inflight == 0 && g.delayed == 0 && !g.queued && !g.locked
}

// readyList is the groups a receive can take from, in the order they became
// ready.
type readyList struct {
	items []*group
	head  int
}

// compactAfter is how many taken items a readyList lets pile up in front of its
// live ones before it moves them down.
const compactAfter = 64

func (r *readyList) len() int { return len(r.items) - r.head }

func (r *readyList) push(g *group) { r.items = append(r.items, g) }

func (r *readyList) pop() *group {
	g := r.items[r.head]
	r.items[r.head] = nil
	r.head++

	switch {
	case r.head == len(r.items):
		r.items, r.head = r.items[:0], 0
	case r.head >= compactAfter && r.head*2 >= len(r.items):
		n := copy(r.items, r.items[r.head:])
		clear(r.items[n:])
		r.items, r.head = r.items[:n], 0
	default:
		// The taken items stay in front until there are enough to move.
	}

	return g
}

func (r *readyList) reset() {
	clear(r.items)
	r.items, r.head = r.items[:0], 0
}

// ceilSecond rounds t up to a whole second.
func ceilSecond(t time.Time) time.Time {
	r := t.Truncate(time.Second)
	if r.Before(t) {
		r = r.Add(time.Second)
	}

	return r
}
