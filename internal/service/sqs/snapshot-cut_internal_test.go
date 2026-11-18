package sqs

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are the cut of SQ-41: a snapshot holds every message in one queue
// of the two that a move is carrying it between, and a restore leaves no move
// task behind to put a message on a restored queue.

const (
	cutAccount   = "000000000000"
	cutRegion    = "us-east-1"
	cutDeadARN   = "arn:aws:sqs:us-east-1:000000000000:dead"
	cutDstARN    = "arn:aws:sqs:us-east-1:000000000000:dst"
	cutRedrive   = `{"deadLetterTargetArn":"` + cutDeadARN + `","maxReceiveCount":1}`
	cutPrecious  = "precious"
	cutStillWait = 150 * time.Millisecond
	cutTick      = 5 * time.Millisecond
	cutPace      = 500 * time.Microsecond
)

// cutProvider is a provider with the queues of a dead-letter move: work sends
// what it receives twice to dead, and a move task takes from dead to dst.
type cutProvider struct {
	p                *Provider
	work, dead, dst  *Queue
	workURL, deadURL string
}

func newCutProvider(t *testing.T) *cutProvider {
	t.Helper()

	p := New(nil)
	t.Cleanup(p.Close)

	ctx := callContext(t)
	create := func(name string, attrs map[string]string) (*Queue, string) {
		out, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: &name, Attributes: attrs})
		require.NoError(t, err)

		q, found := p.states.Get(cutAccount, cutRegion).find(name)
		require.True(t, found)

		return q, *out.QueueUrl
	}

	c := &cutProvider{p: p}
	c.dead, c.deadURL = create("dead", nil)
	c.dst, _ = create("dst", nil)
	c.work, c.workURL = create("work", map[string]string{attrRedrivePolicy: cutRedrive})

	return c
}

func (c *cutProvider) send(t *testing.T, url, body string) {
	t.Helper()

	_, err := c.p.SendMessage(callContext(t), &sqsapi.SendMessageInput{QueueUrl: &url, MessageBody: &body})
	require.NoError(t, err)
}

// saved is the bodies of the messages of every queue in a snapshot, by queue.
func (c *cutProvider) saved() (map[string][]string, error) {
	var buf bytes.Buffer

	err := c.p.Snapshot(&buf)
	if err != nil {
		return nil, err
	}

	return bodiesOf(buf.Bytes())
}

func bodiesOf(raw []byte) (map[string][]string, error) {
	var file snapshotFile

	err := json.Unmarshal(raw, &file, snapshotOptions)
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}

	bodies := make(map[string][]string)

	for i := range file.Data.States {
		for j := range file.Data.States[i].Queues {
			queue := &file.Data.States[i].Queues[j]
			bodies[queue.Name] = nil

			for _, entry := range queue.Contents.Entries {
				bodies[queue.Name] = append(bodies[queue.Name], entry.Message.Body)
			}
		}
	}

	return bodies, nil
}

// onlyOnce reports where a body is found: every queue of the snapshot that
// holds it.
func onlyOnce(bodies map[string][]string, body string) []string {
	var found []string

	for name, list := range bodies {
		for _, one := range list {
			if one == body {
				found = append(found, name)
			}
		}
	}

	return found
}

// asyncSnapshot is a snapshot taken in a goroutine.
type asyncSnapshot struct {
	done     atomic.Bool
	finished chan struct{}
	bodies   map[string][]string
	err      error
}

// snapshotAsync starts a snapshot.
func (c *cutProvider) snapshotAsync() *asyncSnapshot {
	s := &asyncSnapshot{finished: make(chan struct{})}

	go func() {
		defer close(s.finished)

		s.bodies, s.err = c.saved()

		s.done.Store(true)
	}()

	return s
}

// result waits for the snapshot and returns the bodies it holds.
func (s *asyncSnapshot) result(t *testing.T) map[string][]string {
	t.Helper()

	<-s.finished
	require.NoError(t, s.err)

	return s.bodies
}

// Test_a_snapshot_waits_for_a_dead_letter_move is SQ-41 finding 1 for a
// dead-letter move: the message has left its queue and is not on the dead-letter
// queue yet, and a snapshot taken then holds it nowhere.
func Test_a_snapshot_waits_for_a_dead_letter_move(t *testing.T) {
	c := newCutProvider(t)
	c.send(t, c.workURL, cutPrecious)

	ctx := callContext(t)
	rc, err := callOf(ctx)
	require.Nil(t, err)

	// The first receive counts, the second finds the message past the limit.
	zero := int32(0)
	_, err2 := c.p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: &c.workURL, VisibilityTimeout: &zero})
	require.NoError(t, err2)

	inMove, release := make(chan struct{}), make(chan struct{})
	moved := make(chan struct{})

	go func() {
		defer close(moved)

		_, _, failure := c.work.poll(ctx, receiveParams{count: 1, visibility: &zero}, &c.p.transfer,
			func(dead []deadLetter) *awserr.Error {
				close(inMove)
				<-release

				return c.p.deadLetter(rc, c.work, dead)
			})
		assert.Nil(t, failure)
	}()

	<-inMove

	// The message is in neither queue now.
	c.work.mu.Lock()
	assert.Zero(t, c.work.contents.Counts(), "the message left its queue")
	c.work.mu.Unlock()

	snapshot := c.snapshotAsync()

	require.Never(t, snapshot.done.Load, cutStillWait, cutTick, "the snapshot waits for the move")

	close(release)
	<-moved

	assert.Equal(t, []string{"dead"}, onlyOnce(snapshot.result(t), cutPrecious))
}

// Test_a_snapshot_waits_for_a_move_task_message covers the move task's half of
// the cut: the message was put on the destination and is still on the source, or
// the other way round, and the snapshot has to see it in one.
func Test_a_snapshot_waits_for_a_move_task_message(t *testing.T) {
	c := newCutProvider(t)
	c.send(t, c.deadURL, cutPrecious)

	// With the destination's lock held, the task has taken the message from its
	// source and cannot put it anywhere.
	c.dst.mu.Lock()

	task := &moveTask{source: cutDeadARN, destination: cutDstARN}
	moved := make(chan struct{})

	go func() {
		defer close(moved)

		found, failure := c.p.moveOne(c.dead, task)
		assert.True(t, found)
		assert.Nil(t, failure)
	}()

	require.Eventually(t, func() bool {
		c.dead.mu.Lock()
		defer c.dead.mu.Unlock()

		return c.dead.contents.Counts().NotVisible == 1
	}, time.Second, cutTick, "the task took the message from its source")

	snapshot := c.snapshotAsync()

	require.Never(t, snapshot.done.Load, cutStillWait, cutTick, "the snapshot waits for the message")

	c.dst.mu.Unlock()
	<-moved

	assert.Equal(t, []string{"dst"}, onlyOnce(snapshot.result(t), cutPrecious))
}

// Test_snapshots_of_moving_messages_hold_every_message_once is the stress
// version of the two above, under -race: dead-letter moves, move tasks and
// snapshots all at once, and every snapshot holds every message in exactly one
// queue.
func Test_snapshots_of_moving_messages_hold_every_message_once(t *testing.T) {
	const (
		messages = 600
		workers  = 3
	)

	c := newCutProvider(t)

	for i := range messages {
		c.send(t, c.workURL, "m"+strconv.Itoa(i))
	}

	var (
		wg      sync.WaitGroup
		working atomic.Int32
	)

	working.Store(workers)

	zero := int32(0)
	batch := int32(1)

	for range workers {
		wg.Go(func() {
			defer working.Add(-1)

			for {
				out, err := c.p.ReceiveMessage(callContext(t), &sqsapi.ReceiveMessageInput{
					QueueUrl: &c.workURL, VisibilityTimeout: &zero, MaxNumberOfMessages: &batch,
				})
				if err != nil {
					t.Errorf("receive: %v", err)

					return
				}

				if len(out.Messages) == 0 {
					return
				}

				// Slow enough for snapshots to land between the moves.
				time.Sleep(cutPace)
			}
		})
	}

	wg.Go(func() {
		for working.Load() > 0 || c.deadCount() > 0 {
			// A task that is still going refuses another, which is fine.
			_, _ = c.p.StartMessageMoveTask(callContext(t), &sqsapi.StartMessageMoveTaskInput{
				SourceArn: new(cutDeadARN), DestinationArn: new(cutDstARN),
			})

			time.Sleep(time.Millisecond)
		}
	})

	check := func() {
		t.Helper()

		bodies, err := c.saved()
		require.NoError(t, err)

		where := make(map[string][]string, messages)

		for name, list := range bodies {
			for _, body := range list {
				where[body] = append(where[body], name)
			}
		}

		for i := range messages {
			if queues := where["m"+strconv.Itoa(i)]; len(queues) != 1 {
				assert.Failf(t, "a message is not in exactly one queue", "m%d is in %v", i, queues)
			}
		}
	}

	snapshots := 0

	for working.Load() > 0 || c.deadCount() > 0 {
		check()

		snapshots++
	}

	wg.Wait()
	check()

	assert.Positive(t, snapshots)
	t.Logf("%d snapshots while %d messages moved", snapshots, messages)
}

// deadCount is how many messages the dead-letter queue holds.
func (c *cutProvider) deadCount() int {
	c.dead.mu.Lock()
	defer c.dead.mu.Unlock()

	counts := c.dead.contents.Counts()

	return counts.Visible + counts.NotVisible + counts.Delayed
}

// Test_Restore_waits_for_a_running_move_task is SQ-41 finding 3: a task that was
// moving a message when the state was replaced put it on whichever queue of that
// ARN was there by then, a restored one, and left a duplicate.
func Test_Restore_waits_for_a_running_move_task(t *testing.T) {
	const messages = 40

	c := newCutProvider(t)

	for i := range messages {
		c.send(t, c.deadURL, "m"+strconv.Itoa(i))
	}

	var saved bytes.Buffer

	require.NoError(t, c.p.Snapshot(&saved))

	rate := int32(20)
	_, err := c.p.StartMessageMoveTask(callContext(t), &sqsapi.StartMessageMoveTaskInput{
		SourceArn: new(cutDeadARN), DestinationArn: new(cutDstARN), MaxNumberOfMessagesPerSecond: &rate,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return c.deadCount() < messages
	}, time.Second, cutTick, "the task moves its first message")

	// With the task's lock held, the task's goroutine stops after its next move,
	// where it counts the message, and stays there: it is alive, with a message
	// it put on the old destination, while the state is replaced.
	var task *moveTask

	state := c.p.states.Get(cutAccount, cutRegion)

	state.mu.RLock()

	for _, one := range state.moveTasks {
		task = one
	}

	state.mu.RUnlock()
	require.NotNil(t, task)

	task.mu.Lock()

	time.Sleep(3 * time.Second / time.Duration(rate))

	restored := make(chan error, 1)

	go func() { restored <- c.p.Restore(bytes.NewReader(saved.Bytes())) }()

	select {
	case err = <-restored:
		task.mu.Unlock()
		require.Failf(t, "Restore did not wait for the task", "it returned %v", err)
	case <-time.After(cutStillWait):
	}

	task.mu.Unlock()
	require.NoError(t, <-restored)

	bodies, err := c.saved()
	require.NoError(t, err)

	assert.Empty(t, bodies["dst"], "nothing the task moved is on the restored destination")
	assert.Len(t, bodies["dead"], messages, "the restored source holds everything it held")

	for i := range messages {
		assert.Len(t, onlyOnce(bodies, "m"+strconv.Itoa(i)), 1, "m%d", i)
	}
}

// Test_a_snapshot_round_trips_through_the_streaming_encoder pins the shape of the file
// the cut writes, so that a restore of it needs no other.
func Test_a_snapshot_round_trips_through_the_streaming_encoder(t *testing.T) {
	c := newCutProvider(t)
	c.send(t, c.workURL, cutPrecious)

	var first, second bytes.Buffer

	require.NoError(t, c.p.Snapshot(&first))

	other := New(nil)
	t.Cleanup(other.Close)
	require.NoError(t, other.Restore(bytes.NewReader(first.Bytes())))
	require.NoError(t, other.Snapshot(&second))

	assert.JSONEq(t, first.String(), second.String())
}
