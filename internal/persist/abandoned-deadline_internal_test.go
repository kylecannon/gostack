package persist

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingSnap is a Snapshotter whose Snapshot fails for a reason of its own.
type failingSnap struct{}

func (failingSnap) Snapshot(io.Writer) error {
	return errors.New("snapshot failed for a reason of its own")
}

func (failingSnap) Restore(io.Reader) error { return nil }

// exists reports whether anything is at path.
func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// markerOf is where service keeps its marker in the fixture's base dir.
func (f *syncFixture) markerOf(service string) string {
	return filepath.Join(f.e.BaseDir(), service, abandonedFile)
}

// Test_Engine_AbandonedSaveIsMarkedAtItsDeadline checks that the marker is on
// disk when the save's deadline comes, while a Snapshot that cannot be cut
// short is still running, and not after it returns (SQ-431 S2, probe P8: a 2 s
// Snapshot under a 200 ms deadline, and the supervisor's kill 500 ms after the
// deadline). The next start warns, with the Snapshot still running.
func Test_Engine_AbandonedSaveIsMarkedAtItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.commit(t, "late")
		f.n.during = func() string {
			time.Sleep(2 * time.Second)

			return "x"
		}

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- f.e.Flush(ctx) }()

		time.Sleep(700 * time.Millisecond) // where the kill falls

		select {
		case err := <-done:
			t.Fatalf("the flush returned before its Snapshot did: %v", err)
		default:
		}

		assert.FileExists(t, f.abandonedMarker(), "marked at the deadline, with the Snapshot still running")

		warns := f.restart(t)
		require.Len(t, warns, 1, "the next start says so")
		assert.True(t, strings.HasPrefix(warns[0], abandonWarning), warns[0])

		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		assert.FileExists(t, f.abandonedMarker())
	})
}

// Test_Engine_LateSaveClearsTheMarkerOfItsDeadline checks that a service whose
// Snapshot returns after the deadline and then saves, as one with no asset to
// sync does, ends with no marker: it was marked at the deadline, and its save
// removes the marker.
func Test_Engine_LateSaveClearsTheMarkerOfItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.n.add("late")
		f.e.MarkDirty(syncService)
		f.n.during = func() string {
			time.Sleep(time.Second)

			return "x"
		}

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- f.e.Flush(ctx) }()

		time.Sleep(500 * time.Millisecond)
		assert.FileExists(t, f.abandonedMarker(), "marked at the deadline")

		require.NoError(t, <-done)

		assert.FileExists(t, f.stateFile, "the Snapshot returned and the service saved")
		assert.NoFileExists(t, f.abandonedMarker(), "so no marker is left beside a good save")
		assert.Empty(t, f.restart(t))
	})
}

// Test_Engine_MarkAbandonedSkipsAMarkedService checks that a round of markers
// leaves a marker that is there alone, as the second round of a shutdown, at
// Close, finds the first round's (SQ-431 S3): it was truncated and written again
// before, a drive flush each.
func Test_Engine_MarkAbandonedSkipsAMarkedService(t *testing.T) {
	f := newSyncFixture(t, time.Hour)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.abandonedMarker()), dirMode))
	require.NoError(t, os.WriteFile(f.abandonedMarker(), []byte("kept\n"), fileMode))

	f.e.markAbandoned(t.Context(), []string{syncService})

	data, err := os.ReadFile(f.abandonedMarker())
	require.NoError(t, err)
	assert.Equal(t, "kept\n", string(data), "a marker that is there is not written again")
	assert.Empty(t, f.logs.all())
}

// Test_Engine_MarkAbandonedOfEightServices marks all eight services at once, as
// a first round does at a deadline, and logs what that cost: when the last
// marker exists, which is what a kill that follows needs (a process's kill loses
// no file it has written), and when the round has fsynced them all, which a
// power cut needs. The measurement is behind markBudget, and no budget applies
// to it.
func Test_Engine_MarkAbandonedOfEightServices(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	f.e.markBudget = time.Minute

	names := []string{"dynamodb", "dynamodbstreams", "kms", "s3", "secretsmanager", "sns", sqsName, "ssm"}

	for run := range 3 {
		created := make(chan time.Duration, 1)
		start := time.Now()

		go func() {
			for {
				missing := slices.ContainsFunc(names, func(name string) bool { return !exists(f.markerOf(name)) })
				if !missing {
					created <- time.Since(start)

					return
				}

				time.Sleep(50 * time.Microsecond)
			}
		}()

		f.e.markAbandoned(t.Context(), names)
		t.Logf("run %d: %d services, the last marker exists after %s, all fsynced after %s",
			run, len(names), <-created, time.Since(start))

		for _, name := range names {
			require.NoError(t, os.RemoveAll(filepath.Join(f.e.BaseDir(), name)))
		}
	}

	f.e.markAbandoned(t.Context(), names)

	start := time.Now()
	f.e.markAbandoned(t.Context(), names)
	t.Logf("the second round of %d services, which find every marker, took %s", len(names), time.Since(start))
	assert.Empty(t, f.logs.all())
}

// Test_Engine_MarkAbandonedStopsAtItsBudget checks that a round with no time
// left starts no marker and returns, so a round at the Close deadline cannot run
// into the kill. The budget is spent before the round begins.
func Test_Engine_MarkAbandonedStopsAtItsBudget(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	f.e.markBudget = -time.Nanosecond

	names := []string{"s3", sqsName, syncService}
	f.e.markAbandoned(t.Context(), names)

	for _, name := range names {
		assert.NoFileExists(t, f.markerOf(name))
	}

	require.Eventually(t, func() bool { return len(f.logs.all()) > 0 }, 10*time.Second, time.Millisecond,
		"the markers that could not be written are logged")

	for _, name := range names {
		assert.NoFileExists(t, f.markerOf(name))
	}
}

// Test_Engine_AbandonedSaveLeavesAPlantedLinkAlone checks that an abandoned
// save writes nothing through a link planted at the marker's path (SQ-431 N1,
// probe P2): the state file the link points at keeps its content.
func Test_Engine_AbandonedSaveLeavesAPlantedLinkAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.commit(t, "saved")
		require.NoError(t, f.e.Flush(t.Context()))

		before, err := os.ReadFile(f.stateFile)
		require.NoError(t, err)

		err = os.Symlink(filepath.Base(f.stateFile), f.abandonedMarker())
		if err != nil {
			t.Skipf("no links here: %v", err)
		}

		f.commit(t, "late")

		release := f.abandon(t)
		defer release()

		after, err := os.ReadFile(f.stateFile)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "the state file the link points at is untouched")

		info, err := os.Lstat(f.abandonedMarker())
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink, "and the link is still a link")
		assert.Len(t, f.restart(t), 1, "the marker at that path still counts: the start says so")
	})
}

// Test_Engine_AbandonedSaveDoesNotMarkAServiceThatFailedAlone checks that a
// service whose Snapshot failed for a reason of its own, in a flush another
// service ran out of time in, is not marked and not named in the next start's
// warning, whose advice is a longer SHUTDOWN_TIMEOUT, and that the warning of
// the flush names its cause (SQ-431 N2, probe P3).
func Test_Engine_AbandonedSaveDoesNotMarkAServiceThatFailedAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.e.Register("aaa", failingSnap{})
		f.e.MarkDirty("aaa")
		f.commit(t, "late")

		release := f.abandon(t)
		defer release()

		assert.NoFileExists(t, f.markerOf("aaa"), "a failure of its own is no deadline's")
		assert.FileExists(t, f.abandonedMarker())

		var flushWarn string

		for _, line := range f.logs.all() {
			if strings.HasPrefix(line, "persist: save abandoned at its deadline") {
				flushWarn = line
			}
		}

		require.NotEmpty(t, flushWarn)
		assert.Contains(t, flushWarn, "services=["+syncService+"]", "the deadline cut svc alone")
		assert.Contains(t, flushWarn, "aaa: ")
		assert.Contains(t, flushWarn, "for a reason of its own", "and the cause of the other is named")

		warns := f.restart(t)
		require.Len(t, warns, 1)
		assert.Contains(t, warns[0], "services=["+syncService+"]")
		assert.NotContains(t, warns[0], "aaa")
	})
}

// Test_Engine_FlushWaitingForSemMarksTheServicesItDidNotSave checks that a
// Flush whose deadline passes while it waits for the flush that holds sem marks
// the dirty services nothing saved or holds (SQ-431 N3, probe P6, with the
// wait forced rather than drawn): svc changed after the running flush took its
// services. It marks the service that flush holds as well, whatever ctx that
// flush has (SQ-465).
func Test_Engine_FlushWaitingForSemMarksTheServicesItDidNotSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)

		gate := make(chan struct{})
		blocker := &stateSnap{state: "blocker", entered: make(chan struct{}, 1), gate: gate}
		f.e.Register("blocker", blocker)
		f.e.MarkDirty("blocker")

		running := make(chan error, 1)

		go func() { running <- f.e.Flush(t.Context()) }()

		<-blocker.entered // a flush by hand holds sem, inside blocker's Snapshot

		f.commit(t, "late") // svc is dirty, and no flush holds it

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		err := f.e.Flush(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.FileExists(t, f.abandonedMarker(), "marked, though no save of it began")
		assert.FileExists(t, f.markerOf("blocker"), "held by a flush that is stuck, whose own end may come after the kill")

		close(gate)
		require.NoError(t, <-running)
		assert.NoFileExists(t, f.markerOf("blocker"), "the save that completed removed it")
		assert.Len(t, f.restart(t), 1, "and svc, which nothing saved, is the one the start names")
	})
}

// Test_Engine_LentDeadlineMarksTheServicesAtOnce checks that a Flush that ends
// at its deadline, and lends that deadline to the timed flush it shares sem
// with, has the services that flush held marked when it returns, not when the
// Snapshot the timed flush is inside returns: sqs, which waits for s3, is
// marked while s3's Snapshot still runs (SQ-431 S2 through the lent deadline).
func Test_Engine_LentDeadlineMarksTheServicesAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs := &warnings{}
		e := New(warnConfig(dir, 10*time.Millisecond, logs))

		gate := make(chan struct{})
		s3 := &stateSnap{state: "s3 buckets", entered: make(chan struct{}, 1), gate: gate}
		e.Register("s3", s3)
		e.Register(sqsName, followingSnap{stateSnap: &stateSnap{state: "sqs state"}, leader: "s3"})

		e.MarkDirty("s3")
		e.MarkDirty(sqsName)
		<-s3.entered // the timed save is inside s3's Snapshot

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		require.ErrorIs(t, e.Flush(ctx), context.DeadlineExceeded)
		synctest.Wait()

		assert.FileExists(t, filepath.Join(dir, sqsName, abandonedFile), "marked, with s3's Snapshot still running")

		close(gate)
		synctest.Wait()
		require.NoError(t, e.Close(t.Context()))
		assert.NoFileExists(t, filepath.Join(dir, sqsName, abandonedFile), "the save that completes removes it")
		assert.NoFileExists(t, filepath.Join(dir, "s3", abandonedFile), "and so does s3's, which saved late")
	})
}
