package main

import (
	"bytes"
	"context"
	"iter"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
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

// Test_saver_save_keepsWhatTheDrainDeliveredWhenACloseOverruns: the first save
// runs before the in-process calls drain, a call the drain delivers changes
// the state, and then a service's Close holds until its deadline, so the last
// save is skipped. The second save, the engine's Flush, is the only one that
// writes the change, and a new engine on the same directory finds it.
func Test_saver_save_keepsWhatTheDrainDeliveredWhenACloseOverruns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng, dir := persistentEngine(t)
		state := &widgets{data: "acknowledged before the shutdown"}

		eng.Register(testService, state)
		eng.MarkDirty(testService)

		reg := state.registration()
		// It honours its context, and returns at the deadline.
		reg.Close = func(ctx context.Context) { <-ctx.Done() }

		s := &saver{eng: eng, services: &providers{regs: []service.Registration{reg}}}

		s.saveFirst(t.Context())

		state.data = "delivered by the drain"
		eng.MarkDirty(testService)

		closeCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		s.save(t.Context(), closeCtx)

		require.ErrorIs(t, closeCtx.Err(), context.DeadlineExceeded, "the Close held past its deadline")

		restarted := persist.New(persist.Config{PersistVarSet: true, BaseDir: dir})
		loaded := &widgets{}

		restarted.Register(testService, loaded)
		require.NoError(t, restarted.Load(t.Context()))
		assert.Equal(t, "delivered by the drain", loaded.data)
	})
}

// Test_saver_closeServices_warnsOfASkippedLastSave checks the log of a last
// save that does not follow: one warning for each cause, and the right one. A
// Close that honours its context and returns at the deadline is not still
// running, but the last save is skipped, and the log says that rather than
// nothing; a Close that ignores it is named as still running, which already
// says why; and a shutdown with time left warns of neither.
func Test_saver_closeServices_warnsOfASkippedLastSave(t *testing.T) {
	const (
		budget  = 400 * time.Millisecond
		skipped = "the last save was skipped because the close deadline passed"
		running = "a service did not stop in time"
	)

	honours := func(ctx context.Context) { <-ctx.Done() }

	tests := []struct {
		name string
		// ignores makes the service's Close wait for release, ignoring ctx.
		ignores bool
		closeFn func(context.Context)
		saves   bool
		// skipLogs and runningLogs count the warnings of each kind.
		skipLogs, runningLogs int
	}{
		{name: "every Close returns before the deadline", closeFn: func(context.Context) {}, saves: true},
		{name: "a Close that honours ctx returns at the deadline", closeFn: honours, skipLogs: 1},
		{name: "a Close that ignores ctx is still running", ignores: true, runningLogs: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logs bytes.Buffer

				prev := slog.Default()

				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(prev) })

				release := make(chan struct{})

				reg := registration("svc")
				reg.Close = tt.closeFn

				if tt.ignores {
					reg.Close = func(context.Context) { <-release }
				}

				eng, _ := persistentEngine(t)
				s := &saver{eng: eng, services: &providers{regs: []service.Registration{reg}}}

				ctx, cancel := context.WithTimeout(t.Context(), budget)
				defer cancel()

				assert.Equal(t, tt.saves, s.closeServices(ctx))
				assert.Equal(t, tt.skipLogs, strings.Count(logs.String(), skipped), logs.String())
				assert.Equal(t, tt.runningLogs, strings.Count(logs.String(), running), logs.String())

				close(release)
			})
		})
	}
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
