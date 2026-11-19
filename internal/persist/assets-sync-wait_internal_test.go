package persist

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncFixture is an engine with one service whose assets are synced, and the
// state file that service is saved to.
type syncFixture struct {
	e         *Engine
	n         *namer
	a         *Assets
	stateFile string
	logs      *warnings
}

func newSyncFixture(t *testing.T, freq time.Duration) *syncFixture {
	t.Helper()

	dir := t.TempDir()
	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: freq, Logger: slog.New(logs)})
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	return &syncFixture{e: e, n: n, a: a, stateFile: filepath.Join(dir, syncService, FormatJSON.fileName()), logs: logs}
}

// commit commits an asset and names it in the service's state.
func (f *syncFixture) commit(t *testing.T, body string) string {
	t.Helper()

	id := commitAsset(t, f.a, body)
	f.n.add(id)
	f.e.MarkDirty(syncService)

	return id
}

// onDisk returns the ids the state file names.
func (f *syncFixture) onDisk(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(f.stateFile)
	if os.IsNotExist(err) {
		return nil
	}

	require.NoError(t, err)

	return strings.Split(string(data), "\n")
}

// gateFirstSync makes the first sync call of a wait for release and then
// return result, and every later call run the real sync. started receives a
// value as each call starts. release may be called more than once, so a test
// defers it too and a failed assertion leaves no goroutine blocked.
func gateFirstSync(a *Assets, result error) (started <-chan struct{}, release func()) {
	starts, gate := make(chan struct{}, 64), make(chan struct{})

	var (
		calls atomic.Int32
		once  sync.Once
	)

	a.fsync = func(f *os.File) error {
		starts <- struct{}{}

		if calls.Add(1) == 1 {
			<-gate

			return result
		}

		return defaultSync(f)
	}

	return starts, func() { once.Do(func() { close(gate) }) }
}

// Test_Assets_AbandonedSyncThatFailsIsFinal checks that a sync a save stopped
// waiting for still counts: when it fails after its save gave up, the next save
// fails, and the service turns off (SQ-164 F1, the abandoned interleaving).
func Test_Assets_AbandonedSyncThatFailsIsFinal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		saved := f.commit(t, "saved")
		require.NoError(t, f.e.Flush(t.Context()))

		late := f.commit(t, "late")
		_, release := gateFirstSync(f.a, fs.ErrPermission)
		defer release()

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		require.ErrorIs(t, f.e.Flush(ctx), context.DeadlineExceeded)
		assert.True(t, f.e.Enabled(syncService), "a sync cut short loses nothing")

		release()
		synctest.Wait()

		require.ErrorIs(t, f.e.Flush(t.Context()), ErrUnsafe)
		assert.Equal(t, []string{saved}, f.onDisk(t), "no snapshot names %s", late)
		assert.False(t, f.e.Enabled(syncService))
		assert.Len(t, f.logs.errors(), 1)
	})
}

// Test_Assets_NextSaveWaitsForAnAbandonedSync checks that a save does not
// rename its snapshot while a sync an earlier save gave up on is still out,
// even when its own sync is done: that sync may yet report a lost write. Its
// deadline fails it without turning the service off, and once the earlier sync
// has returned clean, a save goes through.
func Test_Assets_NextSaveWaitsForAnAbandonedSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		saved := f.commit(t, "saved")
		require.NoError(t, f.e.Flush(t.Context()))

		late := f.commit(t, "late")
		_, release := gateFirstSync(f.a, nil)
		defer release()

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		require.ErrorIs(t, f.e.Flush(ctx), context.DeadlineExceeded)

		newer := f.commit(t, "newer")

		ctx, cancel = context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		require.ErrorIs(t, f.e.Flush(ctx), context.DeadlineExceeded,
			"the earlier sync has not returned")
		assert.Equal(t, []string{saved}, f.onDisk(t))
		assert.True(t, f.e.Enabled(syncService), "an earlier sync still out is no failure")

		release()
		synctest.Wait()

		require.NoError(t, f.e.Flush(t.Context()))
		assert.ElementsMatch(t, []string{saved, late, newer}, f.onDisk(t))
		assert.Empty(t, f.logs.errors())
	})
}

// Test_Engine_ShutdownPreemptsATimedSave checks that a shutdown save is not
// starved by a timed save whose asset sync is slow (SQ-164 F2). Every sync call
// here takes slowSync in all, a syncfs writing back what other tenants of the
// filesystem dirtied, and the shutdown has 300 ms. Waiting for the timed save
// to finish and then syncing again takes two slow syncs; cancelling it and
// syncing alongside the sync it left running takes one.
func Test_Engine_ShutdownPreemptsATimedSave(t *testing.T) {
	const slowSync = 200 * time.Millisecond

	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, 10*time.Millisecond)
		entered := make(chan struct{}, 64)
		f.a.fsync = func(file *os.File) error {
			entered <- struct{}{}

			time.Sleep(slowSync / syncRounds)

			return defaultSync(file)
		}

		first := f.commit(t, "first")
		<-entered // the timed save is syncing

		acked := f.commit(t, "acknowledged during the timed save")

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		require.NoError(t, f.e.Close(ctx))
		assert.ElementsMatch(t, []string{first, acked}, f.onDisk(t))
		assert.Empty(t, f.logs.errors(), "giving way is no failure")
	})
}

// Test_Engine_LoadAndResetPreemptATimedSave checks that /_gostack/state/load
// and /reset do not wait for a timed save stuck in its asset sync: they cancel
// it and run within their own deadline.
func Test_Engine_LoadAndResetPreemptATimedSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, 10*time.Millisecond)
		started, release := gateFirstSync(f.a, nil)
		defer release()

		f.commit(t, "first")
		<-started // the timed save is syncing, and the sync is stuck

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		require.NoError(t, f.e.Load(ctx))

		<-started // the next timed save is waiting for the stuck sync

		ctx, cancel = context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		require.NoError(t, f.e.ResetAssets(ctx, syncService, func() {}))

		release()
		require.NoError(t, f.e.Close(t.Context()))
	})
}
