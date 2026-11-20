package persist

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of this file are the repros of the SQ-295 review of the persist
// engine (findings F, H, B, C and D), kept as regressions.

// slowSnap is a service whose Snapshot takes d.
type slowSnap struct {
	*stateSnap

	d time.Duration
}

func (s slowSnap) Snapshot(w io.Writer) error {
	time.Sleep(s.d)

	return s.stateSnap.Snapshot(w)
}

// slowFollower is a slowSnap that follows dynamodb.
type slowFollower struct{ slowSnap }

func (slowFollower) Follows() string { return ddb }

// Test_saveOrder_PairGoesByTheSlowerOfLeaderAndFollower: a leader and its
// follower are one unit, ordered by the larger of their two times, so a slow
// follower of a fast leader goes after the fast services (SQ-295 F).
func Test_saveOrder_PairGoesByTheSlowerOfLeaderAndFollower(t *testing.T) {
	log := &orderLog{}
	streams := leading{logged{name: ddbStream, leader: ddb, log: log}}
	pending := []pendingService{
		{name: ddb, snap: logged{name: ddb, log: log}},
		{name: ddbStream, snap: streams, took: 2 * time.Second},
		{name: sqsName, snap: logged{name: sqsName, log: log}, took: time.Second},
	}

	names := make([]string, 0, len(pending))
	for _, p := range saveOrder(pending) {
		names = append(names, p.name)
	}

	require.Equal(t, []string{sqsName, ddb, ddbStream}, names,
		"the pair takes streams' 2 s, which is longer than sqs's 1 s, and the leader still goes first")
}

// Test_Engine_Close_SlowFollowerOfAFastLeaderDoesNotCostSQSItsSave: dynamodb
// and sqs snapshot at once and dynamodbstreams takes 2 s. Once every Snapshot
// is measured, a flush with 300 ms saves sqs before the pair, rather than
// after dynamodb and the slow follower the leader's time alone put in front
// (SQ-295 F).
func Test_Engine_Close_SlowFollowerOfAFastLeaderDoesNotCostSQSItsSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))
		sqs := &stateSnap{state: "sqs v1"}
		e.Register(ddb, &stateSnap{state: "tables"})
		e.Register(ddbStream, slowFollower{slowSnap{stateSnap: &stateSnap{state: "records"}, d: 2 * time.Second}})
		e.Register(sqsName, sqs)

		for _, name := range []string{ddb, ddbStream, sqsName} {
			e.MarkDirty(name)
		}

		require.NoError(t, e.Flush(t.Context())) // every Snapshot is measured now

		sqs.set("sqs v2 acknowledged")

		for _, name := range []string{ddb, sqsName} {
			e.MarkDirty(name)
		}

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		_ = e.Close(ctx)

		assert.Equal(t, "sqs v2 acknowledged", readState(t, filepath.Join(dir, sqsName, FormatJSON.fileName())),
			"sqs is saved before the slow pair")
		synctest.Wait()
	})
}

// Test_Engine_Load_UnopenableBaseDirTurnsSavingOffForEveryService: the base dir
// exists and cannot be opened while Load runs, such as EACCES, EIO or a stale
// mount. The services start empty, so the first save after the fault passes
// would replace the state file nobody read: every service stays unsafe to save,
// with one log line. A missing base dir is still a fresh start (SQ-295 H).
func Test_Engine_Load_UnopenableBaseDirTurnsSavingOffForEveryService(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}

	dir := filepath.Join(t.TempDir(), "base")
	file := filepath.Join(dir, "svc", FormatJSON.fileName())

	e1 := New(quietConfig(dir, time.Hour))
	e1.Register("svc", &stateSnap{state: "everything saved by the last run"})
	e1.Register("other", &stateSnap{state: "another service"})
	e1.MarkDirty("svc")
	e1.MarkDirty("other")
	require.NoError(t, e1.Close(t.Context()))

	require.NoError(t, os.Chmod(dir, 0))

	logs := &warnings{}
	e2 := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})
	s2 := &stateSnap{}
	e2.Register("svc", s2)
	e2.Register("other", &stateSnap{})

	err := e2.Load(t.Context())
	require.ErrorIs(t, err, ErrUnsafe, "Load says the services are off")
	require.ErrorContains(t, err, "persist svc")
	require.ErrorContains(t, err, "persist other")
	assert.False(t, e2.Enabled("svc"), "health reports the service not persisted")
	assert.Len(t, logs.errors(), 1, "one line for the dir, not one per service")

	require.NoError(t, os.Chmod(dir, 0o700)) // the fault passes

	s2.set("one new write")
	e2.MarkDirty("svc")
	require.ErrorIs(t, e2.Close(t.Context()), ErrUnsafe)

	assert.Equal(t, "everything saved by the last run", readState(t, file),
		"the state file the load could not read is not replaced by the next save")

	t.Run("a missing base dir is a fresh start", func(t *testing.T) {
		e := New(quietConfig(filepath.Join(t.TempDir(), "missing"), time.Hour))
		e.Register("svc", &stateSnap{})

		require.NoError(t, e.Load(t.Context()))
		assert.True(t, e.Enabled("svc"))
	})
}

// Test_Engine_SameFailureIsLoggedOnceNotAtEveryRetry: a save fails each time
// with the same cause, at a step whose error text holds the random name of the
// temp file, so the text differs at every retry. The log shows the first
// failure and then every tenth, as the doc says (SQ-295 B).
func Test_Engine_SameFailureIsLoggedOnceNotAtEveryRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}

	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: 10 * time.Millisecond, Logger: slog.New(logs)})
		s := &stateSnap{state: "v1"}
		e.Register("svc", s)
		e.MarkDirty("svc")
		require.NoError(t, e.Flush(t.Context()))

		require.NoError(t, os.Chmod(filepath.Join(dir, "svc"), 0o500)) // creating the temp file fails: EACCES
		defer func() { _ = os.Chmod(filepath.Join(dir, "svc"), 0o700) }()

		s.set("v2")
		e.MarkDirty("svc")

		time.Sleep(5 * time.Minute) // 1, 2, 4, 8, 16, then 30 s of backoff
		synctest.Wait()

		e.mu.Lock()
		failures := e.services["svc"].failures
		e.mu.Unlock()

		require.GreaterOrEqual(t, failures, 11, "the run of failures passes its tenth")
		assert.Len(t, logs.errors(), 2, "the first failure and the tenth, not one per retry")

		require.NoError(t, os.Chmod(filepath.Join(dir, "svc"), 0o700))
		require.NoError(t, e.Close(t.Context()))
	})
}

// Test_Engine_Load_RemovesTheTempFilesOfKilledSaves: a save killed inside its
// Snapshot leaves store.json.tmp-<random>, and neither a restart nor a later
// failed save removed it, so each kill left a file as big as the state. Load
// removes the temp files of a service's state files, in every format, and
// nothing else (SQ-295 C).
func Test_Engine_Load_RemovesTheTempFilesOfKilledSaves(t *testing.T) {
	dir := t.TempDir()

	e1 := New(quietConfig(dir, time.Hour))
	e1.Register("svc", &stateSnap{state: "saved"})
	e1.MarkDirty("svc")
	require.NoError(t, e1.Close(t.Context()))

	svc := filepath.Join(dir, "svc")
	stale := []string{
		FormatJSON.fileName() + tempSuffix + "-killed1",
		FormatJSON.fileName() + tempSuffix + "-killed2",
		FormatBinary.fileName() + tempSuffix + "-killed3",
	}
	kept := []string{"notes.txt", FormatJSON.fileName() + ".backup", "other" + tempSuffix + "-1"}

	for _, name := range slices.Concat(stale, kept) {
		require.NoError(t, os.WriteFile(filepath.Join(svc, name), []byte("x"), 0o600))
	}

	e2 := New(quietConfig(dir, time.Hour))
	s2 := &stateSnap{}
	e2.Register("svc", s2)
	require.NoError(t, e2.Load(t.Context()))

	assert.Equal(t, "saved", s2.restored, "the state file loads")

	for _, name := range stale {
		assert.NoFileExists(t, filepath.Join(svc, name), "a temp file of a state file is removed by Load")
	}

	for _, name := range kept {
		assert.FileExists(t, filepath.Join(svc, name), "Load removes only the temp files of state files")
	}
}

// Test_Engine_Flush_PanickingSnapshotIsLoggedAndTheOthersSave: a Snapshot that
// panics inside a Flush by hand, such as POST /_gostack/state/save, fails its
// own service only. It stays dirty, the panic is logged with its stack, the
// other services in the flush save, and the guard is free for the next flush
// (SQ-295 D).
func Test_Engine_Flush_PanickingSnapshotIsLoggedAndTheOthersSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})
		bad := &stateSnap{state: "bad v1", during: func() error { panic("a service bug") }}
		good := &stateSnap{state: "good v1"}
		e.Register("a-bad", bad)
		e.Register("b-good", good)
		e.MarkDirty("a-bad")
		e.MarkDirty("b-good")

		err := e.Flush(t.Context())
		require.ErrorContains(t, err, "a service bug", "the failure is the caller's to see")

		assert.Equal(t, "good v1", readState(t, filepath.Join(dir, "b-good", FormatJSON.fileName())),
			"the service after the panicking one still saves")
		assert.NoFileExists(t, filepath.Join(dir, "a-bad", FormatJSON.fileName()))

		lines := logs.errors()
		require.NotEmpty(t, lines)
		assert.Contains(t, lines[0], "panicked")
		assert.Contains(t, lines[0], "a service bug")
		assert.Contains(t, lines[0], "a-bad")

		e.mu.Lock()
		dirty := e.services["a-bad"].dirty
		e.mu.Unlock()

		assert.True(t, dirty, "the panicking service stays dirty")

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		require.NoError(t, e.Flush(ctx), "the guard is free, and the Snapshot works the second time")
		assert.Equal(t, "bad v1", readState(t, filepath.Join(dir, "a-bad", FormatJSON.fileName())))
	})
}

// panicsOnCommit is a service whose Committed panics: a panic in the flush
// that is not in a Snapshot.
type panicsOnCommit struct{ *stateSnap }

func (panicsOnCommit) Committed() { panic("committed blew up") }

// Test_Engine_Flush_ReleasesTheGuardWhenAPanicPassesThrough: a panic that
// leaves the flush, which the caller recovers as net/http recovers a handler's,
// does not hold the guard for ever, so shutdown's flush still saves (SQ-295 D).
func Test_Engine_Flush_ReleasesTheGuardWhenAPanicPassesThrough(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Hour))
		good := &stateSnap{state: "good v1"}
		e.Register("a-bad", panicsOnCommit{&stateSnap{state: "bad"}})
		e.Register("b-good", good)
		e.MarkDirty("a-bad")

		require.PanicsWithValue(t, "committed blew up", func() { _ = e.Flush(t.Context()) })

		good.set("good v2 acknowledged")
		e.MarkDirty("b-good")

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		err := e.Close(ctx)
		require.NotErrorIs(t, err, context.DeadlineExceeded, "the guard is not held: %v", err)
		assert.Equal(t, "good v2 acknowledged", readState(t, filepath.Join(dir, "b-good", FormatJSON.fileName())),
			"shutdown saves after the panic")
	})
}
