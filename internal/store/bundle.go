package store

import (
	"hash/maphash"
	"sync"
)

// shardCount is the number of shards a Bundle splits its keys across.
const shardCount = 16

// key identifies the state of one account in one region. It stands in for the
// string account+"/"+region, so a lookup builds no string.
type key struct {
	account string
	region  string
}

type entry[S any] struct {
	key   key
	state *S
}

type shard[S any] struct {
	mu     sync.RWMutex
	states map[key]*S
}

// Bundle holds one state value S for each account and region, created on first
// use. It is safe for concurrent use. Keys are spread over a fixed set of
// shards, so calls for different keys rarely contend on one lock.
//
// A state, once created, stays in the Bundle for good: Reset empties it in
// place. A call that got a state from Get and writes to it after a reset writes
// to the live, emptied state, never to one the Bundle has let go of.
type Bundle[S any] struct {
	newFn   func(account, region string) *S
	emptyFn func(*S)
	seed    maphash.Seed
	shards  [shardCount]shard[S]
}

// NewBundle returns an empty Bundle that calls newFn to create the state for a
// key the first time it is asked for, and emptyFn to empty a state in place on
// Reset. newFn runs under a shard lock, so it must not call back into the
// Bundle. emptyFn runs under no Bundle lock and takes the state's own lock: what
// it takes out under that lock is gone, and anything added after it is kept.
func NewBundle[S any](newFn func(account, region string) *S, emptyFn func(*S)) *Bundle[S] {
	b := &Bundle[S]{newFn: newFn, emptyFn: emptyFn, seed: maphash.MakeSeed()}
	for i := range b.shards {
		b.shards[i].states = make(map[key]*S)
	}

	return b
}

// Get returns the state for account and region, creating it with the Bundle's
// newFn when it does not exist. For any key, newFn runs once however many
// goroutines ask at the same time.
func (b *Bundle[S]) Get(account, region string) *S {
	k := key{account: account, region: region}
	sh := b.shardFor(k)

	sh.mu.RLock()
	s, ok := sh.states[k]
	sh.mu.RUnlock()

	if ok {
		return s
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	if s, ok = sh.states[k]; ok {
		return s
	}

	s = b.newFn(account, region)
	sh.states[k] = s

	return s
}

// Range calls fn for every state created so far, in no particular order, and
// stops when fn returns false. It works on a snapshot of each shard, so fn may
// call the Bundle; a state created during the walk may or may not be seen.
func (b *Bundle[S]) Range(fn func(account, region string, s *S) bool) {
	for i := range b.shards {
		sh := &b.shards[i]

		sh.mu.RLock()

		entries := make([]entry[S], 0, len(sh.states))
		for k, s := range sh.states {
			entries = append(entries, entry[S]{key: k, state: s})
		}

		sh.mu.RUnlock()

		for _, e := range entries {
			if !fn(e.key.account, e.key.region, e.state) {
				return
			}
		}
	}
}

// Reset empties the states that match account and region, with the Bundle's
// emptyFn. An empty argument matches every value: Reset("", "") empties
// everything and Reset(account, "") empties every region of one account. The
// states stay in the Bundle, so Get returns the same ones afterwards.
func (b *Bundle[S]) Reset(account, region string) {
	b.Range(func(stateAccount, stateRegion string, s *S) bool {
		if (account == "" || stateAccount == account) && (region == "" || stateRegion == region) {
			b.emptyFn(s)
		}

		return true
	})
}

func (b *Bundle[S]) shardFor(k key) *shard[S] {
	return &b.shards[maphash.Comparable(b.seed, k)%shardCount]
}
