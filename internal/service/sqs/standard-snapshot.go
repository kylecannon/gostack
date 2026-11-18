package sqs

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
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
}

func (m *message) data() messageData {
	return messageData{
		ID: m.id, Body: m.body, BodyMD5: m.bodyMD5,
		Attributes: m.attributes, AttributesMD5: m.attributesMD5,
		System: m.system, Sent: m.sent,
	}
}

func (d *messageData) message() *message {
	return &message{
		id: d.ID, body: d.Body, bodyMD5: d.BodyMD5,
		attributes: d.Attributes, attributesMD5: d.AttributesMD5,
		system: d.System, sent: d.Sent,
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
	ReceiveCount  int         `json:"receiveCount,omitempty"`
	FirstReceived time.Time   `json:"firstReceived,omitzero"`
	Handles       []string    `json:"handles,omitempty"`
}

// standardData is a standardContents as a snapshot holds it.
type standardData struct {
	Seq     uint64      `json:"seq"`
	Entries []entryData `json:"entries"`
}

var _ persistentContents = (*standardContents)(nil)

func (c *standardContents) snapshot() (json.RawMessage, error) {
	data := standardData{Seq: c.seq, Entries: make([]entryData, 0, c.visible.len()+c.deadlines.len())}

	for _, e := range c.visible.items {
		data.Entries = append(data.Entries, e.data(savedVisible))
	}

	for _, e := range c.deadlines.items {
		name := savedInflight
		if e.state == stateDelayed {
			name = savedDelayed
		}

		data.Entries = append(data.Entries, e.data(name))
	}

	slices.SortFunc(data.Entries, func(a, b entryData) int { return cmp.Compare(a.Seq, b.Seq) })

	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode messages: %w", err)
	}

	return raw, nil
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
func (c *standardContents) restore(raw json.RawMessage) error {
	var data standardData

	err := json.Unmarshal(raw, &data)
	if err != nil {
		return fmt.Errorf("decode messages: %w", err)
	}

	c.seq = data.Seq

	for i := range data.Entries {
		d := &data.Entries[i]
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
