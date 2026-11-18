package sqs

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// dedupData is a deduplication record of a FIFO queue as a snapshot holds it.
// The window ends at an absolute time, so a record that was left when the
// snapshot was taken still suppresses a duplicate after a restart, for the
// rest of its window.
type dedupData struct {
	Key      string    `json:"key"`
	Original string    `json:"original"`
	Sequence string    `json:"sequence"`
	Expires  time.Time `json:"expires"`
}

// fifoData is a fifoContents as a snapshot holds it.
//
// Entries lists the visible and delayed messages in sending order, then the
// messages in flight, group by group in the order they were received: the
// order of a group's in-flight messages decides which of them comes back
// first, and restore rebuilds each group's list by reading them in that order.
// Locked and Ready are the groups that a receive locked and the groups that
// were ready, in the order they became ready, so the next receive takes from
// the group it would have taken from.
type fifoData struct {
	Seq     uint64      `json:"seq"`
	Scope   string      `json:"scope,omitempty"`
	Entries []entryData `json:"entries"`
	Dedup   []dedupData `json:"dedup,omitempty"`
	Locked  []string    `json:"locked,omitempty"`
	Ready   []string    `json:"ready,omitempty"`
}

var _ persistentContents = (*fifoContents)(nil)

func (c *fifoContents) snapshot() (json.RawMessage, error) {
	data := fifoData{Seq: c.seq, Scope: c.scope, Entries: make([]entryData, 0, c.visible+c.delayed+c.inflight)}

	for _, g := range c.groups {
		for _, e := range g.visible.items {
			data.Entries = append(data.Entries, e.data(savedVisible))
		}

		if g.locked {
			data.Locked = append(data.Locked, g.id)
		}
	}

	for _, e := range c.deadlines.items {
		if e.state == stateDelayed {
			data.Entries = append(data.Entries, e.data(savedDelayed))
		}
	}

	slices.SortFunc(data.Entries, func(a, b entryData) int { return cmp.Compare(a.Seq, b.Seq) })
	slices.Sort(data.Locked)

	for _, id := range slices.Sorted(maps.Keys(c.groups)) {
		for e := c.groups[id].head; e != nil; e = e.fifo.next {
			data.Entries = append(data.Entries, e.data(savedInflight))
		}
	}

	for _, g := range c.ready.items[c.ready.head:] {
		if g.queued {
			data.Ready = append(data.Ready, g.id)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(c.dedup)) {
		f := c.dedup[key].fifo
		data.Dedup = append(data.Dedup, dedupData{Key: key, Original: f.original, Sequence: f.sequence, Expires: f.expires})
	}

	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode messages: %w", err)
	}

	return raw, nil
}

// restore fills an empty fifoContents from a snapshot. It puts each message in
// its group and the groups in the state they were in, and sets the deadline
// heap and its timer again: a delayed message whose delay has run out, an
// in-flight one whose timeout has and a deduplication record whose window has
// are settled by the time the queue is used.
func (c *fifoContents) restore(raw json.RawMessage) error {
	var data fifoData

	err := json.Unmarshal(raw, &data)
	if err != nil {
		return fmt.Errorf("decode messages: %w", err)
	}

	c.seq = data.Seq
	c.scope = data.Scope

	for i := range data.Entries {
		d := &data.Entries[i]
		e := &entry{
			msg: d.Message.message(), seq: d.Seq, deadline: d.Deadline,
			receiveCount: d.ReceiveCount, firstReceived: d.FirstReceived, handles: d.Handles,
			visibleAt: -1, deadlineAt: -1,
		}
		e.fifo = &fifoState{group: c.group(e.msg.groupID)}

		err = c.place(e, d.State)
		if err != nil {
			return err
		}

		c.seq = max(c.seq, d.Seq)
		raiseSequence(e.msg.sequence)

		for _, handle := range e.handles {
			c.receipts[handle] = e
		}
	}

	for i := range data.Dedup {
		d := &data.Dedup[i]
		c.dedup[d.Key] = &entry{
			state: stateDedup, deadline: ceilSecond(d.Expires), visibleAt: -1, deadlineAt: -1,
			fifo: &fifoState{dedupKey: d.Key, original: d.Original, sequence: d.Sequence, expires: d.Expires},
		}
		c.deadlines.push(c.dedup[d.Key])
	}

	c.restoreGroups(&data)

	now := time.Now()
	c.refresh(now)
	c.rearm(now)

	return nil
}

// place puts a restored message where its state says.
func (c *fifoContents) place(e *entry, state string) error {
	g := e.fifo.group

	switch state {
	case savedVisible:
		e.state = stateVisible
		e.deadline = time.Time{}
		g.visible.push(e)
		c.visible++
	case savedDelayed, savedInflight:
		if e.deadline.IsZero() {
			return fmt.Errorf("message %s is %s and has no deadline", e.msg.id, state)
		}

		if state == savedInflight {
			c.enterFlight(e, e.deadline)

			return nil
		}

		e.state = stateDelayed
		c.deadlines.push(e)
		c.delayed++
		g.delayed++
	default:
		return fmt.Errorf("message %s is in the unknown state %q", e.msg.id, state)
	}

	return nil
}

// restoreGroups sets the lock of each group and rebuilds the ready list: first
// the groups the snapshot had in it, in its order, then any other group that is
// open and has messages, in the order of their first message.
func (c *fifoContents) restoreGroups(data *fifoData) {
	for _, id := range data.Locked {
		if g, ok := c.groups[id]; ok {
			g.locked = true
		}
	}

	for _, id := range data.Ready {
		if g, ok := c.groups[id]; ok && !g.locked && !g.queued && g.visible.len() > 0 {
			g.queued = true
			c.ready.push(g)
		}
	}

	rest := make([]*group, 0, len(c.groups))
	for _, g := range c.groups {
		if !g.locked && !g.queued && g.visible.len() > 0 {
			rest = append(rest, g)
		}
	}

	slices.SortFunc(rest, func(a, b *group) int {
		return cmp.Or(cmp.Compare(a.visible.peek().seq, b.visible.peek().seq), strings.Compare(a.id, b.id))
	})

	for _, g := range rest {
		c.makeReady(g)
	}
}
