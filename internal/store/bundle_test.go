package store_test

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kylecannon/gostack/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	usEast1 = "us-east-1"
	euWest1 = "eu-west-1"
)

type state struct {
	account string
	region  string
}

type pair struct{ account, region string }

func newState(account, region string) *state {
	return &state{account: account, region: region}
}

// seeded returns a Bundle that holds every pair, plus the count of newFn calls.
func seeded(pairs ...pair) (*store.Bundle[state], *atomic.Int64) {
	var calls atomic.Int64

	b := store.NewBundle(func(account, region string) *state {
		calls.Add(1)

		return newState(account, region)
	})
	for _, p := range pairs {
		b.Get(p.account, p.region)
	}

	return b, &calls
}

func collect(b *store.Bundle[state]) map[pair]*state {
	got := make(map[pair]*state)

	b.Range(func(account, region string, s *state) bool {
		got[pair{account, region}] = s

		return true
	})

	return got
}

func Test_Bundle_Get(t *testing.T) {
	tests := []struct {
		name      string
		giveCalls []pair
		wantInits int64
	}{
		{name: "one key", giveCalls: []pair{{"111111111111", usEast1}}, wantInits: 1},
		{
			name:      "same key twice",
			giveCalls: []pair{{"111111111111", usEast1}, {"111111111111", usEast1}},
			wantInits: 1,
		},
		{
			name:      "other region",
			giveCalls: []pair{{"111111111111", usEast1}, {"111111111111", euWest1}},
			wantInits: 2,
		},
		{
			name:      "other account",
			giveCalls: []pair{{"111111111111", usEast1}, {"222222222222", usEast1}},
			wantInits: 2,
		},
		{
			name:      "empty account and region are a key like any other",
			giveCalls: []pair{{"", ""}, {"", ""}, {"111111111111", ""}},
			wantInits: 2,
		},
		{
			name:      "slash in a name cannot collide",
			giveCalls: []pair{{"a/b", "c"}, {"a", "b/c"}},
			wantInits: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, calls := seeded()

			seen := make(map[pair]*state)

			for _, p := range tt.giveCalls {
				s := b.Get(p.account, p.region)
				require.NotNil(t, s)
				assert.Equal(t, p.account, s.account)
				assert.Equal(t, p.region, s.region)

				if prev, ok := seen[p]; ok {
					assert.Same(t, prev, s, "a key keeps its state")
				}

				seen[p] = s
			}

			assert.Equal(t, tt.wantInits, calls.Load())
			assert.Len(t, seen, int(tt.wantInits))
		})
	}
}

func Test_Bundle_Reset(t *testing.T) {
	all := []pair{
		{"111111111111", usEast1},
		{"111111111111", euWest1},
		{"222222222222", usEast1},
		{"222222222222", euWest1},
	}

	tests := []struct {
		name        string
		giveAccount string
		giveRegion  string
		wantGone    []pair
	}{
		{name: "everything", giveAccount: "", giveRegion: "", wantGone: all},
		{
			name:        "one account, every region",
			giveAccount: "111111111111",
			giveRegion:  "",
			wantGone:    []pair{all[0], all[1]},
		},
		{
			name:        "one account and region",
			giveAccount: "222222222222",
			giveRegion:  euWest1,
			wantGone:    []pair{all[3]},
		},
		{
			name:        "one region, every account",
			giveAccount: "",
			giveRegion:  usEast1,
			wantGone:    []pair{all[0], all[2]},
		},
		{name: "no match", giveAccount: "333333333333", giveRegion: "", wantGone: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, calls := seeded(all...)
			before := collect(b)
			require.Len(t, before, len(all))

			b.Reset(tt.giveAccount, tt.giveRegion)

			after := collect(b)
			assert.Len(t, after, len(all)-len(tt.wantGone))

			for _, p := range all {
				if slices.Contains(tt.wantGone, p) {
					assert.NotContains(t, after, p)

					continue
				}

				assert.Same(t, before[p], after[p], "a state outside the reset survives")
			}

			// A reset key is created again on the next Get.
			initsBefore := calls.Load()

			for _, p := range tt.wantGone {
				fresh := b.Get(p.account, p.region)
				assert.NotSame(t, before[p], fresh)
			}

			assert.Equal(t, initsBefore+int64(len(tt.wantGone)), calls.Load())
		})
	}
}

func Test_Bundle_Range(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		b, _ := seeded()
		assert.Empty(t, collect(b))
	})

	t.Run("visits every key once", func(t *testing.T) {
		pairs := []pair{{"1", "a"}, {"1", "b"}, {"2", "a"}, {"3", "c"}, {"4", "d"}}
		b, _ := seeded(pairs...)

		got := collect(b)
		assert.Len(t, got, len(pairs))

		for _, p := range pairs {
			assert.Same(t, b.Get(p.account, p.region), got[p])
		}
	})

	t.Run("stops when fn returns false", func(t *testing.T) {
		b, _ := seeded(pair{"1", "a"}, pair{"1", "b"}, pair{"2", "a"}, pair{"3", "c"})

		visits := 0

		b.Range(func(string, string, *state) bool {
			visits++

			return false
		})

		assert.Equal(t, 1, visits)
	})

	t.Run("fn may call the bundle", func(t *testing.T) {
		b, _ := seeded(pair{"1", "a"}, pair{"2", "b"})

		b.Range(func(account, region string, _ *state) bool {
			b.Get(account, region)
			b.Reset(account, region)

			return true
		})

		assert.Empty(t, collect(b))
	})
}

func Test_Bundle_Get_concurrent(t *testing.T) {
	const (
		goroutines = 64
		keys       = 8
	)

	var (
		initsMu sync.Mutex
		inits   = make(map[pair]int)
	)

	b := store.NewBundle(func(account, region string) *state {
		initsMu.Lock()
		inits[pair{account, region}]++
		initsMu.Unlock()

		return newState(account, region)
	})

	names := make([]pair, keys)
	for i := range names {
		names[i] = pair{account: "00000000000" + string(rune('0'+i)), region: usEast1}
	}

	results := make([][]*state, goroutines)

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)

	start.Add(1)

	for g := range goroutines {
		results[g] = make([]*state, keys)

		done.Go(func() {
			start.Wait()

			for i, p := range names {
				results[g][i] = b.Get(p.account, p.region)
			}
		})
	}

	start.Done()
	done.Wait()

	for _, p := range names {
		assert.Equal(t, 1, inits[p], "newFn ran once for %v", p)
	}

	for i := range names {
		for g := 1; g < goroutines; g++ {
			assert.Same(t, results[0][i], results[g][i], "every goroutine sees one state per key")
		}
	}
}
