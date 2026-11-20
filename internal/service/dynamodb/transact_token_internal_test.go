package dynamodb

import (
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test_tokens_expire: 100,000 tokens recorded at t0 are all dropped by the
// first transaction once they have expired, with or without a token of its
// own, and the heap returns to about what it was: the review found 1,000,000
// tokens holding 442 MiB that nothing freed until 48,576 more arrived
// (verification/SQ-293).
func Test_tokens_expire(t *testing.T) {
	const n = 100_000

	t0 := time.Unix(1_700_000_000, 0)
	later := t0.Add(tokenWindow)

	heap := func() int64 {
		var m runtime.MemStats

		runtime.GC() //nolint:revive // the test measures what a collection leaves
		runtime.ReadMemStats(&m)

		return int64(m.HeapAlloc)
	}

	record := func(ts *tokens, token string, at time.Time) {
		state, _ := ts.begin(token, [32]byte{1}, at)
		require.Equal(t, tokenNew, state)
		ts.finish(token, at, tableConsumption{"table": &consumption{table: 2}})
	}

	for name, next := range map[string]func(ts *tokens){
		"a token":    func(ts *tokens) { record(ts, "one-more", later) },
		"no token":   func(ts *tokens) { ts.expire(later) },
		"in window":  func(ts *tokens) { ts.expire(later.Add(-time.Nanosecond)) },
		"half later": nil,
	} {
		t.Run(name, func(t *testing.T) {
			ts := &tokens{}
			base := heap()

			for i := range n {
				at := t0
				if next == nil && i >= n/2 {
					at = t0.Add(time.Minute)
				}

				record(ts, "0123456789abcdef0123456789ab-"+strconv.Itoa(i), at)
			}

			full := heap() - base
			require.Len(t, ts.byID, n)

			switch name {
			case "in window":
				next(ts)
				require.Len(t, ts.byID, n, "a token inside its window stays")

				return
			case "half later":
				ts.expire(later)
				require.Len(t, ts.byID, n/2, "only the tokens that have expired go")
				require.Len(t, ts.done[ts.head:], n/2)

				return
			}

			next(ts)

			live := len(ts.byID)
			require.LessOrEqual(t, live, 1, "every expired token was dropped")
			require.Len(t, ts.done[ts.head:], live)
			require.Less(t, heap()-base, full/10, "the heap held %d bytes for %d tokens", full, n)
			runtime.KeepAlive(ts)
		})
	}
}
