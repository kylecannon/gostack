package sqs

import (
	"cmp"
	"slices"
	"time"
)

// failureRestarted is the FailureReason of a task that was running when gostack
// stopped. Its goroutine did not survive, so the task cannot go on.
const failureRestarted = "ServiceRestarted"

// moveTaskData is a message move task as a snapshot holds it.
type moveTaskData struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Destination string    `json:"destination,omitempty"`
	Rate        *int32    `json:"rate,omitempty"`
	Started     time.Time `json:"started"`
	Order       uint64    `json:"order"`
	Status      string    `json:"status"`
	Moved       int64     `json:"moved"`
	ToMove      int64     `json:"toMove"`
	Failure     string    `json:"failure,omitempty"`
}

// saveMoveTasks reads the tasks of the State, in the order they were started.
// The caller holds the State's lock.
func (s *State) saveMoveTasks() []moveTaskData {
	saved := make([]moveTaskData, 0, len(s.moveTasks))

	for _, t := range s.moveTasks {
		t.mu.Lock()
		saved = append(saved, moveTaskData{
			ID: t.id, Source: t.source, Destination: t.destination, Rate: t.rate, Started: t.started,
			Order: t.order, Status: t.status, Moved: t.moved, ToMove: t.toMove, Failure: t.failure,
		})
		t.mu.Unlock()
	}

	slices.SortFunc(saved, func(a, b moveTaskData) int { return cmp.Compare(a.Order, b.Order) })

	return saved
}

// task makes the task a snapshot holds. A task that was still going is not: no
// goroutine moves messages for it any more, and LocalStack, which keeps its
// tasks in a pickled store, would show it as running for good. gostack shows a
// task that was running as FAILED, so a new task can start on its source, and
// one that was being cancelled as CANCELLED, which is where the cancel was
// taking it. The goroutine is not started again, as the messages it would move
// may be gone and the caller who asked for the task is too.
func (d *moveTaskData) task() *moveTask {
	status, failure := d.Status, d.Failure

	switch status {
	case moveRunning:
		status, failure = moveFailed, failureRestarted
	case moveCancelling:
		status = moveCancelled
	default:
		// A task that had ended is as it was.
	}

	return &moveTask{
		id: d.ID, source: d.Source, destination: d.Destination, rate: d.Rate, started: d.Started, order: d.Order,
		cancel: func() {}, status: status, moved: d.Moved, toMove: d.ToMove, failure: failure,
	}
}
