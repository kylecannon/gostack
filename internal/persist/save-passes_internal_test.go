package persist

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// longSnapshot is a namer whose every Snapshot takes 500 ms, and which says
// when one starts.
type longSnapshot struct {
	*namer

	started chan struct{}
}

func (s longSnapshot) Snapshot(w io.Writer) error {
	select {
	case s.started <- struct{}{}:
	default:
	}

	time.Sleep(500 * time.Millisecond)

	return s.namer.Snapshot(w)
}

// Test_Engine_LentDeadlineKeepsAFinishedSnapshot models the shutdown saver:
// Stop(saveCtx), then Close(closeCtx) with closeCtx 400 ms after saveCtx, and
// the kill 500 ms after saveCtx. A timed save of a service with nothing to sync
// is 10 ms into a 500 ms Snapshot when Stop lends it 300 ms. The lent deadline
// runs out first, but the snapshot is taken and needs no sync, so it renames,
// as a save past its own deadline does: taking it again would end after the
// kill (SQ-193 M1).
func Test_Engine_LentDeadlineKeepsAFinishedSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		n := longSnapshot{namer: &namer{}, started: make(chan struct{})}
		e.Register(syncService, n)

		n.add("acknowledged")
		e.MarkDirty(syncService)
		<-n.started
		time.Sleep(10 * time.Millisecond)

		saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
		defer cancelSave()

		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(t.Context()), 700*time.Millisecond)
		defer cancelClose()

		done := make(chan struct{})

		go func() {
			defer close(done)

			_ = e.Stop(saveCtx)
			_ = e.Close(closeCtx)
		}()

		time.Sleep(800 * time.Millisecond) // the kill: saveCtx + KillMargin

		onDisk, _ := os.ReadFile(filepath.Join(dir, syncService, FormatJSON.fileName()))
		assert.Contains(t, string(onDisk), "acknowledged", "at the kill, the shutdown has lost the acknowledged write")

		<-done
	})
}

// Test_Engine_PreemptAfterALentDeadlineDoesNotRename: a Flush lends a timed
// save its deadline, which runs out while the save is in Snapshot, and then a
// Load preempts it. A context's cause is set once, so the lent deadline's cause
// must not hide the preempt: the save renames nothing, and the Load restores
// the state saved before it was asked for.
func Test_Engine_PreemptAfterALentDeadlineDoesNotRename(t *testing.T) {
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

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		require.ErrorIs(t, e.Flush(ctx), context.DeadlineExceeded, "the lent deadline runs out")

		done := make(chan error, 1)

		go func() { done <- e.Load(t.Context()) }()

		synctest.Wait() // the load has preempted the timed save and waits for the guard
		close(gate)
		require.NoError(t, <-done)

		s.mu.Lock()
		restored := s.restored
		s.mu.Unlock()

		assert.Equal(t, "saved before", restored, "the load restored what the preempted timed save wrote")
	})
}

// restoreHook is a namer whose Restore runs a hook, standing in for the
// requests that run while a Load restores.
type restoreHook struct {
	*namer

	onRestore func()
}

func (h *restoreHook) Restore(io.Reader) error {
	if h.onRestore != nil {
		h.onRestore()
	}

	return nil
}

// Test_Engine_SyncFailingDuringALoadSweepsNothing: a Load preempts a timed save
// stuck in its sync. While the Load restores, a request commits X and the
// abandoned sync fails, after which committed no longer tracks X. The Load's
// sweep must then delete nothing, or it deletes X, which the request goes on to
// link into the live state: a GET of an acknowledged PUT would answer 500 until
// restart (SQ-193 S2).
func Test_Engine_SyncFailingDuringALoadSweepsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), 10*time.Millisecond))
		h := &restoreHook{namer: &namer{}}
		e.Register(syncService, h)

		a, err := e.Assets(syncService)
		require.NoError(t, err)

		commit := func(body string) {
			h.add(commitAsset(t, a, body))
			e.MarkDirty(syncService)
		}

		commit("saved")
		require.NoError(t, e.Flush(t.Context()))

		started, release := gateFirstSync(a, fs.ErrPermission)
		defer release()

		commit("late")
		<-started // the timed save is stuck in its sync

		var inFlight string

		h.onRestore = func() {
			inFlight = commitAsset(t, a, "in flight")
			release()

			for a.failure() == nil {
				time.Sleep(time.Millisecond)
			}
		}

		_ = e.Load(t.Context())
		h.add(inFlight) // the request links the body it committed

		r, err := a.Open(inFlight)
		if err == nil {
			_ = r.Close()
		}

		require.NoError(t, err, "the load swept a body the live state names")
		synctest.Wait()
	})
}

// Test_Engine_CommitDuringS3SnapshotDefersItsSync: s3 has nothing pending when
// the flush takes it, and sorts ahead of sqs. A PUT commits during s3's own
// Snapshot, so s3's save has a sync to run, which here takes the whole
// deadline. The first pass defers s3 and renames sqs, which needs no sync, so
// sqs is on disk although s3 never finishes (SQ-193 S3).
func Test_Engine_CommitDuringS3SnapshotDefersItsSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))

		s3 := &namer{}
		e.Register("s3", s3)

		a, err := e.Assets("s3")
		require.NoError(t, err)

		sqs := &namer{}
		sqs.add("sqs-state")
		e.Register("sqs", sqs)

		gate := make(chan struct{})
		a.fsync = func(*os.File) error { <-gate; return nil }

		s3.during = func() string { return commitAsset(t, a, "put during the snapshot") }
		e.MarkDirty("s3")
		e.MarkDirty("sqs")

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		err = e.Flush(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "s3's sync runs out of time")

		onDisk, _ := os.ReadFile(filepath.Join(dir, "sqs", FormatJSON.fileName()))
		assert.Contains(t, string(onDisk), "sqs-state", "sqs needs no sync and was not saved")
		assert.NoFileExists(t, filepath.Join(dir, "s3", FormatJSON.fileName()), "s3's snapshot names an unsynced asset")

		entries, err := os.ReadDir(filepath.Join(dir, "s3"))
		require.NoError(t, err)

		for _, entry := range entries {
			assert.NotContains(t, entry.Name(), tempSuffix, "the deferred save left its temp file")
		}

		close(gate)
		synctest.Wait()
	})
}

// Test_Engine_TimedSaveCutShortByCloseIsNoFailure: a timed save is in Snapshot
// when Close's deadline runs out and Close releases the assets. The save then
// fails on the released assets, which at exit is no failed flush to log.
func Test_Engine_TimedSaveCutShortByCloseIsNoFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, 10*time.Millisecond)
		body := commitAsset(t, f.a, "body")
		inSnapshot := make(chan struct{})

		f.n.during = func() string {
			close(inSnapshot)
			time.Sleep(500 * time.Millisecond)

			return body
		}
		f.e.MarkDirty(syncService)
		<-inSnapshot

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		require.ErrorIs(t, f.e.Close(ctx), context.DeadlineExceeded)
		time.Sleep(500 * time.Millisecond)
		synctest.Wait() // the timed save has failed on the released assets

		assert.Empty(t, f.logs.errors(), "a save cut short at exit was logged as a failed flush")
	})
}
