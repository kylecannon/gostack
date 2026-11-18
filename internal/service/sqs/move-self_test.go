package sqs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_a_move_task_cannot_move_a_queue_onto_itself is the code-review finding on
// StartMessageMoveTask: with the source as its own destination the task took each
// message and put it back, and never finished.
func Test_a_move_task_cannot_move_a_queue_onto_itself(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			pipe := newPipe(t, c, "source")
			pipe.deadLetter(c, "m0")

			res := c.startMove(map[string]any{paramSourceArn: pipe.deadARN, paramDestination: pipe.deadARN})

			assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
			assert.Equal(t, "The DestinationArn must be different from the SourceArn.", res.errorMessage())

			// A started task is recorded before its goroutine runs, so no task is
			// no goroutine.
			assert.Empty(t, c.listMoves(pipe.deadARN))
			assert.Equal(t, []string{"m0"}, bodiesOf(c.receive(pipe.dead, nil)), "the message is where it was")
		})
	}
}

// Test_a_move_task_to_another_queue_still_starts guards the check above against
// refusing a destination that merely resembles the source.
func Test_a_move_task_to_another_queue_still_starts(t *testing.T) {
	c := newStack(t).as(protoJSON)
	pipe := newPipe(t, c, "source")
	pipe.deadLetter(c, "m0")

	res := c.startMove(map[string]any{paramSourceArn: pipe.deadARN, paramDestination: pipe.sourceARN})

	require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())
}
