package sqs_test

import (
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	statusFailed    = "FAILED"
	statusCompleted = "COMPLETED"
	keyStatus       = "Status"
	keyMessageID    = "MessageId"
	reasonRestarted = "ServiceRestarted"
)

// sequenceOf reads the SequenceNumber of a SendMessage result.
func sequenceOf(t *testing.T, res result) uint64 {
	t.Helper()

	n, err := strconv.ParseUint(res.str(paramSequence), 10, 64)
	require.NoError(t, err)

	return n
}

func Test_a_FIFO_queue_is_the_same_after_a_snapshot(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := newStack(t)
				ca := a.as(protocol)
				url := ca.createFIFO(nil)

				sent := map[string]result{}

				for _, m := range [][2]string{
					{"m1", groupOne}, {"n1", groupTwo}, {"m2", groupOne}, {"o1", groupThree}, {"m3", groupOne}, {"n2", groupTwo},
				} {
					sent[m[0]] = ca.sendTo(url, m[0], m[1], "d-"+m[0])
				}

				// Group one is ready first: the receive takes its first two and locks it.
				flying := ca.receive(url, map[string]any{paramMaxMessages: 2, paramVisibilityTimeout: 100})
				require.Equal(t, []string{"m1", "m2"}, bodiesOf(flying))

				b := restoredStack(t, newSettings(), snapshotOf(t, a.provider))
				cb := b.as(protocol)

				assert.Equal(t, wantCounts("4", "2", "0"), cb.counts(url))

				// Group one stays locked by its in-flight messages, so m3 waits behind them.
				// The others come in the order their groups became ready.
				got := cb.receive(url, map[string]any{paramMaxMessages: 10, paramVisibilityTimeout: 100})
				assert.Equal(t, []string{"n1", "n2", "o1"}, bodiesOf(got), "m3 is held by its group's lock")

				// A duplicate of a message sent before the snapshot is not queued.
				again := cb.sendTo(url, "m1 again", groupOne, "d-m1")
				assert.Equal(t, sent["m1"].str(keyMessageID), again.str(keyMessageID))
				assert.Equal(t, sent["m1"].str(paramSequence), again.str(paramSequence))
				assert.Equal(t, wantCounts("1", "5", "0"), cb.counts(url), "nothing was added")

				// A receipt handle given before the snapshot still deletes.
				for _, m := range flying {
					res := cb.deleteMessage(url, m.handle)
					require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())
				}

				// The last in-flight message of the group was deleted, which opens it.
				assert.Equal(t, []string{"m3"}, bodiesOf(cb.receive(url, nil)))

				// A new message's sequence number is above every number before it.
				fresh := cb.sendTo(url, "fresh", groupOne, "d-fresh")
				for body, res := range sent {
					assert.Greater(t, sequenceOf(t, fresh), sequenceOf(t, res), body)
				}

				// The window runs out at the time it was sent, whatever the restart did.
				time.Sleep(5 * minute)

				expired := cb.sendTo(url, "m1 later", groupOne, "d-m1")
				assert.NotEqual(t, sent["m1"].str(keyMessageID), expired.str(keyMessageID), "the window is over")
			})
		})
	}
}

func Test_a_FIFO_group_gets_its_in_flight_messages_back_in_order(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newStack(t)
		ca := a.as(protoJSON)
		url := ca.createFIFO(nil)

		ca.sendTo(url, "m1", groupOne, "d1")
		ca.sendTo(url, "m2", groupOne, "d2")

		flying := ca.receive(url, map[string]any{paramMaxMessages: 2, paramVisibilityTimeout: 100})
		require.Equal(t, []string{"m1", "m2"}, bodiesOf(flying))

		// m2's timeout ends first, but it stays hidden until m1 comes back.
		require.True(t, ca.changeVisibility(url, flying[1].handle, 10).ok())

		b := restoredStack(t, newSettings(), snapshotOf(t, a.provider))
		cb := b.as(protoJSON)

		time.Sleep(50 * time.Second)
		assert.Equal(t, wantCounts("0", "2", "0"), cb.counts(url), "m2 waits behind m1")

		time.Sleep(50 * time.Second)

		assert.Equal(t, []string{"m1", "m2"}, bodiesOf(cb.receive(url, map[string]any{paramMaxMessages: 10})))
	})
}

func Test_a_delayed_FIFO_message_comes_after_a_snapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newStack(t)
		ca := a.as(protoJSON)
		url := ca.createFIFO(map[string]string{"DelaySeconds": "30"})

		ca.sendTo(url, "late", groupOne, "d1")

		b := restoredStack(t, newSettings(), snapshotOf(t, a.provider))
		cb := b.as(protoJSON)

		assert.Equal(t, wantCounts("0", "0", "1"), cb.counts(url))
		assert.Empty(t, cb.receive(url, nil))

		time.Sleep(30 * time.Second)

		assert.Equal(t, []string{"late"}, bodiesOf(cb.receive(url, nil)))
	})
}

func Test_a_running_move_task_is_failed_after_a_snapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newStack(t)
		ca := a.as(protoJSON)
		pipe := newPipe(t, ca, "source")
		destination := ca.createQueue("destination", nil)
		pipe.deadLetter(ca, "m0", "m1", "m2", "m3")

		started := ca.startMove(map[string]any{
			paramSourceArn: pipe.deadARN, paramDestination: ca.arnOf(destination), paramRate: 1,
		})
		require.True(t, started.ok())
		synctest.Wait()

		running := ca.listMoves(pipe.deadARN)
		require.Len(t, running, 1)
		require.Equal(t, statusRunning, running[0][keyStatus])

		snapshot := snapshotOf(t, a.provider)

		// The task finishes in the original, which is what a later snapshot shows.
		time.Sleep(5 * time.Second)
		synctest.Wait()

		finished := snapshotOf(t, a.provider)

		b := restoredStack(t, newSettings(), snapshot)
		cb := b.as(protoQuery)

		tasks := cb.listMoves(pipe.deadARN)
		require.Len(t, tasks, 1)
		assert.Equal(t, statusFailed, tasks[0][keyStatus], "its goroutine is gone")
		assert.Equal(t, reasonRestarted, tasks[0]["FailureReason"])
		assert.Equal(t, "1", tasks[0]["ApproximateNumberOfMessagesMoved"])
		assert.Equal(t, "4", tasks[0]["ApproximateNumberOfMessagesToMove"])
		assert.Equal(t, ca.arnOf(destination), tasks[0][paramDestination])
		assert.Empty(t, tasks[0][paramTaskHandle], "only a running task has a handle")

		// Nothing moves the rest.
		time.Sleep(10 * time.Second)
		synctest.Wait()
		assert.Equal(t, wantCounts("3", "0", "0"), cb.counts(pipe.dead))

		// The source is free for a new task, which moves what is left.
		again := cb.startMove(map[string]any{paramSourceArn: pipe.deadARN, paramDestination: ca.arnOf(destination)})
		require.True(t, again.ok(), "%s %s", again.errorCode(), again.errorMessage())

		time.Sleep(time.Second)
		synctest.Wait()

		assert.Equal(t, wantCounts("0", "0", "0"), cb.counts(pipe.dead))

		// A task that had ended is as it was.
		c := restoredStack(t, newSettings(), finished).as(protoJSON)

		done := c.listMoves(pipe.deadARN)
		require.Len(t, done, 1)
		assert.Equal(t, statusCompleted, done[0][keyStatus])
		assert.Equal(t, "4", done[0]["ApproximateNumberOfMessagesMoved"])
		assert.Empty(t, done[0]["FailureReason"])
	})
}
