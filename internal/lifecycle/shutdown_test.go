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
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		giveTimeout time.Duration
		wantWork    time.Duration // offset from start; zero with wantSave zero means no limit
		wantSave    time.Duration
	}{
		{name: "no limit", giveTimeout: 0},
		{name: "a negative timeout is no limit", giveTimeout: -time.Second},
		{
			name: "the default of five seconds", giveTimeout: 5 * time.Second,
			wantWork: 3375 * time.Millisecond, wantSave: 4500 * time.Millisecond,
		},
		{
			name: "one second leaves the half second margin", giveTimeout: time.Second,
			wantWork: 250 * time.Millisecond, wantSave: 500 * time.Millisecond,
		},
		{
			name: "a long timeout reserves a quarter for the save", giveTimeout: 30 * time.Second,
			wantWork: 22125 * time.Millisecond, wantSave: 29500 * time.Millisecond,
		},
		{
			name: "two seconds reserve the minimum", giveTimeout: 2 * time.Second,
			wantWork: 1000 * time.Millisecond, wantSave: 1500 * time.Millisecond,
		},
		{
			name: "a timeout under the margin still plans a save", giveTimeout: 400 * time.Millisecond,
			wantWork: 50 * time.Millisecond, wantSave: 100 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lifecycle.PlanShutdown(start, tt.giveTimeout)

			if tt.wantSave == 0 {
				assert.True(t, got.Work.IsZero())
				assert.True(t, got.Save.IsZero())

				return
			}

			assert.Equal(t, start.Add(tt.wantWork), got.Work)
			assert.Equal(t, start.Add(tt.wantSave), got.Save)
			assert.LessOrEqual(t, got.Save.Sub(start), tt.giveTimeout, "the save ends before the kill")
		})
	}

	t.Run("the margin before the kill is at least KillMargin for every whole second", func(t *testing.T) {
		for secs := 1; secs <= 120; secs++ {
			timeout := time.Duration(secs) * time.Second
			got := lifecycle.PlanShutdown(start, timeout)

			assert.GreaterOrEqual(t, start.Add(timeout).Sub(got.Save), lifecycle.KillMargin, "%d s", secs)
			assert.Less(t, got.Work, got.Save, "%d s", secs)
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
