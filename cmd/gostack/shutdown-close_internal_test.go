package main

import (
	"context"
	"iter"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assetful is a persisted service whose state names no assets, which is all
// persist.Engine.Assets asks of a Snapshotter besides Snapshot and Restore.
type assetful struct{ stamped }

func (*assetful) References() iter.Seq[string] { return func(func(string) bool) {} }
func (*assetful) Restored() iter.Seq[string]   { return func(func(string) bool) {} }

// Test_saver_save_order pins the order of a shutdown save through what the
// assets do. The save is Engine.Stop, which leaves the assets open; then every
// Close, which still uses them; then Engine.Close, which releases them. The
// assets fail every call once released, so a service's Close that creates an
// asset fails when Stop is replaced by Close, and a create after the save
// succeeds when the final Close is replaced by Flush.
func Test_saver_save_order(t *testing.T) {
	const name = "assetful"

	eng, _ := persistentEngine(t)
	state := &assetful{}

	eng.Register(name, state)

	assets, err := eng.Assets(name)
	require.NoError(t, err)
	require.NotNil(t, assets)

	var createInClose error

	reg := registration(name)
	reg.Persist = state
	reg.Close = func(context.Context) {
		writer, createErr := assets.Create()

		createInClose = createErr

		if createErr == nil {
			writer.Abort()
		}
	}

	s := &saver{eng: eng, services: &providers{regs: []service.Registration{reg}}}

	s.save(t.Context(), t.Context())

	require.NoError(t, createInClose, "the assets were released before the service's Close ran")

	writer, err := assets.Create()
	if err == nil {
		writer.Abort()
	}

	require.Error(t, err, "the assets were never released by the last save")
}

// Test_providers_close_reportsWhatIsStillRunning checks that a Close which
// returned is never reported as still running, whether it finished before ctx
// or returned because ctx ended, and that one which ignores ctx is, by name.
func Test_providers_close_reportsWhatIsStillRunning(t *testing.T) {
	const budget = 400 * time.Millisecond

	honours := func(ctx context.Context) { <-ctx.Done() }
	quick := func(context.Context) {}

	tests := []struct {
		name string
		// ignores is the name of a Close that waits for release, ignoring ctx.
		ignores string
		closes  map[string]func(context.Context)
		running []string
		// ends is when close returns, after the start.
		ends time.Duration
		// saves is whether a last save can follow.
		saves bool
	}{
		{
			name:   "every Close returns before the deadline",
			closes: map[string]func(context.Context){"a": quick, "b": quick},
			ends:   0,
			saves:  true,
		},
		{
			name:   "a Close that honours ctx returns at the deadline and is not reported",
			closes: map[string]func(context.Context){"a": quick, "b": honours},
			ends:   budget,
		},
		{
			name:    "a Close that ignores ctx is reported by name, once the grace has passed",
			ignores: "stuck",
			closes:  map[string]func(context.Context){"a": quick, "b": honours},
			running: []string{"stuck"},
			ends:    budget + closeGrace,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})

				regs := make([]service.Registration, 0, len(tt.closes)+1)
				for name, closeFn := range tt.closes {
					reg := registration(name)
					reg.Close = closeFn
					regs = append(regs, reg)
				}

				if tt.ignores != "" {
					reg := registration(tt.ignores)
					reg.Close = func(context.Context) { <-release }
					regs = append(regs, reg)
				}

				regs = append(regs, registration("no Close"))

				eng, _ := persistentEngine(t)
				s := &saver{eng: eng, services: &providers{regs: regs}}

				begin := time.Now()

				ctx, cancel := context.WithTimeout(t.Context(), budget)
				defer cancel()

				assert.Equal(t, tt.running, s.services.close(ctx))
				assert.Equal(t, begin.Add(tt.ends), time.Now())

				// Each case runs the same Close functions again through the saver, with
				// the same budget, for the answer it gives the save.
				ctx2, cancel2 := context.WithTimeout(t.Context(), budget)
				defer cancel2()

				assert.Equal(t, tt.saves, s.closeServices(ctx2))

				close(release)
			})
		})
	}
}
