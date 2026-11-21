package persist

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here are review SQ-440's probes P12 to P15 and P18 (round 2 of
// SQ-427), rewritten as assertions. Each fails on 411a64d.

// reviewEngine returns an engine on a new base dir that logs to logs.
func reviewEngine(t *testing.T, freq time.Duration) (*Engine, string, *warnings) {
	t.Helper()

	dir := t.TempDir()
	logs := &warnings{}

	return New(Config{PersistVarSet: true, BaseDir: dir, Frequency: freq, Logger: slog.New(logs)}), dir, logs
}

// markedIn reports whether service has a marker in dir.
func markedIn(dir, service string) bool {
	return exists(filepath.Join(dir, service, abandonedFile))
}

// startWarnings returns the warn records of a new engine on a copy of dir with
// the services named, once it has loaded: what the next start says. It is a
// copy because Load removes the temp files of a save a crash cut short, and the
// engine whose dir it is has saves under way.
func startWarnings(t *testing.T, dir string, names ...string) []string {
	t.Helper()

	copied := t.TempDir()
	require.NoError(t, os.CopyFS(copied, os.DirFS(dir)))

	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: copied, Frequency: time.Hour, Logger: slog.New(logs)})

	for _, name := range names {
		e.Register(name, &stateSnap{})
	}

	require.NoError(t, e.Load(t.Context()))

	return logs.all()
}

// isDirty reports whether the engine holds a change of service no save took.
func isDirty(e *Engine, service string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.services[service].dirty
}

// Test_Engine_DeadlineMarksAServiceChangedAfterItsSave is probe P14: aaa is
// saved, changes while the flush is inside zzz's Snapshot, and the Snapshot
// outlasts the deadline and the kill. aaa's change is lost with zzz's, so aaa
// is named as well, though no save of it was abandoned: the deadline marks every
// service that is dirty, not only the ones the flush held. It is the Flush that
// holds sem alone, which Stop is.
func Test_Engine_DeadlineMarksAServiceChangedAfterItsSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, dir, _ := reviewEngine(t, time.Hour)

		gate := make(chan struct{})
		aaa := &stateSnap{state: "v1"}
		zzz := &stateSnap{state: "z", entered: make(chan struct{}, 1), gate: gate}
		e.Register("aaa", aaa)
		e.Register("zzz", zzz)
		e.MarkDirty("aaa")
		e.MarkDirty("zzz")

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- e.Stop(ctx) }()

		<-zzz.entered // aaa is saved, and the flush is inside the Snapshot of zzz

		aaa.mu.Lock()
		aaa.state = "v2"
		aaa.mu.Unlock()
		e.MarkDirty("aaa")

		time.Sleep(600 * time.Millisecond) // the kill, 500 ms after the save's deadline
		synctest.Wait()

		assert.True(t, markedIn(dir, "zzz"), "the service whose Snapshot outlasts the deadline")
		assert.True(t, markedIn(dir, "aaa"), "and the one that changed after its save")

		warns := startWarnings(t, dir, "aaa", "zzz")
		require.Len(t, warns, 1, "the next start says so")
		assert.Contains(t, warns[0], "services=[aaa zzz]")

		close(gate)
		require.NoError(t, <-done, "zzz saved, late")
		synctest.Wait()

		assert.False(t, markedIn(dir, "zzz"), "zzz saved, late, and is not dirty")
		assert.True(t, markedIn(dir, "aaa"), "aaa's change is still unsaved")
	})
}

// Test_Engine_DeadlineMarksAChangeAfterTheRound checks a change made after the
// round at the deadline, while the Snapshot the flush is inside returns later:
// the flush's end marks it, so the flush that saved everything it took and ended
// past its deadline names the service that changed since.
func Test_Engine_DeadlineMarksAChangeAfterTheRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, dir, _ := reviewEngine(t, time.Hour)

		gate := make(chan struct{})
		zzz := &stateSnap{state: "z", entered: make(chan struct{}, 1), gate: gate}
		e.Register("aaa", &stateSnap{state: "a"})
		e.Register("zzz", zzz)
		e.MarkDirty("zzz")

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- e.Flush(ctx) }()

		<-zzz.entered
		time.Sleep(200 * time.Millisecond) // past the deadline, and the round's marks
		synctest.Wait()
		assert.False(t, markedIn(dir, "aaa"), "aaa had not changed at the round")

		e.MarkDirty("aaa")
		close(gate)

		require.NoError(t, <-done, "zzz saved, late")
		assert.True(t, markedIn(dir, "aaa"), "the end of the flush marked the change made after the round")
		assert.False(t, markedIn(dir, "zzz"))
	})
}

// Test_Engine_LateSaveKeepsTheMarkerOfANewerChange is probe P13: sqs is
// snapshotted at v1, changes to v2 while the Snapshot outlasts the deadline, and
// the save then completes at v1. The save removed the marker, though v2 is
// unsaved and the process is killed before anything saves it: the start after
// it was quiet. The marker stays while the service is dirty, and the next save
// that finds it unchanged removes it.
func Test_Engine_LateSaveKeepsTheMarkerOfANewerChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, dir, logs := reviewEngine(t, time.Hour)

		gate := make(chan struct{})
		sqs := &stateSnap{state: "v1", entered: make(chan struct{}, 1), gate: gate}
		e.Register("sqs", sqs)
		e.MarkDirty("sqs")

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- e.Stop(ctx) }()

		<-sqs.entered // the Snapshot holds v1

		time.Sleep(50 * time.Millisecond)
		sqs.mu.Lock()
		sqs.state = "v2"
		sqs.mu.Unlock()
		e.MarkDirty("sqs")

		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assert.True(t, markedIn(dir, "sqs"), "marked at the deadline")

		close(gate)
		require.NoError(t, <-done)
		synctest.Wait()

		assert.Equal(t, "v1", sq447State(t, dir, "sqs"), "the save that completed is v1")
		assert.True(t, markedIn(dir, "sqs"), "and v2 is unsaved, so the marker stays")
		assert.True(t, isDirty(e, "sqs"))
		assert.Empty(t, logs.all())

		warns := startWarnings(t, dir, "sqs")
		require.Len(t, warns, 1, "so the next start warns")
		assert.Contains(t, warns[0], "services=[sqs]")

		require.NoError(t, e.Flush(t.Context()))
		assert.Equal(t, "v2", sq447State(t, dir, "sqs"))
		assert.False(t, markedIn(dir, "sqs"), "a save of v2 removes it")
	})
}

// sq447State returns the content of service's state file in dir.
func sq447State(t *testing.T, dir, service string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, service, FormatJSON.fileName()))
	require.NoError(t, err)

	return string(data)
}

// Test_Engine_SharedFlushLendsItsDeadlineWhenItComes is probe P12: the timed
// flush is inside aaa's Snapshot, and the Flush that shares sem with it is
// inside bbb's when its deadline comes. aaa is marked at that deadline, by the
// lend the deadline makes, not 1.8 s later when bbb's Snapshot returns and the
// Flush waits for the timed flush.
func Test_Engine_SharedFlushLendsItsDeadlineWhenItComes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, dir, _ := reviewEngine(t, 10*time.Millisecond)

		gateA, gateB := make(chan struct{}), make(chan struct{})
		a := &stateSnap{state: "a", entered: make(chan struct{}, 1), gate: gateA}
		b := &stateSnap{state: "b", entered: make(chan struct{}, 1), gate: gateB}
		e.Register("aaa", a)
		e.Register("bbb", b)

		e.MarkDirty("aaa")
		<-a.entered // the timed save is inside the Snapshot of aaa

		e.MarkDirty("bbb")

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- e.Flush(ctx) }()

		<-b.entered // the Flush by hand is inside the Snapshot of bbb

		time.Sleep(700 * time.Millisecond) // the kill, 500 ms after the deadline
		synctest.Wait()
		assert.True(t, markedIn(dir, "aaa"), "the timed flush's service, marked by the lent deadline")
		assert.True(t, markedIn(dir, "bbb"), "and the Flush's own")

		close(gateB)
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		close(gateA)
		synctest.Wait()
		require.NoError(t, e.Close(t.Context()))
	})
}

// Test_Engine_LendingFlushReturnsAfterTheMarksItCaused is probe P18: a Flush
// whose deadline passes while it waits for the timed flush lends the deadline
// and marks what that flush holds, and does not return before those markers
// exist. The probe found it absent on return 40 times of 40, up to 6.9 ms late.
func Test_Engine_LendingFlushReturnsAfterTheMarksItCaused(t *testing.T) {
	for range 10 {
		e, dir, _ := reviewEngine(t, time.Millisecond)

		gate := make(chan struct{})
		a := &stateSnap{state: "a", entered: make(chan struct{}, 1), gate: gate}
		e.Register("aaa", a)
		e.MarkDirty("aaa")
		<-a.entered // the timed save is inside aaa's Snapshot

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		err := e.Flush(ctx)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.True(t, markedIn(dir, "aaa"), "the marker is there when Flush returns")

		cancel()
		close(gate)
		require.NoError(t, e.Close(t.Context()))
	}
}

// Test_Engine_AbandonedSaveNamesAFollowerOfAFailedLeaderApart is probe P15: s3
// fails for a reason of its own, its follower sqs is given way to it, and svc
// runs out of time. sqs was named, and marked, as cut by the deadline, though a
// longer SHUTDOWN_TIMEOUT would not have saved it. It is named with the cause of
// its leader's failure under failed_for_other_reasons, and not marked.
func Test_Engine_AbandonedSaveNamesAFollowerOfAFailedLeaderApart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.e.Register("s3", failingSnap{})
		f.e.Register(sqsName, followingSnap{stateSnap: &stateSnap{state: "q"}, leader: "s3"})
		f.e.MarkDirty("s3")
		f.e.MarkDirty(sqsName)
		f.commit(t, "late")

		release := f.abandon(t)
		defer release()

		assert.NoFileExists(t, f.markerOf("s3"), "a failure of its own is no deadline's")
		assert.NoFileExists(t, f.markerOf(sqsName), "and its follower was lost to it, not to the deadline")
		assert.FileExists(t, f.abandonedMarker(), "svc is the one the deadline cut")

		var flushWarn string

		for _, line := range f.logs.all() {
			if strings.HasPrefix(line, "persist: save abandoned at its deadline") {
				flushWarn = line
			}
		}

		require.NotEmpty(t, flushWarn)
		assert.Contains(t, flushWarn, "services=["+syncService+"]", "the deadline cut svc alone")
		assert.Contains(t, flushWarn, "failed_for_other_reasons=[")
		assert.Contains(t, flushWarn, "s3: ")
		assert.Contains(t, flushWarn, sqsName+": "+errLeaderUnsaved.Error()+" (s3: ", "sqs, with its leader's cause")

		warns := f.restart(t)
		require.Len(t, warns, 1)
		assert.Contains(t, warns[0], "services=["+syncService+"]")
	})
}
