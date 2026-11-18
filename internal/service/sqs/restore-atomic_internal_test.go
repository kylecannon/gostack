package sqs

import (
	"bytes"
	"sync"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/require"
)

// Test_Restore_never_shows_a_queue_missing is SQ-89: a call that looks a queue
// up while Restore replaces the state finds the queue, the old one or the
// restored one, never none. The test holds the old queue's lock, so Restore
// stops where it closes that queue, after it took the queue out of the State.
// Before the fix the State stayed empty from there until the restored queues
// went in, and a ReceiveMessage that looked its queue up then answered
// QueueDoesNotExist.
func Test_Restore_never_shows_a_queue_missing(t *testing.T) {
	c := newCutProvider(t)
	state := c.p.states.Get(cutAccount, cutRegion)

	var saved bytes.Buffer

	require.NoError(t, c.p.Snapshot(&saved))

	c.work.mu.Lock()

	unlock := sync.OnceFunc(c.work.mu.Unlock)
	defer unlock()

	done := make(chan error, 1)

	go func() { done <- c.p.Restore(bytes.NewReader(saved.Bytes())) }()

	require.Eventually(t, func() bool {
		q, _ := state.find("work")

		return q != c.work
	}, exclusiveWait, cutTick, "Restore takes the old queue out of the State")

	name := "work"
	out, err := c.p.GetQueueUrl(callContext(t), &sqsapi.GetQueueUrlInput{QueueName: &name})

	unlock()
	require.NoError(t, <-done)

	require.NoError(t, err, "the queue is missing while Restore closes the old one")
	require.Equal(t, c.workURL, *out.QueueUrl)
}
