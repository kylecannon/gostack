package sqs_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opStartMove  = "StartMessageMoveTask"
	opCancelMove = "CancelMessageMoveTask"
	opListMoves  = "ListMessageMoveTasks"

	paramSourceArn   = "SourceArn"
	paramDestination = "DestinationArn"
	paramRate        = "MaxNumberOfMessagesPerSecond"
	paramTaskHandle  = "TaskHandle"
	paramMaxResults  = "MaxResults"
	messageNoSource  = "The resource that you specified for the SourceArn parameter doesn't exist."

	elementMoveTask = "ListMessageMoveTasksResultEntry"
	statusRunning   = "RUNNING"
	codeNotFound    = "ResourceNotFoundException"
)

// notFoundStatus is the status of a ResourceNotFoundException. The json protocol
// answers 400 to a caller that does not send x-amzn-query-mode, as it does for
// any client error; the SDKs send it and get 404.
func notFoundStatus(protocol string) int {
	if protocol == protoJSON {
		return http.StatusBadRequest
	}

	return http.StatusNotFound
}

// number reads a numeric member of a result, in the form either protocol gives it.
func (r result) number(name string) string {
	if r.query {
		return r.str(name)
	}

	return jsonText(r.json[name])
}

// pipe is a source queue that dead-letters into a queue, and that queue's ARN.
type pipe struct {
	source, dead       string
	sourceARN, deadARN string
}

// newPipe creates a dead-letter queue and a source queue that sends to it after
// one receive.
func newPipe(t *testing.T, c caller, sourceName string) pipe {
	t.Helper()

	dead := c.createQueue("dead-"+sourceName, nil)
	deadARN := c.arnOf(dead)
	source := c.createQueue(sourceName, map[string]string{attrRedrivePolicy: redrivePolicy(t, deadARN, 1)})

	return pipe{source: source, dead: dead, sourceARN: c.arnOf(source), deadARN: deadARN}
}

// deadLetter sends the bodies to the source and receives each twice, which
// leaves them on the dead-letter queue.
func (p pipe) deadLetter(c caller, bodies ...string) {
	c.s.t.Helper()

	for _, body := range bodies {
		c.send(p.source, body, nil)
	}

	for range 2 * len(bodies) {
		c.receive(p.source, map[string]any{paramVisibilityTimeout: 0})
	}
}

func (c caller) startMove(params map[string]any) result {
	c.s.t.Helper()

	return c.call(opStartMove, params)
}

func (c caller) listMoves(source string) []map[string]string {
	c.s.t.Helper()

	res := c.call(opListMoves, map[string]any{paramSourceArn: source})
	require.True(c.s.t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

	return res.entries("Results", elementMoveTask)
}

func Test_StartMessageMoveTask_errors(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			pipe := newPipe(t, c, "source")
			plain := c.createQueue("plain", nil)

			missing := c.startMove(map[string]any{paramSourceArn: pipe.deadARN + "-gone"})
			assert.Equal(t, notFoundStatus(protocol), missing.status)
			assert.Equal(t, codeNotFound, missing.errorCode())
			assert.Equal(t, messageNoSource, missing.errorMessage())

			notDead := c.startMove(map[string]any{paramSourceArn: c.arnOf(plain), paramDestination: pipe.sourceARN})
			assert.Equal(t, invalidParameterCode(protocol), notDead.errorCode())
			assert.Equal(t, "Source queue must be configured as a Dead Letter Queue.", notDead.errorMessage())

			noDestination := c.startMove(map[string]any{paramSourceArn: pipe.deadARN, paramDestination: pipe.sourceARN + "x"})
			assert.Equal(t, notFoundStatus(protocol), noDestination.status)
			assert.Equal(t, "The resource that you specified for the DestinationArn parameter doesn't exist.",
				noDestination.errorMessage())
		})
	}
}

func Test_CancelMessageMoveTask_errors(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			plain := c.createQueue("plain", nil)
			handle := func(task, source string) string {
				return base64.StdEncoding.EncodeToString([]byte(`{"taskId":"` + task + `","sourceArn":"` + source + `"}`))
			}

			invalid := c.call(opCancelMove, map[string]any{paramTaskHandle: "foobared"})
			assert.Equal(t, http.StatusBadRequest, invalid.status)
			assert.Equal(t, invalidParameterCode(protocol), invalid.errorCode())
			assert.Equal(t, "Value for parameter TaskHandle is invalid.", invalid.errorMessage())

			noSource := c.call(opCancelMove, map[string]any{
				paramTaskHandle: handle("10f57157-fc38-4da9-a113-4de7e12d05dd", "arn:aws:sqs:us-east-1:000000000000:gone"),
			})
			assert.Equal(t, notFoundStatus(protocol), noSource.status)
			assert.Equal(t, messageNoSource, noSource.errorMessage())

			noTask := c.call(opCancelMove, map[string]any{paramTaskHandle: handle("10f57157", c.arnOf(plain))})
			assert.Equal(t, notFoundStatus(protocol), noTask.status)
			assert.Equal(t, "Task does not exist.", noTask.errorMessage())
		})
	}
}

func Test_a_move_task_moves_a_dead_letter_queue_to_a_destination(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				pipe := newPipe(t, c, "source")
				destination := c.createQueue("destination", nil)
				pipe.deadLetter(c, "m0", "m1", "m2", "m3")

				start := time.Now()
				started := c.startMove(map[string]any{
					paramSourceArn: pipe.deadARN, paramDestination: c.arnOf(destination), paramRate: 1,
				})
				require.True(t, started.ok(), "%s %s", started.errorCode(), started.errorMessage())

				synctest.Wait()

				// The first message goes at once, and the task pauses for a second.
				running := c.listMoves(pipe.deadARN)
				require.Len(t, running, 1)
				assert.Equal(t, statusRunning, running[0]["Status"])
				assert.Equal(t, started.str(paramTaskHandle), running[0][paramTaskHandle])
				assert.Equal(t, "1", running[0]["ApproximateNumberOfMessagesMoved"])
				assert.Equal(t, "4", running[0]["ApproximateNumberOfMessagesToMove"])
				assert.Equal(t, "1", running[0][paramRate])
				assert.Equal(t, c.arnOf(destination), running[0][paramDestination])

				again := c.startMove(map[string]any{paramSourceArn: pipe.deadARN, paramDestination: c.arnOf(destination)})
				assert.Equal(t, invalidParameterCode(protocol), again.errorCode())
				assert.Equal(t, "There is already a task running. Only one active task is allowed for a source "+
					"queue arn at a given time.", again.errorMessage())

				time.Sleep(3 * time.Second)
				synctest.Wait()

				assert.Equal(t, 3*time.Second, time.Since(start))
				moved := c.receive(destination, map[string]any{paramMaxMessages: 10})
				assert.Equal(t, []string{"m0", "m1", "m2", "m3"}, bodiesOf(moved))

				time.Sleep(time.Second)
				synctest.Wait()

				done := c.listMoves(pipe.deadARN)
				require.Len(t, done, 1)
				assert.Equal(t, "COMPLETED", done[0]["Status"])
				assert.Equal(t, "4", done[0]["ApproximateNumberOfMessagesMoved"])
				assert.Empty(t, done[0][paramTaskHandle], "only a running task has a handle")
				assert.Empty(t, c.receive(pipe.dead, nil))
			})
		})
	}
}

func Test_a_move_task_without_a_destination_returns_messages_to_their_sources(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				first := newPipe(t, c, "first")

				// A second source for the same dead-letter queue.
				second := c.createQueue("second", map[string]string{attrRedrivePolicy: redrivePolicy(t, first.deadARN, 1)})

				first.deadLetter(c, "from-first")
				c.send(second, "from-second", nil)
				c.receive(second, map[string]any{paramVisibilityTimeout: 0})
				c.receive(second, map[string]any{paramVisibilityTimeout: 0})

				started := c.startMove(map[string]any{paramSourceArn: first.deadARN})
				require.True(t, started.ok(), "%s %s", started.errorCode(), started.errorMessage())

				synctest.Wait()

				assert.Equal(t, []string{"from-first"}, bodiesOf(c.receive(first.source, nil)))
				assert.Equal(t, []string{"from-second"}, bodiesOf(c.receive(second, nil)))

				done := c.listMoves(first.deadARN)
				require.Len(t, done, 1)
				assert.Equal(t, "COMPLETED", done[0]["Status"])
				assert.Empty(t, done[0][paramDestination])
			})
		})
	}
}

func Test_a_move_task_can_be_cancelled(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				pipe := newPipe(t, c, "source")
				destination := c.createQueue("destination", nil)
				pipe.deadLetter(c, "m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9")

				started := c.startMove(map[string]any{
					paramSourceArn: pipe.deadARN, paramDestination: c.arnOf(destination), paramRate: 1,
				})
				require.True(t, started.ok())

				synctest.Wait()
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()

				cancelled := c.call(opCancelMove, map[string]any{paramTaskHandle: started.str(paramTaskHandle)})
				require.True(t, cancelled.ok(), "%s %s", cancelled.errorCode(), cancelled.errorMessage())
				assert.Equal(t, "2", cancelled.number("ApproximateNumberOfMessagesMoved"))

				synctest.Wait()

				after := c.listMoves(pipe.deadARN)
				require.Len(t, after, 1)
				assert.Equal(t, "CANCELLED", after[0]["Status"])
				assert.Equal(t, "2", after[0]["ApproximateNumberOfMessagesMoved"])
				assert.Len(t, c.receive(pipe.dead, map[string]any{paramMaxMessages: 10}), 8, "the rest stays")

				// A task that has ended is not cancelled again, and a new one can start.
				again := c.call(opCancelMove, map[string]any{paramTaskHandle: started.str(paramTaskHandle)})
				assert.True(t, again.ok())
				assert.Equal(t, "CANCELLED", c.listMoves(pipe.deadARN)[0]["Status"])
			})
		})
	}
}

func Test_a_move_task_fails_when_its_destination_is_deleted(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				pipe := newPipe(t, c, "source")
				destination := c.createQueue("destination", nil)
				pipe.deadLetter(c, "m0", "m1", "m2")

				started := c.startMove(map[string]any{
					paramSourceArn: pipe.deadARN, paramDestination: c.arnOf(destination), paramRate: 1,
				})
				require.True(t, started.ok())

				synctest.Wait()
				require.True(t, c.call(opDeleteQueue, map[string]any{paramQueueURL: destination}).ok())
				time.Sleep(time.Second)
				synctest.Wait()

				failed := c.listMoves(pipe.deadARN)
				require.Len(t, failed, 1)
				assert.Equal(t, "FAILED", failed[0]["Status"])
				assert.Equal(t, "AWS.SimpleQueueService.NonExistentQueue", failed[0]["FailureReason"])
				assert.Equal(t, "1", failed[0]["ApproximateNumberOfMessagesMoved"])

				// The message it could not move is still there.
				time.Sleep(time.Second)
				assert.Len(t, c.receive(pipe.dead, map[string]any{paramMaxMessages: 10}), 2)
			})
		})
	}
}

func Test_ListMessageMoveTasks_lists_the_latest_first(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				pipe := newPipe(t, c, "source")
				pipe.deadLetter(c, "m0")

				for range 2 {
					require.True(t, c.startMove(map[string]any{paramSourceArn: pipe.deadARN, paramDestination: pipe.sourceARN}).ok())
					synctest.Wait()
				}

				res := c.call(opListMoves, map[string]any{paramSourceArn: pipe.deadARN, paramMaxResults: 5})
				require.True(t, res.ok())

				tasks := res.entries("Results", elementMoveTask)
				require.Len(t, tasks, 2)

				toMove := make([]int, 0, len(tasks))

				for _, task := range tasks {
					count, err := strconv.Atoi(task["ApproximateNumberOfMessagesToMove"])
					require.NoError(t, err)

					toMove = append(toMove, count)
				}

				assert.Equal(t, []int{0, 1}, toMove, "the second task found the queue empty")
				assert.Len(t, c.listMoves(pipe.deadARN), 1, "one result without MaxResults")

				bad := c.call(opListMoves, map[string]any{paramSourceArn: "not-an-arn"})
				assert.Equal(t, invalidParameterCode(protocol), bad.errorCode())
			})
		})
	}
}
