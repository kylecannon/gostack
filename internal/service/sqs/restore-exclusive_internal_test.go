package sqs

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are SQ-53 finding 5: Restore, which POST /_gostack/state/load
// runs at any time, shared the move tasks' WaitGroup with the calls that start
// them, and nothing kept a task from putting a message on a queue it had just
// restored.

const (
	exclusiveMessages = 30
	exclusiveBody     = "d"
	exclusiveWait     = 5 * time.Second
)

// deadLetterSnapshot is the snapshot of a cutProvider whose dead-letter queue
// holds n messages, which a task moves to dst.
func deadLetterSnapshot(t *testing.T, c *cutProvider, n int) []byte {
	t.Helper()

	for i := range n {
		c.send(t, c.deadURL, exclusiveBody+strconv.Itoa(i))
	}

	var buf bytes.Buffer

	require.NoError(t, c.p.Snapshot(&buf))

	return buf.Bytes()
}

// startTask starts a move task from dead to dst, at most rate messages a second.
func (c *cutProvider) startTask(ctx context.Context, rate int32) error {
	source, destination := cutDeadARN, cutDstARN

	_, err := c.p.StartMessageMoveTask(ctx, &sqsapi.StartMessageMoveTaskInput{
		SourceArn: &source, DestinationArn: &destination, MaxNumberOfMessagesPerSecond: &rate,
	})

	return err
}

// requireOnce fails unless every one of the n messages of the dead-letter queue
// is in exactly one queue.
func (c *cutProvider) requireOnce(t *testing.T, n int) {
	t.Helper()

	bodies, err := c.saved()
	require.NoError(t, err)

	for i := range n {
		body := exclusiveBody + strconv.Itoa(i)

		assert.Len(t, onlyOnce(bodies, body), 1, "%s is in %v", body, onlyOnce(bodies, body))
	}
}

// Test_Restore_and_StartMessageMoveTask_in_a_tight_loop is the probe H1 of the
// SQ-53 review. Before the fix Restore panicked with "WaitGroup is reused before
// previous Wait has returned" in about one run of five, the race detector found
// the WaitGroup in one of three, and a task that Restore had not stopped put its
// message on the restored queue as well.
func Test_Restore_and_StartMessageMoveTask_in_a_tight_loop(t *testing.T) {
	const restores = 60

	c := newCutProvider(t)
	saved := deadLetterSnapshot(t, c, exclusiveMessages)
	ctx := callContext(t)

	stop := make(chan struct{})

	var starters sync.WaitGroup

	for range 2 {
		starters.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					// The call is refused while a task is running: only the ones that
					// are not count as started.
					_ = c.startTask(ctx, 1)
				}
			}
		})
	}

	slowest := time.Duration(0)

	for range restores {
		began := time.Now()

		require.NoError(t, c.p.Restore(bytes.NewReader(saved)))

		slowest = max(slowest, time.Since(began))
	}

	close(stop)
	starters.Wait()

	assert.Less(t, slowest, exclusiveWait, "no task starts while Restore waits for the others, so the wait ends")

	// A restore puts back every message, and a task that was running during it is
	// stopped before it can put one on the restored queues.
	c.p.Close()
	c.requireOnce(t, exclusiveMessages)
}

// churn is the load of the probe H2: snapshots and restores take turns, as
// persist.Engine's flush lock makes them, while messages move.
type churn struct {
	t        *testing.T
	c        *cutProvider
	messages int
	turn     sync.Mutex // persist.Engine.flushMu
	last     atomic.Pointer[[]byte]
}

// snapshots takes a snapshot again and again, and checks that it holds every
// message once.
func (h *churn) snapshots(ctx context.Context) {
	for ctx.Err() == nil {
		var buf bytes.Buffer

		h.turn.Lock()
		err := h.c.p.Snapshot(&buf)
		h.turn.Unlock()

		if err != nil {
			h.t.Errorf("snapshot: %v", err)

			return
		}

		bodies, err := bodiesOf(buf.Bytes())
		if err != nil {
			h.t.Errorf("read the snapshot: %v", err)

			return
		}

		for i := range h.messages {
			body := "b" + strconv.Itoa(i)
			assert.Len(h.t, onlyOnce(bodies, body), 1, "%s is in %v", body, onlyOnce(bodies, body))
		}

		raw := buf.Bytes()
		h.last.Store(&raw)
	}
}

// restores puts the latest snapshot back every interval.
func (h *churn) restores(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		time.Sleep(interval)

		raw := h.last.Load()
		if raw == nil {
			continue
		}

		h.turn.Lock()
		err := h.c.p.Restore(bytes.NewReader(*raw))
		h.turn.Unlock()

		if err != nil {
			h.t.Errorf("restore: %v", err)

			return
		}
	}
}

// Test_Restore_and_message_moves_keep_every_message_once is the probe H2: dead
// letter moves, a move task, snapshots and a restore every 20ms, all at once.
func Test_Restore_and_message_moves_keep_every_message_once(t *testing.T) {
	const (
		messages = 120
		duration = time.Second
		every    = 20 * time.Millisecond
	)

	c := newCutProvider(t)
	call := callContext(t)

	for i := range messages {
		c.send(t, c.workURL, "b"+strconv.Itoa(i))
	}

	ctx, cancel := context.WithTimeout(t.Context(), duration)
	defer cancel()

	h := &churn{t: t, c: c, messages: messages}

	var workers sync.WaitGroup

	// Every receive of a message that is already received once sends it to dead.
	for range 4 {
		workers.Go(func() {
			for ctx.Err() == nil {
				_, err := c.p.ReceiveMessage(call, &sqsapi.ReceiveMessageInput{
					QueueUrl: &c.workURL, VisibilityTimeout: new(int32(0)), MaxNumberOfMessages: new(int32(maxReceiveCount)),
				})
				if err != nil {
					t.Errorf("receive: %v", err)

					return
				}
			}
		})
	}

	workers.Go(func() {
		for ctx.Err() == nil {
			_ = c.startTask(call, 0)

			time.Sleep(time.Millisecond)
		}
	})

	workers.Go(func() { h.snapshots(ctx) })
	workers.Go(func() { h.restores(ctx, every) })

	workers.Wait()
	c.p.Close()

	bodies, err := c.saved()
	require.NoError(t, err)

	for i := range messages {
		body := "b" + strconv.Itoa(i)

		assert.Len(t, onlyOnce(bodies, body), 1, "%s is in %v", body, onlyOnce(bodies, body))
	}
}

// Test_StartMessageMoveTask_waits_for_a_restore: a start that arrives while
// Restore holds the transfer lock answers after it, on the restored state, so no
// task starts for Restore to miss.
func Test_StartMessageMoveTask_waits_for_a_restore(t *testing.T) {
	c := newCutProvider(t)
	ctx := callContext(t)

	c.p.transfer.Lock()

	started := make(chan error, 1)

	go func() { started <- c.startTask(ctx, 1) }()

	select {
	case err := <-started:
		c.p.transfer.Unlock()
		t.Fatalf("a task started during a restore: %v", err)
	case <-time.After(cutStillWait):
	}

	c.p.transfer.Unlock()

	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(exclusiveWait):
		t.Fatal("the start did not run after the restore")
	}
}

// Test_CancelMessageMoveTask_waits_for_a_restore is the same for a cancel.
func Test_CancelMessageMoveTask_waits_for_a_restore(t *testing.T) {
	c := newCutProvider(t)
	ctx := callContext(t)

	c.send(t, c.deadURL, exclusiveBody)
	require.NoError(t, c.startTask(ctx, 1))

	c.p.transfer.Lock()

	handle := newTaskHandle(t, c)
	cancelled := make(chan error, 1)

	go func() {
		_, err := c.p.CancelMessageMoveTask(ctx, &sqsapi.CancelMessageMoveTaskInput{TaskHandle: &handle})
		cancelled <- err
	}()

	select {
	case err := <-cancelled:
		c.p.transfer.Unlock()
		t.Fatalf("a task was cancelled during a restore: %v", err)
	case <-time.After(cutStillWait):
	}

	c.p.transfer.Unlock()

	select {
	case err := <-cancelled:
		require.NoError(t, err)
	case <-time.After(exclusiveWait):
		t.Fatal("the cancel did not run after the restore")
	}
}

// newTaskHandle is the handle of the only task of the provider.
func newTaskHandle(t *testing.T, c *cutProvider) string {
	t.Helper()

	out, err := c.p.ListMessageMoveTasks(callContext(t), &sqsapi.ListMessageMoveTasksInput{SourceArn: new(cutDeadARN)})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	require.NotNil(t, out.Results[0].TaskHandle)

	return *out.Results[0].TaskHandle
}

// Test_Restore_does_not_wait_for_a_task_that_waits_for_it: a task that is about
// to take the transfer lock while Restore waits for the writer's turn must give
// up when Restore cancels it, or the two would wait for each other for good.
func Test_Restore_does_not_wait_for_a_task_that_waits_for_it(t *testing.T) {
	// So many that the task is still going when Restore gets its turn.
	const many = 5000

	c := newCutProvider(t)
	saved := deadLetterSnapshot(t, c, many)

	require.NoError(t, c.startTask(callContext(t), 0))
	require.Eventually(t, func() bool {
		c.dst.mu.Lock()
		defer c.dst.mu.Unlock()

		return c.dst.contents.Counts().Visible > 0
	}, exclusiveWait, cutTick, "the task is moving messages")

	// A reader that a dead-letter move would be: Restore queues for the writer
	// behind it, and the task's own attempts at the read lock fail meanwhile.
	c.p.transfer.RLock()

	done := make(chan error, 1)

	go func() { done <- c.p.Restore(bytes.NewReader(saved)) }()

	select {
	case err := <-done:
		c.p.transfer.RUnlock()
		t.Fatalf("Restore ran under a reader: %v", err)
	case <-time.After(cutStillWait):
	}

	c.p.transfer.RUnlock()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(exclusiveWait):
		t.Fatal("Restore and the move task wait for each other")
	}

	c.p.Close()
	c.requireOnce(t, many)
}
