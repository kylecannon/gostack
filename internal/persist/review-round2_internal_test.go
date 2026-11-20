package persist

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// The tests of this file are the repros of the SQ-386 review of the persist
// engine (findings D, H, B and C), kept as regressions: each failed on 2112c11.

// The states the services of these tests hold.
const (
	badState     = "round 2 bad"
	goodState    = "round 2 good v1"
	tablesState  = "round 2 tables"
	recordsState = "round 2 records"
)

// leaderCommitPanics is a [Leader] whose first Committed panics, and that
// records what it was told.
type leaderCommitPanics struct {
	*stateSnap

	calls     *[]bool
	committed *int
}

func (l leaderCommitPanics) Followed(saved bool) { *l.calls = append(*l.calls, saved) }

func (l leaderCommitPanics) Committed() {
	*l.committed++

	if *l.committed == 1 {
		panic("committed blew up")
	}
}

// streamFollower follows dynamodb.
type streamFollower struct{ *stateSnap }

func (streamFollower) Follows() string { return ddb }

// leaderFollowedPanics is a [Leader] whose Followed(true) panics.
type leaderFollowedPanics struct{ *stateSnap }

func (leaderFollowedPanics) Followed(saved bool) { //nolint:revive // the signature of Leader
	if saved {
		panic("followed blew up")
	}
}

// panicsAlways is a service whose Snapshot panics at every call.
type panicsAlways struct{ *stateSnap }

func (panicsAlways) Snapshot(io.Writer) error { panic("always") }

// panicsUnderItsLock takes its lock without a defer, as secretsmanager's
// Snapshot does per secret, and panics under it once: the lock stays held, and
// a second Snapshot waits for it for ever.
type panicsUnderItsLock struct {
	mu    sync.Mutex
	calls atomic.Int32
}

func (s *panicsUnderItsLock) Snapshot(w io.Writer) error {
	n := s.calls.Add(1)

	s.mu.Lock()

	if n == 1 {
		panic("a bug under the lock")
	}

	s.mu.Unlock()

	_, err := io.WriteString(w, "state")

	return err //nolint:wrapcheck // a test double
}

func (*panicsUnderItsLock) Restore(io.Reader) error { return nil }

// timerArmed reports whether the engine has a flush timer armed.
func timerArmed(e *Engine) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.timer != nil
}

// Test_Engine_Flush_APanicAfterTheSnapshotLeavesTheServicesItTookDirty: a-bad
// and b-good are both dirty. The flush by hand saves a-bad, whose Committed
// panics; the caller recovers, as net/http does. b-good was taken by that flush
// and not written: it is dirty again, the guard is free and the flush is over,
// so a clean shutdown saves it (SQ-386 D1).
func Test_Engine_Flush_APanicAfterTheSnapshotLeavesTheServicesItTookDirty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))
		e.Register("a-bad", panicsOnCommit{&stateSnap{state: badState}})
		e.Register("b-good", &stateSnap{state: "good v1 acknowledged"})
		e.MarkDirty("a-bad")
		e.MarkDirty("b-good")

		require.PanicsWithValue(t, "committed blew up", func() { _ = e.Flush(t.Context()) })

		e.mu.Lock()
		flushing, dirty := e.flushing, e.services["b-good"].dirty
		e.mu.Unlock()

		assert.False(t, flushing, "the flush is over")
		assert.True(t, dirty, "the service the flush took and did not write is dirty again")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		require.NoError(t, e.Close(ctx))
		assert.Equal(t, "good v1 acknowledged", readState(t, filepath.Join(dir, "b-good", FormatJSON.fileName())),
			"a clean shutdown saves it")
	})
}

// Test_Engine_Flush_TimerSavesAgainAfterAPanickedFlush: PERSIST_FREQUENCY is
// 10 ms. After the flush by hand a panic passed through, a write is acknowledged
// and marked dirty. The timer saves it; before, flushing stayed set and no
// timer was armed again, so an hour passed with nothing saved (SQ-386 D2).
func Test_Engine_Flush_TimerSavesAgainAfterAPanickedFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		good := &stateSnap{state: goodState}
		e.Register("a-bad", panicsOnCommit{&stateSnap{state: badState}})
		e.Register("b-good", good)
		e.MarkDirty("a-bad")
		e.MarkDirty("b-good")

		require.PanicsWithValue(t, "committed blew up", func() { _ = e.Flush(t.Context()) })

		assert.True(t, timerArmed(e), "what the flush left dirty has a timer")

		good.set("good v2 acknowledged")
		e.MarkDirty("b-good")

		time.Sleep(time.Second)
		synctest.Wait()

		assert.Equal(t, "good v2 acknowledged", readState(t, filepath.Join(dir, "b-good", FormatJSON.fileName())),
			"the timer saved the write without a flush by hand")

		require.NoError(t, e.Close(t.Context()))
	})
}

// Test_Engine_Flush_LeaderIsToldTheFlushEndedWhenAPanicPassesThrough: the
// leader is told Followed(true), is saved, and its Committed panics. It is told
// Followed(false) all the same, and its follower, taken and not written, is
// saved by the shutdown (SQ-386 D3).
func Test_Engine_Flush_LeaderIsToldTheFlushEndedWhenAPanicPassesThrough(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))

		var (
			calls     []bool
			committed int
		)

		e.Register(ddb, leaderCommitPanics{&stateSnap{state: tablesState}, &calls, &committed})
		e.Register(ddbStream, streamFollower{&stateSnap{state: recordsState}})
		e.MarkDirty(ddb)
		e.MarkDirty(ddbStream)

		require.PanicsWithValue(t, "committed blew up", func() { _ = e.Flush(t.Context()) })
		require.NotEmpty(t, calls)
		assert.Equal(t, []bool{true, false}, calls, "the leader keeps nothing for a follower after the flush")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		require.NoError(t, e.Close(ctx))
		assert.Equal(t, recordsState, readState(t, filepath.Join(dir, ddbStream, FormatJSON.fileName())),
			"the dirty follower is saved by the clean shutdown")
	})
}

// Test_Engine_Flush_APanicInFollowedFailsTheLeaderWithABackoff: a leader whose
// Followed panics is the service under way, so it fails with a backoff and the
// timer does not call it at every PERSIST_FREQUENCY; the follower it left is
// dirty again with none. Its Followed(false) is called, because the leader may
// have kept something when it was told true.
func Test_Engine_Flush_APanicInFollowedFailsTheLeaderWithABackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: 10 * time.Millisecond, Logger: slog.New(logs)})
		e.Register(ddb, leaderFollowedPanics{&stateSnap{state: tablesState}})
		e.Register(ddbStream, streamFollower{&stateSnap{state: recordsState}})
		e.MarkDirty(ddb)
		e.MarkDirty(ddbStream)

		time.Sleep(10 * time.Second)
		synctest.Wait()

		e.mu.Lock()
		failures := e.services[ddb].failures
		e.mu.Unlock()

		assert.Positive(t, failures, "the leader failed")
		assert.Less(t, failures, 6, "the timer backed off instead of calling it every 10 ms")
		assert.Len(t, logs.errors(), failures, "each panic is logged once, with its stack")

		require.Panics(t, func() { _ = e.Close(t.Context()) })
	})
}

// Test_Engine_Timer_APanicOutsideSnapshotIsLoggedAndTheNextFlushSaves: the
// same panic on the timer's flush would end the process, since the timer's
// goroutine is gostack's own. It is logged with its stack, and the services it
// took are saved by the next flush.
func Test_Engine_Timer_APanicOutsideSnapshotIsLoggedAndTheNextFlushSaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: 10 * time.Millisecond, Logger: slog.New(logs)})
		e.Register("a-bad", panicsOnCommit{&stateSnap{state: badState}})
		e.Register("b-good", &stateSnap{state: "good"})
		e.MarkDirty("a-bad")
		e.MarkDirty("b-good")

		time.Sleep(time.Second)
		synctest.Wait()

		lines := logs.errors()
		require.Len(t, lines, 1, "one panic, one line")
		assert.Contains(t, lines[0], "committed blew up")
		assert.Contains(t, lines[0], "stack=")

		assert.Equal(t, "good", readState(t, filepath.Join(dir, "b-good", FormatJSON.fileName())))
		assert.Equal(t, badState, readState(t, filepath.Join(dir, "a-bad", FormatJSON.fileName())),
			"the service whose Committed panicked was renamed before it")

		require.NoError(t, e.Close(t.Context()))
	})
}

// Test_Engine_Timer_APanickingSnapshotTurnsItsServiceOffAndIsLoggedOnce: a
// Snapshot that panics at every call, PERSIST_FREQUENCY 10 ms, five minutes. The
// first panic turns saving off for the service, so it is neither retried nor
// logged again; the other service keeps saving (SQ-386 D4).
func Test_Engine_Timer_APanickingSnapshotTurnsItsServiceOffAndIsLoggedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: 10 * time.Millisecond, Logger: slog.New(logs)})
		good := &stateSnap{state: goodState}
		e.Register("a-bad", panicsAlways{&stateSnap{state: "v1"}})
		e.Register("b-good", good)
		e.MarkDirty("a-bad")
		e.MarkDirty("b-good")

		time.Sleep(5 * time.Minute)
		synctest.Wait()

		lines := logs.errors()
		require.Len(t, lines, 1, "one line for the panic, no more at the retries")
		assert.Contains(t, lines[0], "panicked")
		assert.Contains(t, lines[0], "stack=")
		assert.False(t, e.Enabled("a-bad"), "health reports the service not persisted")

		good.set("good v2")
		e.MarkDirty("b-good")
		time.Sleep(time.Second)
		synctest.Wait()

		assert.Equal(t, "good v2", readState(t, filepath.Join(dir, "b-good", FormatJSON.fileName())),
			"the other service keeps saving")

		err := e.Close(t.Context())
		require.ErrorIs(t, err, ErrUnsafe, "the flush by hand reports the service that is off")
		assert.Len(t, logs.errors(), 1, "and logs nothing more")
	})
}

// Test_Engine_Close_APanicUnderALockDoesNotWedgeShutdown: the panic leaves the
// service's lock held. The timer's retry used to wait for that lock inside the
// guard, and shutdown could save no service. The service is off after the
// panic, so the engine never calls it again (SQ-386 D5).
func Test_Engine_Close_APanicUnderALockDoesNotWedgeShutdown(t *testing.T) {
	dir := t.TempDir()
	e := New(quietConfig(dir, 10*time.Millisecond))
	bad := &panicsUnderItsLock{}
	sqs := &stateSnap{state: "sqs v1"}
	e.Register("a-bad", bad)
	e.Register(sqsName, sqs)
	e.MarkDirty("a-bad")
	e.MarkDirty(sqsName)

	require.Eventually(t, func() bool { return !e.Enabled("a-bad") }, 10*time.Second, 5*time.Millisecond,
		"the panic turns the service off")

	time.Sleep(2 * time.Second) // past the backoff a retry would wait for

	sqs.set("sqs v2 acknowledged")
	e.MarkDirty(sqsName)

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err := e.Close(ctx)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int32(1), bad.calls.Load(), "the service is called once")
	assert.Equal(t, "sqs v2 acknowledged", readState(t, filepath.Join(dir, sqsName, FormatJSON.fileName())))
}

// Test_Engine_Load_ALaterLoadThatCannotOpenTheBaseDirChangesNothing: the
// process started and saved. A later Load (POST /_gostack/state/load) runs while
// the base dir cannot be opened. It returns its error and changes nothing: the
// service is still persisted, nothing is restored, and once the fault passes a
// write is saved (SQ-386 H1).
func Test_Engine_Load_ALaterLoadThatCannotOpenTheBaseDirChangesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}

	dir := filepath.Join(t.TempDir(), "base")
	file := filepath.Join(dir, "svc", FormatJSON.fileName())
	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})
	s := &stateSnap{state: "v1"}
	e.Register("svc", s)
	require.NoError(t, e.Load(t.Context()))
	e.MarkDirty("svc")
	require.NoError(t, e.Flush(t.Context()))
	s.set("v2 in memory")

	require.NoError(t, os.Chmod(dir, 0))

	err := e.Load(t.Context())
	require.NoError(t, os.Chmod(dir, 0o700)) // the fault passes

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnsafe, "no service is refused")
	assert.True(t, e.Enabled("svc"), "health still reports the service persisted")
	assert.Equal(t, "v2 in memory", s.state, "nothing was restored over the memory")
	assert.Empty(t, logs.errors(), "the caller has the error; the engine logs no refusal")

	e.MarkDirty("svc")
	require.NoError(t, e.Close(t.Context()))
	assert.Equal(t, "v2 in memory", readState(t, file), "the write made after the fault is saved")
}

// Test_Engine_Load_AStartupRefusalIsLoggedOnce: the startup Load refuses every
// service, and a later one while the base dir still cannot be opened refuses no
// new one, so it logs nothing and the shutdown that follows adds no line.
func Test_Engine_Load_AStartupRefusalIsLoggedOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}

	dir := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0))

	defer func() { _ = os.Chmod(dir, 0o700) }()

	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})
	e.Register("svc", &stateSnap{})

	require.ErrorIs(t, e.Load(t.Context()), ErrUnsafe)
	require.ErrorIs(t, e.Load(t.Context()), ErrUnsafe)
	require.Len(t, logs.errors(), 1)
	assert.Contains(t, logs.errors()[0], "at startup", "the line says it was the startup Load")
}

// Test_Engine_SameFailureIsNotHiddenBehindTheSameErrno: the save fails with
// EACCES creating the temp file, and then the base dir itself cannot be opened:
// another failure with the same errno at the root of both. The new one is
// logged at its first occurrence (SQ-386 B1).
func Test_Engine_SameFailureIsNotHiddenBehindTheSameErrno(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}

	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "base")
		svc := filepath.Join(dir, "svc")
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: 10 * time.Millisecond, Logger: slog.New(logs)})
		s := &stateSnap{state: "v1"}
		e.Register("svc", s)
		e.MarkDirty("svc")
		require.NoError(t, e.Flush(t.Context()))

		require.NoError(t, os.Chmod(svc, 0o500))

		s.set("v2")
		e.MarkDirty("svc")
		time.Sleep(3 * time.Second)
		synctest.Wait()

		require.NoError(t, os.Chmod(svc, 0o700))
		require.NoError(t, os.Chmod(dir, 0))

		defer func() { _ = os.Chmod(dir, 0o700) }()

		time.Sleep(10 * time.Second)
		synctest.Wait()

		var created, opened int

		for _, line := range logs.errors() {
			if strings.Contains(line, "create svc/") {
				created++
			}

			if strings.Contains(line, "open base dir") {
				opened++
			}
		}

		assert.Equal(t, 1, created, "the first failure is logged once: %v", logs.errors())
		assert.Equal(t, 1, opened, "the different failure is logged at its first occurrence: %v", logs.errors())

		require.NoError(t, os.Chmod(dir, 0o700))
		require.NoError(t, e.Close(t.Context()))
	})
}

// Test_failureKey: the key is the set of leaf errors with the text of the wraps
// around each, random temp names replaced, so that a Join or a multi-%w node
// neither hides a new failure behind one of its leaves nor shows the same one
// as new when its order changes.
func Test_failureKey(t *testing.T) {
	errA := errors.New("no space left on device")
	errB := errors.New("permission denied")
	temp := func(name string) error { return fmt.Errorf("create svc/a.json.tmp-%s: %w", name, errA) }

	same := func(a, b error) {
		t.Helper()
		assert.Equal(t, failureKey(a), failureKey(b))
	}
	differ := func(a, b error) {
		t.Helper()
		assert.NotEqual(t, failureKey(a), failureKey(b))
	}

	same(temp("AAAAAAAAAAAAAAAAAAAAAAAAAA"), temp("BBBBBBBBBBBBBBBBBBBBBBBBBB"))
	differ(temp("AAAAAAAAAAAAAAAAAAAAAAAAAA"), fmt.Errorf("open base dir: %w", errA))
	same(errors.Join(errA, errB), errors.Join(errB, errA))
	same(fmt.Errorf("x: %w: %w", errA, errB), fmt.Errorf("x: %w: %w", errA, errB))
	differ(errors.Join(errA, errB), errors.Join(errA, errors.New("another")))
	differ(errors.Join(errA, errB), errA)
	assert.Empty(t, failureKey(nil))
}

// Test_Engine_Load_RemovesTheTempFilesBesideALinkedStateFilesTarget: the state
// file is a relative link, in each form. A save puts its temp file beside the
// file the link leads to, so that is where a killed save leaves one, and Load
// sweeps it there, and only the temp names of that file (SQ-386 C1).
func Test_Engine_Load_RemovesTheTempFilesBesideALinkedStateFilesTarget(t *testing.T) {
	for _, link := range []string{"../shared/svc.json", "real.json"} {
		t.Run(link, func(t *testing.T) {
			dir := t.TempDir()
			svc := filepath.Join(dir, "svc")
			require.NoError(t, os.MkdirAll(svc, 0o750))
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "shared"), 0o750))
			require.NoError(t, os.Symlink(link, filepath.Join(svc, FormatJSON.fileName())))

			linked := filepath.Join(svc, link)

			e1 := New(quietConfig(dir, time.Hour))
			e1.Register("svc", &stateSnap{state: "saved"})
			e1.MarkDirty("svc")
			require.NoError(t, e1.Close(t.Context()))
			require.FileExists(t, linked)

			// What a SIGKILL inside a Snapshot leaves, and what is not a temp file of it.
			temp := linked + tempSuffix + "-KILLEDINSNAPSHOT"
			unrelated := filepath.Join(filepath.Dir(linked), "other.json"+tempSuffix+"-1")
			require.NoError(t, os.WriteFile(temp, make([]byte, 1<<10), 0o600))
			require.NoError(t, os.WriteFile(unrelated, []byte("x"), 0o600))

			e2 := New(quietConfig(dir, time.Hour))
			s2 := &stateSnap{}
			e2.Register("svc", s2)
			require.NoError(t, e2.Load(t.Context()))

			assert.Equal(t, "saved", s2.restored)
			assert.NoFileExists(t, temp, "the temp file of a killed save of a linked state file is removed")
			assert.FileExists(t, unrelated, "only the temp files of the state file go")
		})
	}
}
