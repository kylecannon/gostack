package sqs_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scopeOldID = "old"
	scopeNewID = "new"
)

// scopeCase is one change of DeduplicationScope, with the sends around it.
type scopeCase struct {
	name       string
	giveStart  string
	giveChange string
	wantSameID bool // whether a group-two send of one id duplicates the group-one send
	restart    bool // whether the queue goes through a snapshot after the change
}

// run creates a FIFO queue with the start scope, sends an id, changes the scope
// (and FifoThroughputLimit with it, as the parity test does) with
// SetQueueAttributes, optionally takes the queue through a snapshot, and sends
// the old id and a new one to a second group.
func (tt scopeCase) run(t *testing.T, protocol string) {
	t.Helper()

	s := newStack(t)
	c := s.as(protocol)
	url := c.createFIFO(map[string]string{attrDedupScope: tt.giveStart})

	before := c.sendTo(url, "before", groupOne, scopeOldID)
	require.True(t, before.ok(), "%s %s", before.errorCode(), before.errorMessage())

	change := map[string]string{attrDedupScope: tt.giveChange}
	if tt.giveChange == valueMsgGroup {
		change[attrThroughput] = valuePerGroup
	}

	set := c.call(opSetAttributes, map[string]any{paramQueueURL: url, paramAttributes: change})
	require.True(t, set.ok(), "%s %s", set.errorCode(), set.errorMessage())

	if tt.restart {
		c = restoredStack(t, newSettings(), snapshotOf(t, s.provider)).as(protocol)
	}

	assert.Equal(t, tt.giveChange, c.allAttributes(url)[attrDedupScope], "GetQueueAttributes shows the new value")

	oldSame := c.sendTo(url, "old-same-group", groupOne, scopeOldID)
	assert.Equal(t, before.str(keyMessageID), oldSame.str(keyMessageID), "the old id in its group is a duplicate")

	oldOther := c.sendTo(url, "old-other-group", groupTwo, scopeOldID)
	assert.Equal(t, tt.wantSameID, before.str(keyMessageID) == oldOther.str(keyMessageID),
		"the old id in another group follows the creation scope")

	newFirst := c.sendTo(url, "new-first", groupOne, scopeNewID)
	newSecond := c.sendTo(url, "new-second", groupTwo, scopeNewID)
	assert.Equal(t, tt.wantSameID, newFirst.str(keyMessageID) == newSecond.str(keyMessageID),
		"a new id in another group follows the creation scope")
}

// DeduplicationScope is settable (botocore SetQueueAttributes lists it), and
// GetQueueAttributes shows the new value, but deduplication keeps the scope the
// queue was created with, for the ids sent before the change and the ones sent
// after it. The LocalStack parity test
// TestSqsProvider::test_fifo_change_to_high_throughput_after_creation, which
// passes on AWS, pins this.
func Test_FIFO_deduplication_scope_is_fixed_at_creation(t *testing.T) {
	tests := []scopeCase{
		{name: "queue to messageGroup", giveStart: scopeQueueValue, giveChange: valueMsgGroup, wantSameID: true},
		{name: "messageGroup to queue", giveStart: valueMsgGroup, giveChange: scopeQueueValue},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) { tt.run(t, protocol) })

			tt.restart = true
			t.Run(protocol+"/"+tt.name+"/after a restore", func(t *testing.T) { tt.run(t, protocol) })
		}
	}
}

// A snapshot written before the scope was saved has none. The queue's current
// DeduplicationScope attribute is the scope it restores with.
func Test_FIFO_snapshot_without_scope_restores_with_the_attribute(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			s := newStack(t)
			c := s.as(protocol)
			url := c.createFIFO(map[string]string{attrDedupScope: scopeQueueValue})

			set := c.call(opSetAttributes, map[string]any{
				paramQueueURL: url, paramAttributes: map[string]string{attrDedupScope: valueMsgGroup},
			})
			require.True(t, set.ok(), "%s %s", set.errorCode(), set.errorMessage())

			snapshot := snapshotOf(t, s.provider)
			require.Equal(t, 1, bytes.Count(snapshot, []byte(`"scope":"queue",`)), "the snapshot saves the creation scope")

			legacy := bytes.ReplaceAll(snapshot, []byte(`"scope":"queue",`), nil)
			c = restoredStack(t, newSettings(), legacy).as(protocol)

			first := c.sendTo(url, "first", groupOne, scopeOldID)
			second := c.sendTo(url, "second", groupTwo, scopeOldID)
			assert.NotEqual(t, first.str(keyMessageID), second.str(keyMessageID),
				"the attribute's messageGroup scope keeps the groups apart")

			c = restoredStack(t, newSettings(), snapshot).as(protocol)

			first = c.sendTo(url, "first", groupOne, scopeNewID)
			second = c.sendTo(url, "second", groupTwo, scopeNewID)
			assert.Equal(t, first.str(keyMessageID), second.str(keyMessageID), "the saved queue scope joins them")
		})
	}
}
