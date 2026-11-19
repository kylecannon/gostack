package persist

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timedNamer is a namer whose every Snapshot takes d.
type timedNamer struct {
	*namer

	d time.Duration
}

func (s timedNamer) Snapshot(w io.Writer) error {
	time.Sleep(s.d)

	return s.namer.Snapshot(w)
}

// Test_Engine_SlowSnapshotWithPendingAssetsSavesSQSFirst: s3 has an asset
// pending when the flush starts (a PUT since the last save) and a Snapshot that
// takes 400 ms; sqs has nothing to sync. The save has 300 ms. A service with
// assets pending at flush start is snapshotted after the ones with none, so
// sqs is written before s3's Snapshot takes the deadline (SQ-195 M1).
func Test_Engine_SlowSnapshotWithPendingAssetsSavesSQSFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))

		s3 := timedNamer{namer: &namer{}, d: 400 * time.Millisecond}
		e.Register("s3", s3)

		a, err := e.Assets("s3")
		require.NoError(t, err)

		sqs := &namer{}
		sqs.add("sqs-state")
		e.Register("sqs", sqs)

		s3.add(commitAsset(t, a, "put since the last save"))
		e.MarkDirty("s3")
		e.MarkDirty("sqs")

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		_ = e.Flush(ctx)

		onDisk, _ := os.ReadFile(filepath.Join(dir, "sqs", FormatJSON.fileName()))
		assert.Contains(t, string(onDisk), "sqs-state", "sqs needs no sync and was not saved")

		synctest.Wait()
	})
}

// Test_Engine_SlowSnapshotGoesLastOnceMeasured: s3 has nothing pending and a
// Snapshot that takes 400 ms; sqs and sts are dirty; each flush has 300 ms.
// The first flush in a process has no Snapshot measured, so it saves in name
// order: s3's Snapshot takes the deadline, s3 is saved and sqs and sts are not.
// The second flush knows s3's Snapshot is the slowest and takes it last, so
// sqs and sts are saved.
func Test_Engine_SlowSnapshotGoesLastOnceMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))

		s3 := timedNamer{namer: &namer{}, d: 400 * time.Millisecond}
		s3.add("buckets")
		e.Register("s3", s3)

		_, err := e.Assets("s3")
		require.NoError(t, err)

		e.Register("sqs", &stateSnap{state: "sqs queues"})
		e.Register("sts", &stateSnap{state: "keys"})

		flush := func() {
			for _, name := range []string{"s3", "sqs", "sts"} {
				e.MarkDirty(name)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()

			_ = e.Flush(ctx)
		}

		flush()
		assert.Equal(t, "buckets", readState(t, filepath.Join(dir, "s3", "store.json")),
			"a finished snapshot with nothing to sync renames past its deadline")
		assert.NoFileExists(t, filepath.Join(dir, "sqs", "store.json"),
			"the first flush has no Snapshot measured and takes s3 first, in name order")
		assert.NoFileExists(t, filepath.Join(dir, "sts", "store.json"))

		flush()
		assert.Equal(t, "sqs queues", readState(t, filepath.Join(dir, "sqs", "store.json")),
			"s3's measured Snapshot is the slowest, so sqs goes before it")
		assert.Equal(t, "keys", readState(t, filepath.Join(dir, "sts", "store.json")))

		synctest.Wait()
	})
}

// Test_Engine_PreemptDuringPassTwoRenamesNothingAndLeavesNoTemp: a timed save
// defers s3 (an asset pending) and is in its pass-two sync, gated, when a Load
// preempts it. The gate opens after the preempt. The s3 state file must still
// name only what was saved before, and no temp file may be left (SQ-195).
func Test_Engine_PreemptDuringPassTwoRenamesNothingAndLeavesNoTemp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		s3 := &namer{}
		e.Register("s3", s3)

		a, err := e.Assets("s3")
		require.NoError(t, err)

		s3.add(commitAsset(t, a, "saved"))
		e.MarkDirty("s3")
		require.NoError(t, e.Flush(t.Context()))

		before, err := os.ReadFile(filepath.Join(dir, "s3", FormatJSON.fileName()))
		require.NoError(t, err)

		entered := make(chan struct{}, 1)
		gate := make(chan struct{})
		realSync := a.fsync
		a.fsync = func(f *os.File) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate

			return realSync(f)
		}

		s3.add(commitAsset(t, a, "unsaved"))
		e.MarkDirty("s3")
		<-entered // the timed save is in its pass-two sync

		done := make(chan error, 1)
		go func() { done <- e.Load(t.Context()) }()

		synctest.Wait() // the Load has preempted and waits for the guard
		close(gate)
		require.NoError(t, <-done)

		after, err := os.ReadFile(filepath.Join(dir, "s3", FormatJSON.fileName()))
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "a preempted pass-two save renamed")

		entries, err := os.ReadDir(filepath.Join(dir, "s3"))
		require.NoError(t, err)

		for _, entry := range entries {
			assert.NotContains(t, entry.Name(), tempSuffix, "temp file left")
		}

		synctest.Wait()
	})
}

// lockedBuffer is a log sink the engine's goroutines and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // a test double
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// Test_Engine_LentDeadlineLogsTheAbandonedSave: a lent deadline reaches the
// timed save as context.Canceled. When it runs out, the services the save did
// not reach are logged as a save's own deadline logs them, and the flush is
// still no failure. A Load that preempts the save after the lent deadline is no
// deadline, and logs nothing.
func Test_Engine_LentDeadlineLogsTheAbandonedSave(t *testing.T) {
	const warning = "save abandoned at its deadline"

	run := func(t *testing.T, preempt bool) string {
		t.Helper()

		var logs lockedBuffer

		synctest.Test(t, func(t *testing.T) {
			cfg := quietConfig(t.TempDir(), 10*time.Millisecond)
			cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			e := New(cfg)

			s3 := &stateSnap{state: "s3 buckets", entered: make(chan struct{}, 1), gate: make(chan struct{})}
			e.Register("s3", s3)
			e.Register("sqs", &stateSnap{state: "sqs state"})

			s3.mu.Lock()
			gate := s3.gate
			s3.mu.Unlock()

			e.MarkDirty("s3")
			e.MarkDirty("sqs")
			<-s3.entered // the timed save is inside s3's Snapshot

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			require.ErrorIs(t, e.Flush(ctx), context.DeadlineExceeded, "the lent deadline runs out")

			if preempt {
				done := make(chan error, 1)

				go func() { done <- e.Load(t.Context()) }()

				synctest.Wait() // the load has preempted the timed save and waits for the guard
				close(gate)
				require.NoError(t, <-done)
			} else {
				close(gate)
			}

			synctest.Wait()
			require.NoError(t, e.Close(t.Context()))
		})

		return logs.String()
	}

	t.Run("lent deadline", func(t *testing.T) {
		logs := run(t, false)
		assert.Contains(t, logs, warning)
		assert.Contains(t, logs, "services=[sqs]", "sqs is the service the save did not reach")
		assert.NotContains(t, logs, "flush failed", "a lent deadline is still no failure")
	})

	t.Run("preempt after a lent deadline", func(t *testing.T) {
		logs := run(t, true)
		assert.NotContains(t, logs, warning)
		assert.NotContains(t, logs, "flush failed")
	})
}
