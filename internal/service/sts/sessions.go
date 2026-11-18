package sts

import (
	"sync"
	"time"
)

// minSweep is how many sessions an account holds before expired ones are
// looked for; it doubles with the live count so a sweep stays amortized.
const (
	minSweep    = 1024
	sweepGrowth = 2
)

// session is the identity that credentials issued by an STS call stand for.
type session struct {
	userID  string
	account string
	arn     string
	expires time.Time
}

// sessions holds the sessions of one account by access key ID.
type sessions struct {
	mu      sync.Mutex
	byKey   map[string]session
	sweepAt int
}

func newSessions() *sessions {
	return &sessions{byKey: make(map[string]session), sweepAt: minSweep}
}

// put records the session of an access key ID. Sessions past their expiry are
// dropped once enough have piled up.
func (s *sessions) put(accessKeyID string, v session, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.byKey) >= s.sweepAt {
		for key, old := range s.byKey {
			if !old.expires.After(now) {
				delete(s.byKey, key)
			}
		}

		s.sweepAt = max(minSweep, sweepGrowth*len(s.byKey))
	}

	s.byKey[accessKeyID] = v
}

// clear forgets every session. It is the emptyFn of the provider's Bundle, so
// it empties the sessions in place.
func (s *sessions) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.byKey = make(map[string]session)
	s.sweepAt = minSweep
}

// get returns the live session of an access key ID.
func (s *sessions) get(accessKeyID string, now time.Time) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.byKey[accessKeyID]
	if !ok || !v.expires.After(now) {
		return session{}, false
	}

	return v, true
}
