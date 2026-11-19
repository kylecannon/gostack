package lifecycle

import (
	"context"
	"time"
)

const (
	// KillMargin is the least time between the end of the save and the
	// supervisor's SIGKILL. The supervisor starts its timer when it sends SIGTERM,
	// and the child starts its own when it sees the signal, so the margin covers
	// that delay and the exit itself.
	KillMargin = 500 * time.Millisecond

	// CloseMargin is the least time between the end of the services' Close and the
	// supervisor's SIGKILL. A Close still running then is abandoned and the
	// process exits, but not at once: the caller waits a short grace past the
	// deadline (cmd/gostack's closeGrace, 20 ms) for a Close that honours its
	// context to return, so an abandoned Close leaves 80 ms of the margin, not
	// 100. That is enough to exit, which takes microseconds after the warning,
	// and the state was written by the save before Close began.
	CloseMargin = 100 * time.Millisecond

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
	// Save ends the save of persisted state. It is KillMargin before the
	// supervisor's SIGKILL.
	Save time.Time
	// Close ends the services' Close functions, and the shutdown with them. It
	// falls after Save, CloseMargin before the supervisor's SIGKILL, so a slow
	// Close cannot take time from the save.
	Close time.Time
}

// PlanShutdown splits timeout, the SHUTDOWN_TIMEOUT the supervisor gives its
// child between SIGTERM and SIGKILL, for a shutdown that starts at start. The
// budget has four slots, one after the other: hooks, drain, save and Close.
//
//	start          Work                 Save    Close  start+timeout
//	  | hooks, drain |  save (reserved)  | Close | kill |
//
// The save is reserved first. Hooks and drain end at Work, however long they
// take, so state is written before the kill even when they overrun. The
// services' Close functions get what is left after the save. For a timeout T:
//
//	Save  = start + T - KillMargin    (start + T/4 when T <= KillMargin)
//	Work  = Save - reserve            the reserve is a quarter of the time before
//	                                  Save, at least 500 ms when the time allows,
//	                                  at most half
//	Close = start + T - CloseMargin   (Save, when T is too short to leave room)
//
// A timeout of zero or less is no limit, as it is for the supervisor: every
// deadline is zero, so the hooks, the drain, the save and Close all wait.
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

	closeBy := start.Add(timeout - CloseMargin)
	if closeBy.Before(save) {
		closeBy = save
	}

	return Deadlines{Work: save.Add(-reserve), Save: save, Close: closeBy}
}

// Until returns ctx limited to deadline, or ctx itself when deadline is zero.
func Until(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return ctx, func() {}
	}

	return context.WithDeadline(ctx, deadline)
}
