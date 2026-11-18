package lifecycle

import (
	"context"
	"time"
)

const (
	// KillMargin is the least time between the end of the child's own shutdown
	// and the supervisor's SIGKILL. The supervisor starts its timer when it sends
	// SIGTERM, and the child starts its own when it sees the signal, so the margin
	// covers that delay and the exit itself.
	KillMargin = 500 * time.Millisecond

	// minSaveReserve is the least time kept for saving persisted state.
	minSaveReserve = 500 * time.Millisecond

	// saveShare divides the child's time: the save reserves a quarter of it.
	saveShare = 4
)

// Deadlines are the instants a shutdown keeps. A zero time means no limit.
type Deadlines struct {
	// Work ends the shutdown hooks and the drain of requests in flight. Both
	// share it, in that order, and whatever overruns is cut.
	Work time.Time
	// Save ends the save of persisted state, and the shutdown with it. It is
	// KillMargin before the supervisor's SIGKILL.
	Save time.Time
}

// PlanShutdown splits timeout, the SHUTDOWN_TIMEOUT the supervisor gives its
// child between SIGTERM and SIGKILL, for a shutdown that starts at start:
//
//	start          Work                Save           start+timeout
//	  | hooks, drain |  save (reserved)  | KillMargin |  SIGKILL
//
// The save is reserved first. Hooks and drain end at Work, however long they
// take, so state is written before the kill even when they overrun. The reserve
// is a quarter of the time before Save, and at least half a second when the time
// allows. A timeout of zero or less is no limit, as it is for the supervisor.
func PlanShutdown(start time.Time, timeout time.Duration) Deadlines {
	if timeout <= 0 {
		return Deadlines{}
	}

	budget := timeout - KillMargin
	if budget <= 0 {
		// Only a caller that builds the timeout by hand gets here: SHUTDOWN_TIMEOUT
		// counts whole seconds.
		budget = timeout / saveShare
	}

	reserve := min(max(budget/saveShare, minSaveReserve), budget/2) //nolint:mnd // half of the budget at most

	save := start.Add(budget)

	return Deadlines{Work: save.Add(-reserve), Save: save}
}

// Until returns ctx limited to deadline, or ctx itself when deadline is zero.
func Until(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return ctx, func() {}
	}

	return context.WithDeadline(ctx, deadline)
}
