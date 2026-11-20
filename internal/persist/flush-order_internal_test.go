package persist

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateSnap is a Snapshotter whose state is one string. A Snapshot can be held
// at a gate, and can run a hook first; a Restore can be held until released.
type stateSnap struct {
	mu       sync.Mutex
	state    string
	restored string
	// gate, when set, holds the next Snapshot until it is closed; entered
	// receives a value as that Snapshot starts.
	gate    chan struct{}
	entered chan struct{}
	// during runs inside the next Snapshot and is cleared; its error is the
	// Snapshot's.
	during func() error
	// hold, when set, holds every Restore until it is closed; restoring
	// receives a value as each one starts.
	hold      chan struct{}
	restoring chan struct{}
}

func (s *stateSnap) Snapshot(w io.Writer) error {
	s.mu.Lock()
	state, gate, during := s.state, s.gate, s.during
	s.gate, s.during = nil, nil
	s.mu.Unlock()

	if gate != nil {
		s.entered <- struct{}{}
		<-gate
	}

	if during != nil {
		err := during()
		if err != nil {
			return err
		}
	}

	_, err := io.WriteString(w, state)

	return err //nolint:wrapcheck // a test double
}

func (s *stateSnap) Restore(r io.Reader) error {
	s.mu.Lock()
	hold, restoring := s.hold, s.restoring
	s.mu.Unlock()

	if hold != nil {
		restoring <- struct{}{}
		<-hold
	}

	b, err := io.ReadAll(r)
	if err != nil {
		return err //nolint:wrapcheck // a test double
	}

	s.mu.Lock()
	s.state, s.restored = string(b), string(b)
	s.mu.Unlock()

	return nil
}

func (s *stateSnap) set(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

func quietConfig(dir string, freq time.Duration) Config {
	return Config{PersistVarSet: true, BaseDir: dir, Frequency: freq, Logger: slog.New(slog.DiscardHandler)}
}

// Test_Engine_Close_SavesServicesWithNoPendingSyncFirst checks that one
// service's slow asset sync does not cost the others their save: s3 sorts
// first and its sync never returns, yet the shutdown save writes sqs before
// its deadline ends (SQ-190 item 1).
func Test_Engine_Close_SavesServicesWithNoPendingSyncFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))

		n := &namer{}
		e.Register("s3", n)
		e.Register("sqs", &stateSnap{state: "queues"})

		a, err := e.Assets("s3")
		require.NoError(t, err)

		_, release := gateFirstSync(a, nil)
		defer release()

		n.add(commitAsset(t, a, "body"))
		e.MarkDirty("s3")
		e.MarkDirty("sqs")

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		err = e.Close(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "s3's save runs out of time")
		assert.Equal(t, "queues", readState(t, filepath.Join(dir, "sqs", "store.json")),
			"sqs is saved although s3 sorts first and its sync never returns")
		assert.NoFileExists(t, filepath.Join(dir, "s3", "store.json"))
	})
}

func readState(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(b)
}

// Test_Engine_TimerDoesNotSpinWhileACallerWaits checks that with
// PERSIST_FREQUENCY=0 a timer callback that gives way to a waiting Load does
// not re-arm at once: the timer is armed again only when the waiter is done
// (SQ-190 item 3).
func Test_Engine_TimerDoesNotSpinWhileACallerWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), Immediate))
		s := &stateSnap{state: "a"}
		e.Register("svc", s)
		e.MarkDirty("svc")
		require.NoError(t, e.Flush(t.Context()))

		hold := make(chan struct{})
		s.mu.Lock()
		s.hold, s.restoring = hold, make(chan struct{}, 2)
		s.mu.Unlock()

		done := make(chan error, 2)

		go func() { done <- e.Load(t.Context()) }()

		<-s.restoring // the first load holds the guard

		go func() { done <- e.Load(t.Context()) }()

		synctest.Wait() // the second load waits for the guard

		e.mu.Lock()
		before := e.gen
		e.mu.Unlock()

		s.set("b")
		e.MarkDirty("svc")

		// A zero-delay timer that re-arms itself never lets the bubble's clock
		// move, so count the arms while yielding instead of sleeping.
		var after uint64

		for range 2000 {
			runtime.Gosched()

			e.mu.Lock()
			after = e.gen
			e.mu.Unlock()

			if after-before > 5 {
				break
			}
		}

		s.mu.Lock()
		s.hold = nil
		s.mu.Unlock()
		close(hold)
		require.NoError(t, <-done)
		require.NoError(t, <-done)

		assert.LessOrEqual(t, after-before, uint64(1), "the timer re-armed while a load waited")

		synctest.Wait()
		time.Sleep(time.Second)

		e.mu.Lock()
		dirty := e.services["svc"].dirty
		e.mu.Unlock()

		assert.False(t, dirty, "the timer is armed once the waiters are done, and saves the change")
	})
}

// Test_Engine_PreemptedTimedSaveDoesNotRename checks that a timed save a Load
// cancels while it is inside Snapshot of a service with no assets does not
// rename its file, so the Load restores the state saved before it was asked
// for (SQ-190 item 4).
func Test_Engine_PreemptedTimedSaveDoesNotRename(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), 10*time.Millisecond))
		s := &stateSnap{state: "saved before", entered: make(chan struct{}, 1)}
		e.Register("svc", s)
		e.MarkDirty("svc")
		require.NoError(t, e.Flush(t.Context()))

		gate := make(chan struct{})
		s.mu.Lock()
		s.state, s.gate = "unsaved change", gate
		s.mu.Unlock()

		e.MarkDirty("svc")
		<-s.entered // the timed save is inside Snapshot

		done := make(chan error, 1)

		go func() { done <- e.Load(t.Context()) }()

		synctest.Wait() // the load has cancelled the timed save and waits for the guard
		close(gate)
		require.NoError(t, <-done)

		s.mu.Lock()
		restored := s.restored
		s.mu.Unlock()

		assert.Equal(t, "saved before", restored, "the load restored what the cancelled timed save wrote")
	})
}

// Test_Engine_FlushIsClassifiedByItsError checks that a flush whose context was
// cancelled is judged by the error it failed with, not by the context (SQ-190
// item 5).
func Test_Engine_FlushIsClassifiedByItsError(t *testing.T) {
	errDiskFull := errors.New("no space left on device")

	t.Run("a real failure under a cancelled context counts, with a backoff", func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		s := &stateSnap{state: "a", during: func() error { cancel(); return errDiskFull }}
		e.Register("svc", s)
		e.MarkDirty("svc")

		require.ErrorIs(t, e.Flush(ctx), errDiskFull)

		e.mu.Lock()
		defer e.mu.Unlock()

		svc := e.services["svc"]

		assert.Equal(t, 1, svc.failures)
		assert.False(t, svc.retryAt.IsZero(), "the next flush waits for the backoff")
		assert.True(t, svc.dirty)
	})

	t.Run("a flush that saved everything as its context ended ends the run of failures", func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		c := &cancelsOnReferences{cancel: cancel}
		e.Register("svc", c)

		a, err := e.Assets("svc")
		require.NoError(t, err)

		c.add(commitAsset(t, a, "body"))
		svc := e.services["svc"]
		svc.failures, svc.lastFailure, svc.retryAt = 3, "disk full", time.Now().Add(time.Hour)
		e.MarkDirty("svc")

		require.NoError(t, e.Flush(ctx))
		require.Error(t, ctx.Err(), "the context ended after the rename")

		e.mu.Lock()
		defer e.mu.Unlock()

		assert.Zero(t, svc.failures)
		assert.True(t, svc.retryAt.IsZero(), "no stale backoff is left")
	})

	t.Run("a cancelled save does not log removals as stuck", func(t *testing.T) {
		var logs bytes.Buffer

		cfg := quietConfig(t.TempDir(), time.Hour)
		cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		e := New(cfg)
		n := &namer{}
		e.Register("svc", n)

		a, err := e.Assets("svc")
		require.NoError(t, err)

		old := commitAsset(t, a, "old")
		n.add(old)
		e.MarkDirty("svc")
		require.NoError(t, e.Flush(t.Context()))

		n.mu.Lock()
		n.ids = nil
		n.mu.Unlock()
		require.NoError(t, a.Remove(old)) // queued: the file on disk names it

		ctx, cancel := context.WithCancel(t.Context())
		n.during = func() string { cancel(); return commitAsset(t, a, "new") }
		e.MarkDirty("svc")

		require.ErrorIs(t, e.Flush(ctx), context.Canceled)
		assert.NotContains(t, logs.String(), "asset removals are waiting")

		a.mu.Lock()
		defer a.mu.Unlock()

		assert.Contains(t, a.removed, old, "the removal waits for the next save")
	})
}

// cancelsOnReferences is a namer that cancels a context when the engine reads
// its references, which a save does after its rename.
type cancelsOnReferences struct {
	namer

	cancel context.CancelFunc
}

func (c *cancelsOnReferences) References() iter.Seq[string] {
	c.cancel()

	return c.namer.References()
}

// Test_Engine_ZeroTimedSaveIsNotPreempt checks that the zero value of timedSave
// is neither mode, so a caller that forgets to pick one never cancels a timed
// save by accident (SQ-190 item 10).
func Test_Engine_ZeroTimedSaveIsNotPreempt(t *testing.T) {
	var zero timedSave

	assert.NotEqual(t, preempt, zero)
	assert.NotEqual(t, lend, zero)
}
