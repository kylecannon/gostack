package sqs

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
)

// The names of the entry states in a snapshot. The numbers of entryState are an
// implementation detail, so they stay out of the file.
const (
	savedDelayed  = "delayed"
	savedVisible  = "visible"
	savedInflight = "inflight"
)

// messageData is a message as a snapshot holds it.
type messageData struct {
	ID            string                                  `json:"id"`
	Body          string                                  `json:"body"`
	BodyMD5       string                                  `json:"bodyMD5"`
	Attributes    map[string]sqsapi.MessageAttributeValue `json:"attributes,omitempty"`
	AttributesMD5 string                                  `json:"attributesMD5,omitempty"`
	System        map[string]string                       `json:"system,omitempty"`
	Sent          time.Time                               `json:"sent"`
	GroupID       string                                  `json:"groupID,omitempty"`
	DedupID       string                                  `json:"dedupID,omitempty"`
	Sequence      string                                  `json:"sequence,omitempty"`
	ReceiveBase   int                                     `json:"receiveBase,omitzero"`
}

func (m *message) data() messageData {
	return messageData{
		ID: m.id, Body: m.body, BodyMD5: m.bodyMD5,
		Attributes: m.attributes, AttributesMD5: m.attributesMD5,
		System: m.system, Sent: m.sent,
		GroupID: m.groupID, DedupID: m.dedupID, Sequence: m.sequence, ReceiveBase: m.receiveBase,
	}
}

func (d *messageData) message() *message {
	return &message{
		id: d.ID, body: d.Body, bodyMD5: d.BodyMD5,
		attributes: d.Attributes, attributesMD5: d.AttributesMD5,
		system: d.System, sent: d.Sent,
		groupID: d.GroupID, dedupID: d.DedupID, sequence: d.Sequence, receiveBase: d.ReceiveBase,
	}
}

// entryData is an entry of a standardContents as a snapshot holds it: the
// message, where it is, and what the contents counted for it. The deadlines and
// the send and receive times are absolute, so a message that was in flight when
// the snapshot was taken is visible again once its timeout has passed, however
// long the process was down.
type entryData struct {
	Message       messageData `json:"message"`
	Seq           uint64      `json:"seq"`
	State         string      `json:"state"`
	Deadline      time.Time   `json:"deadline,omitzero"`
	ReceiveCount  int         `json:"receiveCount,omitzero"`
	FirstReceived time.Time   `json:"firstReceived,omitzero"`
	Handles       []string    `json:"handles,omitempty"`
}

// checkEntry refuses what a null leaves of an entry. The entries are pointers
// so that sort swaps pointers, and a null is nil; a null where a message belongs
// is a message with no id, which no message is.
func checkEntry(d *entryData) error {
	switch {
	case d == nil:
		return errors.New("sqs snapshot has a null message entry")
	case d.Message.ID == "":
		return errors.New("sqs snapshot has a message without an id")
	case slices.Contains(d.Handles, ""):
		return errors.New("sqs snapshot has a message with an empty receipt handle")
	default:
		return nil
	}
}

// entryStore hands out the entryData of one copy from a single allocation, so a
// copy of a hundred thousand messages is one allocation and not that many, and
// the entries can be sorted by swapping pointers.
type entryStore struct{ items []entryData }

func newEntryStore(size int) *entryStore { return &entryStore{items: make([]entryData, 0, size)} }

// add stores d and returns where. When the store was sized too small it moves
// to a new array, and the pointers handed out before stay good.
func (s *entryStore) add(d entryData) *entryData {
	s.items = append(s.items, d)

	return &s.items[len(s.items)-1]
}

// contentsData is a Contents as a snapshot holds it: the messages of a standard
// queue with the creation counter, and for a FIFO queue the rest of what orders
// its groups. A standard queue leaves the FIFO members out.
//
// Entries lists the messages as the Contents restores them. For a standard
// queue that is every message in sending order. For a FIFO queue it is the
// visible and delayed messages in sending order, then the messages in flight,
// group by group in the order they were received: the order of a group's
// in-flight messages decides which of them comes back first, and restore
// rebuilds each group's list by reading them in that order. Locked and Ready are
// the groups that a receive locked and the groups that were ready, in the order
// they became ready, so the next receive takes from the group it would have
// taken from.
//
// snapshot fills it in no particular order, because it runs under the queue's
// lock, and sort puts it in the order above.
type contentsData struct {
	Seq     uint64       `json:"seq"`
	Scope   string       `json:"scope,omitempty"`
	Entries []*entryData `json:"entries"`
	Dedup   []dedupData  `json:"dedup,omitempty"`
	Locked  []string     `json:"locked,omitempty"`
	Ready   []string     `json:"ready,omitempty"`

	// bySeq is how many entries, from the first, sort by sequence number. The
	// ones after them are in-flight messages: sort groups them by their group id
	// and keeps the order snapshot gave them within a group.
	bySeq int
}

// sort puts the copy snapshot made in the order the file keeps it.
func (d *contentsData) sort() {
	slices.SortFunc(d.Entries[:d.bySeq], func(a, b *entryData) int { return cmp.Compare(a.Seq, b.Seq) })
	slices.SortStableFunc(d.Entries[d.bySeq:], func(a, b *entryData) int {
		return strings.Compare(a.Message.GroupID, b.Message.GroupID)
	})
	slices.Sort(d.Locked)
	slices.SortFunc(d.Dedup, func(a, b dedupData) int { return strings.Compare(a.Key, b.Key) })
}

var _ persistentContents = (*standardContents)(nil)

func (c *standardContents) snapshot() contentsData {
	data := contentsData{Seq: c.seq, Entries: make([]*entryData, 0, c.visible.len()+c.deadlines.len())}
	store := newEntryStore(c.visible.len() + c.deadlines.len())

	for _, e := range c.visible.items {
		data.Entries = append(data.Entries, store.add(e.data(savedVisible)))
	}

	for _, e := range c.deadlines.items {
		name := savedInflight
		if e.state == stateDelayed {
			name = savedDelayed
		}

		data.Entries = append(data.Entries, store.add(e.data(name)))
	}

	data.bySeq = len(data.Entries)

	return data
}

func (e *entry) data(state string) entryData {
	return entryData{
		Message: e.msg.data(), Seq: e.seq, State: state, Deadline: e.deadline,
		ReceiveCount: e.receiveCount, FirstReceived: e.firstReceived, Handles: e.handles,
	}
}

// restore fills an empty standardContents from a snapshot. It builds the heaps
// again and arms the timer for the earliest deadline, so a delayed message
// whose delay has run out, or an in-flight one whose timeout has, is visible by
// the time the queue is used.
func (c *standardContents) restore(data *contentsData) error {
	c.seq = data.Seq

	for i := range data.Entries {
		d := data.Entries[i]

		err := checkEntry(d)
		if err != nil {
			return err
		}

		e := &entry{
			msg: d.Message.message(), seq: d.Seq, deadline: d.Deadline,
			receiveCount: d.ReceiveCount, firstReceived: d.FirstReceived, handles: d.Handles,
			visibleAt: -1, deadlineAt: -1,
		}

		err = c.place(e, d.State)
		if err != nil {
			return err
		}

		c.seq = max(c.seq, d.Seq)

		for _, handle := range e.handles {
			c.receipts[handle] = e
		}
	}

	now := time.Now()
	c.refresh(now)
	c.rearm(now)

	return nil
}

// place puts a restored entry in the heap its state names.
func (c *standardContents) place(e *entry, state string) error {
	switch state {
	case savedVisible:
		e.state = stateVisible
		e.deadline = time.Time{}
		c.visible.push(e)
	case savedDelayed, savedInflight:
		if e.deadline.IsZero() {
			return fmt.Errorf("message %s is %s and has no deadline", e.msg.id, state)
		}

		if state == savedDelayed {
			e.state = stateDelayed
			c.delayed++
		} else {
			e.state = stateInflight
			c.inflight++
		}

		c.deadlines.push(e)
	default:
		return fmt.Errorf("message %s is in the unknown state %q", e.msg.id, state)
	}

	return nil
}
