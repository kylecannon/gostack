package sqs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scopeDedupID = "d"

// scopeCase is one change of DeduplicationScope between sends of one
// deduplication id.
type scopeCase struct {
	name       string
	giveStart  string
	giveChange string
	giveBefore string // group of a send before the change, "" for none
	giveFirst  string // group of the first send after it
	giveSecond string // group of the second
	wantSame   bool   // whether the second send is a duplicate of the first
	restart    bool   // whether the queue goes through a snapshot after the change
}

// run creates a FIFO queue with the start scope, sends before the change,
// changes the scope with SetQueueAttributes, optionally takes the queue through a
// snapshot, and sends twice.
func (tt scopeCase) run(t *testing.T, protocol string) {
	t.Helper()

	s := newStack(t)
	c := s.as(protocol)
	url := c.createFIFO(map[string]string{attrDedupScope: tt.giveStart})

	var before result
	if tt.giveBefore != "" {
		before = c.sendTo(url, "before", tt.giveBefore, scopeDedupID)
	}

	set := c.call(opSetAttributes, map[string]any{
		paramQueueURL: url, paramAttributes: map[string]string{attrDedupScope: tt.giveChange},
	})
	require.True(t, set.ok(), "%s %s", set.errorCode(), set.errorMessage())

	if tt.restart {
		c = restoredStack(t, newSettings(), snapshotOf(t, s.provider)).as(protocol)
	}

	first := c.sendTo(url, "first", tt.giveFirst, scopeDedupID)
	second := c.sendTo(url, "second", tt.giveSecond, scopeDedupID)

	if tt.giveBefore != "" {
		assert.NotEqual(t, before.str(keyMessageID), first.str(keyMessageID),
			"the record of the old scope is not the new scope's")
	}

	if tt.wantSame {
		assert.Equal(t, first.str(keyMessageID), second.str(keyMessageID), "the second is a duplicate")
	} else {
		assert.NotEqual(t, first.str(keyMessageID), second.str(keyMessageID), "the second is not a duplicate")
	}
}

// DeduplicationScope is settable (botocore SetQueueAttributes lists it), and the
// next send reads it. A deduplication record keeps the scope it was made under,
// so a send after the change matches only the records of its own scope.
func Test_FIFO_deduplication_scope_is_read_on_each_send(t *testing.T) {
	tests := []scopeCase{
		{
			name: "queue to messageGroup keeps two groups apart", giveStart: scopeQueueValue, giveChange: valueMsgGroup,
			giveFirst: groupOne, giveSecond: groupTwo,
		},
		{
			name: "queue to messageGroup still deduplicates inside a group", giveStart: scopeQueueValue,
			giveChange: valueMsgGroup, giveFirst: groupOne, giveSecond: groupOne, wantSame: true,
		},
		{
			name: "messageGroup to queue joins two groups", giveStart: valueMsgGroup, giveChange: scopeQueueValue,
			giveFirst: groupOne, giveSecond: groupTwo, wantSame: true,
		},
		{
			name:      "a record of the queue scope does not catch a send of the messageGroup scope",
			giveStart: scopeQueueValue, giveChange: valueMsgGroup, giveBefore: groupOne,
			giveFirst: groupOne, giveSecond: groupOne, wantSame: true,
		},
		{
			name:      "a record of the messageGroup scope does not catch a send of the queue scope",
			giveStart: valueMsgGroup, giveChange: scopeQueueValue, giveBefore: groupOne,
			giveFirst: groupOne, giveSecond: groupOne, wantSame: true,
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) { tt.run(t, protocol) })

			tt.restart = true
			t.Run(protocol+"/"+tt.name+"/after a restore", func(t *testing.T) { tt.run(t, protocol) })
		}
	}
}
