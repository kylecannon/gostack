package sqs

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// The states of a message move task, as ListMessageMoveTasks shows them.
const (
	moveRunning    = "RUNNING"
	moveCompleted  = "COMPLETED"
	moveCancelling = "CANCELLING"
	moveCancelled  = "CANCELLED"
	moveFailed     = "FAILED"
)

// moveVisibility is how long a message stays hidden in the source queue while a
// move task puts it on its destination, as in LocalStack.
const moveVisibility = int32(1)

// Messages of the errors a move task call answers with.
const (
	messageSourceMissing      = "The resource that you specified for the SourceArn parameter doesn't exist."
	messageDestinationMissing = "The resource that you specified for the DestinationArn parameter doesn't exist."
	messageSourceFormat       = "You must use this format to specify the SourceArn: " +
		"arn:<partition>:<service>:<region>:<account-id>:<resource-id>"
)

// moveTask is a message move task: a goroutine that takes messages from a
// dead-letter queue, one at a time, and puts each on its destination or on the
// queue it came from.
type moveTask struct {
	id          string
	source      string
	destination string
	rate        *int32
	started     time.Time
	// order is the creation order in the State, which breaks ties of started.
	order uint64
	// cancel stops the goroutine of the task.
	cancel context.CancelFunc

	mu      sync.Mutex
	status  string
	moved   int64
	toMove  int64
	failure string
}

// handle is the TaskHandle of the task: the base64 of a JSON document with the
// task id and the source ARN, as in LocalStack.
func (t *moveTask) handle() string {
	doc, _ := json.Marshal(map[string]string{"taskId": t.id, "sourceArn": t.source}) //nolint:errchkjson // strings.

	return base64.StdEncoding.EncodeToString(doc)
}

// active reports whether the task still holds its source.
func (t *moveTask) active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.status == moveRunning || t.status == moveCancelling
}

// resourceNotFound is ResourceNotFoundException, which LocalStack answers with
// 404 where the generated constructor says 400.
func resourceNotFound(message string) *awserr.Error {
	err := sqsapi.NewResourceNotFoundException(message)
	err.StatusCode = http.StatusNotFound

	return err
}

// StartMessageMoveTask starts a goroutine that moves the messages of a
// dead-letter queue to the DestinationArn, or without one back to the queues
// they came from, at most MaxNumberOfMessagesPerSecond a second.
func (p *Provider) StartMessageMoveTask(
	ctx context.Context, in *sqsapi.StartMessageMoveTaskInput,
) (*sqsapi.StartMessageMoveTaskOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	source := deref(in.SourceArn)

	sourceQueue, err := p.requireARN(source, messageSourceMissing)
	if err != nil {
		return nil, err
	}

	if !p.isDeadLetterQueue(source) {
		return nil, invalidParameterValue("Source queue must be configured as a Dead Letter Queue.")
	}

	if destination := deref(in.DestinationArn); destination != "" {
		_, err = p.requireARN(destination, messageDestinationMissing)
		if err != nil {
			return nil, err
		}
	}

	taskCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	task := &moveTask{
		id: newRequestID(), source: source, destination: deref(in.DestinationArn), rate: in.MaxNumberOfMessagesPerSecond,
		started: time.Now(), cancel: cancel, status: moveRunning,
	}

	sourceQueue.mu.Lock()
	task.toMove = int64(sourceQueue.contents.Counts().Visible)
	sourceQueue.mu.Unlock()

	err = p.states.Get(rc.AccountID, rc.Region).addMoveTask(task)
	if err != nil {
		cancel()

		return nil, err
	}

	p.tasks.Go(func() {
		defer cancel()

		p.drive(taskCtx, task)
	})

	return &sqsapi.StartMessageMoveTaskOutput{TaskHandle: new(task.handle())}, nil
}

// CancelMessageMoveTask stops a running task. The task is cancelling until its
// goroutine has seen it, and the call answers with what was moved so far.
func (p *Provider) CancelMessageMoveTask(
	ctx context.Context, in *sqsapi.CancelMessageMoveTaskInput,
) (*sqsapi.CancelMessageMoveTaskOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	taskID, source, ok := decodeTaskHandle(deref(in.TaskHandle))
	if !ok {
		return nil, invalidParameterValue("Value for parameter TaskHandle is invalid.")
	}

	_, err = p.requireARN(source, messageSourceMissing)
	if err != nil {
		return nil, err
	}

	task, found := p.states.Get(rc.AccountID, rc.Region).moveTask(taskID)
	if !found {
		return nil, resourceNotFound("Task does not exist.")
	}

	task.mu.Lock()
	defer task.mu.Unlock()

	if task.status == moveRunning {
		task.status = moveCancelling
		task.cancel()
	}

	return &sqsapi.CancelMessageMoveTaskOutput{ApproximateNumberOfMessagesMoved: new(task.moved)}, nil
}

// ListMessageMoveTasks lists the tasks of a source queue, the most recent
// first. Without MaxResults it lists the latest one.
func (p *Provider) ListMessageMoveTasks(
	ctx context.Context, in *sqsapi.ListMessageMoveTasksInput,
) (*sqsapi.ListMessageMoveTasksOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	source := deref(in.SourceArn)
	if _, valid := parseARN(source); !valid {
		return nil, invalidParameterValue(messageSourceFormat)
	}

	_, err = p.requireARN(source, messageSourceMissing)
	if err != nil {
		return nil, err
	}

	tasks := p.states.Get(rc.AccountID, rc.Region).moveTasksOf(source)

	limit := 1
	if in.MaxResults != nil && *in.MaxResults > 0 {
		limit = int(*in.MaxResults)
	}

	out := &sqsapi.ListMessageMoveTasksOutput{}

	for _, task := range tasks[:min(limit, len(tasks))] {
		out.Results = append(out.Results, task.entry())
	}

	return out, nil
}

// entry is the task as ListMessageMoveTasks shows it. Only a running task has a
// handle.
func (t *moveTask) entry() sqsapi.ListMessageMoveTasksResultEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	// The entry outlives the lock, so it points at copies.
	entry := sqsapi.ListMessageMoveTasksResultEntry{
		SourceArn:                         new(t.source),
		Status:                            new(t.status),
		StartedTimestamp:                  new(t.started.UnixMilli()),
		MaxNumberOfMessagesPerSecond:      t.rate,
		ApproximateNumberOfMessagesToMove: new(t.toMove),
		ApproximateNumberOfMessagesMoved:  new(t.moved),
	}

	if t.destination != "" {
		entry.DestinationArn = new(t.destination)
	}

	if t.status == moveRunning {
		entry.TaskHandle = new(t.handle())
	}

	if t.failure != "" {
		entry.FailureReason = new(t.failure)
	}

	return entry
}

// decodeTaskHandle reads the task id and the source ARN out of a TaskHandle.
func decodeTaskHandle(handle string) (taskID, source string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(handle)
	if err != nil {
		return "", "", false
	}

	var doc struct {
		TaskID string `json:"taskId"`
		Source string `json:"sourceArn"`
	}

	err = json.Unmarshal(raw, &doc)

	return doc.TaskID, doc.Source, err == nil && doc.TaskID != "" && doc.Source != ""
}

// requireARN finds the queue an ARN names, or answers with ResourceNotFound and
// the message. An ARN that is not one finds nothing.
func (p *Provider) requireARN(arn, message string) (*Queue, *awserr.Error) {
	q, ok := p.queueByARN(arn)
	if !ok {
		return nil, resourceNotFound(message)
	}

	return q, nil
}

// isDeadLetterQueue reports whether any queue of any account and region has the
// ARN as its dead-letter queue.
func (p *Provider) isDeadLetterQueue(arn string) bool {
	found := false

	p.states.Range(func(_, _ string, state *State) bool {
		state.Range(func(q *Queue) bool {
			found = q.redrivesTo(arn)

			return !found
		})

		return !found
	})

	return found
}

// drive runs a task until its source is empty, it is cancelled or it fails, and
// then records how it ended. It is the only code of the task that runs without
// a call behind it, and it never outlives its context.
func (p *Provider) drive(ctx context.Context, task *moveTask) {
	err := p.moveMessages(ctx, task)

	task.mu.Lock()
	defer task.mu.Unlock()

	switch {
	case err != nil:
		task.status = moveFailed
		task.failure = err.QueryCode

		if task.failure == "" {
			task.failure = err.Code
		}
	case ctx.Err() != nil:
		task.status = moveCancelled
	default:
		task.status = moveCompleted
	}
}

// moveMessages moves one message at a time, looking the queues up again for each
// as LocalStack does in case one was deleted, and pauses between them as the
// rate asks.
func (p *Provider) moveMessages(ctx context.Context, task *moveTask) *awserr.Error {
	for ctx.Err() == nil {
		source, ok := p.queueByARN(task.source)
		if !ok {
			return sqsapi.NewQueueDoesNotExist("The specified queue does not exist.")
		}

		moved, err := p.moveOne(source, task)
		if err != nil || !moved {
			return err
		}

		task.mu.Lock()
		task.moved++
		task.mu.Unlock()

		if task.rate != nil && *task.rate > 0 {
			pause(ctx, time.Second/time.Duration(*task.rate))
		}
	}

	return nil
}

// moveOne takes a message from the source and puts it on its destination. The
// message is deleted from the source only after the put, so a failed move loses
// nothing. It reports whether there was a message.
func (p *Provider) moveOne(source *Queue, task *moveTask) (bool, *awserr.Error) {
	visibility := moveVisibility
	got, _ := source.receiveOnce(receiveParams{count: 1, visibility: &visibility}, 1)

	if len(got.dead) > 0 {
		return false, &awserr.Error{
			Code: "NotImplementedError", Message: "Cannot deal with DLQ chains in move tasks",
			StatusCode: http.StatusNotImplemented, Fault: awserr.FaultReceiver,
		}
	}

	if len(got.taken) == 0 {
		return false, nil
	}

	moving := got.taken[0]

	destination := task.destination
	if destination == "" {
		destination = moving.msg.system[sysDeadLetterQueueSourceArn]
	}

	target, found := p.queueByARN(destination)
	if !found {
		return false, sqsapi.NewQueueDoesNotExist("The specified queue does not exist.")
	}

	err := target.put(moving.msg.moved("", moving.receiveCount, time.Now()), nil)
	if err != nil {
		return false, err
	}

	source.mu.Lock()
	defer source.mu.Unlock()

	return true, source.contents.Delete(moving.handle)
}

// pause waits for d or until ctx is done.
func pause(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// addMoveTask records a task, unless one is already active for its source.
func (s *State) addMoveTask(task *moveTask) *awserr.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, other := range s.moveTasks {
		if other.source == task.source && other.active() {
			return invalidParameterValue("There is already a task running. " +
				"Only one active task is allowed for a source queue arn at a given time.")
		}
	}

	s.moveSeq++
	task.order = s.moveSeq
	s.moveTasks[task.id] = task

	return nil
}

// moveTask returns the task with the id.
func (s *State) moveTask(id string) (*moveTask, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.moveTasks[id]

	return task, ok
}

// moveTasksOf returns the tasks of a source queue, the most recent first.
func (s *State) moveTasksOf(source string) []*moveTask {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tasks []*moveTask

	for _, task := range s.moveTasks {
		if task.source == source {
			tasks = append(tasks, task)
		}
	}

	slices.SortFunc(tasks, func(a, b *moveTask) int { return cmp.Compare(b.order, a.order) })

	return tasks
}

// cancelMoveTasks stops every task of the State.
func (s *State) cancelMoveTasks() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, task := range s.moveTasks {
		task.cancel()
	}
}

// Close stops every message move task and waits for their goroutines. The
// provider serves no more calls that start one afterwards only if its caller
// stops sending them first.
func (p *Provider) Close() {
	p.states.Range(func(_, _ string, state *State) bool {
		state.cancelMoveTasks()

		return true
	})

	p.tasks.Wait()
}
