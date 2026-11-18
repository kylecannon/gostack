package sqs

import (
	"errors"
	"time"
)

// lockHoldRuns is how many times QueueLockHold copies the queue.
const lockHoldRuns = 3

// QueueLockHold is how long a save holds the lock of the provider's only queue
// to copy it, the longest of a few copies. It lets BenchmarkSnapshot, which is
// an external test, report the time that every send and receive on the queue
// waits for each flush.
func QueueLockHold(p *Provider) (time.Duration, error) {
	var held time.Duration

	found := false

	p.states.Range(func(_, _ string, state *State) bool {
		state.Range(func(q *Queue) bool {
			found = true

			for range lockHoldRuns {
				start := time.Now()

				_, _ = q.save()

				held = max(held, time.Since(start))
			}

			return false
		})

		return !found
	})

	if !found {
		return 0, errors.New("the provider holds no queue")
	}

	return held, nil
}
