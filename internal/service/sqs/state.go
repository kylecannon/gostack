package sqs

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"
)

// recentlyDeletedTimeout is how long a deleted queue's name stays taken when
// SQS_DELAY_RECENTLY_DELETED is on.
const recentlyDeletedTimeout = 60 * time.Second

// State is what SQS holds for one account in one region: its queues, and the
// names it deleted lately. The Provider keeps one per account and region in a
// store.Bundle.
//
// The lock guards the two maps. It is held to find, add or remove a queue and
// never while a queue works, so a call on one queue does not wait for another.
type State struct {
	mu      sync.RWMutex
	queues  map[string]*Queue
	deleted map[string]time.Time
	// next is the creation sequence number of the next queue.
	next uint64
	// moveTasks are the message move tasks by id, and moveSeq counts them.
	moveTasks map[string]*moveTask
	moveSeq   uint64
}

// newState returns an empty State. It is the newFn of the Provider's Bundle.
func newState(string, string) *State {
	return &State{
		queues: make(map[string]*Queue), deleted: make(map[string]time.Time), moveTasks: make(map[string]*moveTask),
	}
}

// Range calls fn for every queue, in creation order, and stops when fn returns
// false. It works on a copy of the list, so fn may take a queue's lock and may
// call back into the State. Persistence reads the state through it.
func (s *State) Range(fn func(*Queue) bool) {
	for _, q := range s.list("") {
		if !fn(q) {
			return
		}
	}
}

// find returns the queue with the given name.
func (s *State) find(name string) (*Queue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q, ok := s.queues[name]

	return q, ok
}

// list returns the queues whose name starts with prefix, in creation order.
func (s *State) list(prefix string) []*Queue {
	s.mu.RLock()

	found := make([]*Queue, 0, len(s.queues))

	for name, q := range s.queues {
		if strings.HasPrefix(name, prefix) {
			found = append(found, q)
		}
	}

	s.mu.RUnlock()

	slices.SortFunc(found, func(a, b *Queue) int { return cmp.Compare(a.seq, b.seq) })

	return found
}

// expireDeleted forgets the deleted names whose delay is over. The caller holds
// the write lock.
func (s *State) expireDeleted(now time.Time) {
	for name, at := range s.deleted {
		if !at.After(now.Add(-recentlyDeletedTimeout)) {
			delete(s.deleted, name)
		}
	}
}

// recentlyDeleted reports whether the name was deleted less than a minute ago.
// The caller holds the lock.
func (s *State) recentlyDeleted(name string, now time.Time) bool {
	at, ok := s.deleted[name]

	return ok && at.After(now.Add(-recentlyDeletedTimeout))
}

// add stores a new queue. The caller holds the write lock and has made the queue
// with the sequence number from nextSeq.
func (s *State) add(q *Queue) {
	s.queues[q.name] = q
}

// nextSeq returns the creation sequence number for the next queue. The caller
// holds the write lock.
func (s *State) nextSeq() uint64 {
	s.next++

	return s.next
}
