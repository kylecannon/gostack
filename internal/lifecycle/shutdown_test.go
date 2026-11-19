package lifecycle_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_PlanShutdown(t *testing.T) {
	const (
		largest = 9223372036 * time.Second
		// The reserve of the largest timeout: a quarter of the time before Save.
		largestReserve = (largest - lifecycle.KillMargin) / 4
	)

	tests := []struct {
		name        string
		giveTimeout time.Duration
		// Offsets from the start of the shutdown. All zero means no limit.
		wantWork  time.Duration
		wantCalls time.Duration
		wantSave  time.Duration
		wantClose time.Duration
	}{
		{name: "no limit", giveTimeout: 0},
		{name: "a negative timeout is no limit", giveTimeout: -time.Second},
		{
			name: "one second leaves the half second margin", giveTimeout: time.Second,
			wantWork: 250 * time.Millisecond, wantCalls: 375 * time.Millisecond,
			wantSave: 500 * time.Millisecond, wantClose: 900 * time.Millisecond,
		},
		{
			name: "two seconds reserve the minimum", giveTimeout: 2 * time.Second,
			wantWork: 1000 * time.Millisecond, wantCalls: 1250 * time.Millisecond,
			wantSave: 1500 * time.Millisecond, wantClose: 1900 * time.Millisecond,
		},
		{
			// Work is main's: a shutdown.d hook of 3 s finishes (SQ-294, S3).
			name: "the default of five seconds", giveTimeout: 5 * time.Second,
			wantWork: 3375 * time.Millisecond, wantCalls: 3937500 * time.Microsecond,
			wantSave: 4500 * time.Millisecond, wantClose: 4900 * time.Millisecond,
		},
		{
			name: "ten seconds", giveTimeout: 10 * time.Second,
			wantWork: 7125 * time.Millisecond, wantCalls: 8312500 * time.Microsecond,
			wantSave: 9500 * time.Millisecond, wantClose: 9900 * time.Millisecond,
		},
		{
			name: "thirty seconds reserve a quarter for the save", giveTimeout: 30 * time.Second,
			wantWork: 22125 * time.Millisecond, wantCalls: 25812500 * time.Microsecond,
			wantSave: 29500 * time.Millisecond, wantClose: 29900 * time.Millisecond,
		},
		{
			name: "sixty seconds", giveTimeout: 60 * time.Second,
			wantWork: 44625 * time.Millisecond, wantCalls: 52062500 * time.Microsecond,
			wantSave: 59500 * time.Millisecond, wantClose: 59900 * time.Millisecond,
		},
		{
			name: "a timeout under the margin still plans a save and a close", giveTimeout: 400 * time.Millisecond,
			wantWork: 50 * time.Millisecond, wantCalls: 75 * time.Millisecond,
			wantSave: 100 * time.Millisecond, wantClose: 300 * time.Millisecond,
		},
		{
			name: "a timeout too short for the close margin closes with the save", giveTimeout: 80 * time.Millisecond,
			wantWork: 10 * time.Millisecond, wantCalls: 15 * time.Millisecond,
			wantSave: 20 * time.Millisecond, wantClose: 20 * time.Millisecond,
		},
		{
			// config refuses a larger SHUTDOWN_TIMEOUT, so this is the edge of what
			// PlanShutdown can be given.
			name: "the largest SHUTDOWN_TIMEOUT does not overflow", giveTimeout: largest,
			wantWork:  largest - lifecycle.KillMargin - largestReserve,
			wantCalls: largest - lifecycle.KillMargin - largestReserve/2,
			wantSave:  largest - lifecycle.KillMargin,
			wantClose: largest - lifecycle.CloseMargin,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				got := lifecycle.PlanShutdown(start, tt.giveTimeout)

				if tt.wantSave == 0 {
					assert.True(t, got.Work.IsZero(), "no limit for the hooks and the drain")
					assert.True(t, got.Calls.IsZero(), "no limit for the in-process calls")
					assert.True(t, got.Save.IsZero(), "no limit for the save")
					assert.True(t, got.Close.IsZero(), "no limit for Close")

					return
				}

				assert.Equal(t, start.Add(tt.wantWork), got.Work)
				assert.Equal(t, start.Add(tt.wantCalls), got.Calls)
				assert.Equal(t, start.Add(tt.wantSave), got.Save)
				assert.Equal(t, start.Add(tt.wantClose), got.Close)

				assert.True(t, got.Work.Before(got.Calls), "the save has a reserve, and the calls drain is in it")
				assert.Equal(t, got.Save.Sub(got.Work)/2, got.Save.Sub(got.Calls), "the second save keeps half the reserve")
				assert.False(t, got.Close.Before(got.Save), "Close falls after the save")
				assert.LessOrEqual(t, got.Close.Sub(start), tt.giveTimeout, "Close ends before the kill")
			})
		})
	}

	t.Run("the margins before the kill hold for every whole second", func(t *testing.T) {
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		for secs := 1; secs <= 120; secs++ {
			timeout := time.Duration(secs) * time.Second
			got := lifecycle.PlanShutdown(start, timeout)

			assert.GreaterOrEqual(t, start.Add(timeout).Sub(got.Save), lifecycle.KillMargin, "%d s", secs)
			assert.GreaterOrEqual(t, start.Add(timeout).Sub(got.Close), lifecycle.CloseMargin, "%d s", secs)
			assert.True(t, got.Work.Before(got.Save), "%d s", secs)
			assert.True(t, got.Close.After(got.Save), "%d s", secs)
		}
	})
}

func Test_Until(t *testing.T) {
	t.Run("a zero deadline is no limit", func(t *testing.T) {
		ctx, cancel := lifecycle.Until(t.Context(), time.Time{})
		defer cancel()

		_, ok := ctx.Deadline()
		assert.False(t, ok)
	})

	t.Run("a deadline ends the context at that time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			deadline := time.Now().Add(2 * time.Second)

			ctx, cancel := lifecycle.Until(t.Context(), deadline)
			defer cancel()

			time.Sleep(time.Second)
			require.NoError(t, ctx.Err())

			time.Sleep(time.Second)
			synctest.Wait()
			require.Error(t, ctx.Err())
		})
	})
}
