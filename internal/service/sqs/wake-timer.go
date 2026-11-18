package sqs

import "time"

// waker is the channel that long polls of a Contents wait on. It is closed, and
// made again on the next ask, when a message may have become visible.
type waker struct {
	ch     chan struct{}
	closed bool
}

// channel returns the channel to wait on, or nil once the waker is shut down.
func (w *waker) channel() <-chan struct{} {
	if w.closed {
		return nil
	}

	if w.ch == nil {
		w.ch = make(chan struct{})
	}

	return w.ch
}

// notify wakes every long poll that asked for the channel.
func (w *waker) notify() {
	if w.ch != nil {
		close(w.ch)
		w.ch = nil
	}
}

// shutdown wakes every long poll for good.
func (w *waker) shutdown() {
	w.closed = true
	w.notify()
}

// deadlineTimer is the one time.Timer of a Contents. It is armed for the
// earliest deadline of the Contents' heap and stopped when there is none, so a
// queue with nothing pending costs no wakeups.
type deadlineTimer struct {
	timer *time.Timer
	// armed is whether timer is set; armedFor is the deadline it is set for.
	armed    bool
	armedFor time.Time
}

// arm sets the timer for the deadline of top, or stops it when top is nil. fire
// is the callback of the timer, which arm makes on the first call.
func (t *deadlineTimer) arm(top *entry, now time.Time, fire func()) {
	if top == nil {
		if t.armed {
			t.timer.Stop()
			t.armed = false
		}

		return
	}

	if t.armed && t.armedFor.Equal(top.deadline) {
		return
	}

	wait := max(top.deadline.Sub(now), 0)

	if t.timer == nil {
		t.timer = time.AfterFunc(wait, fire)
	} else {
		t.timer.Reset(wait)
	}

	t.armed, t.armedFor = true, top.deadline
}

// spent records that the timer fired, so the next arm sets it again.
func (t *deadlineTimer) spent() { t.armed = false }
