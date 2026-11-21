package persist

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here are the follow-ups of PR #56's review (SQ-465): a ctx with no
// deadline that ends with a deadline's cause, a marker whose name differs in
// case, a Stop behind a Flush that has a deadline later than its own or none, a
// held service whose save has renamed, and a state file linked to the marker's
// name.

// markerOf returns where the marker of service is in dir.
func markerOf(dir, service string) string {
	return filepath.Join(dir, service, abandonedFile)
}

// gatedSnap returns a Snapshotter whose Snapshot waits for gate to be closed,
// and signals on its entered channel when it has begun.
func gatedSnap(state string) (snap *stateSnap, gate chan struct{}) {
	gate = make(chan struct{})

	return &stateSnap{state: state, gate: gate, entered: make(chan struct{}, 1)}, gate
}

// watchState returns what e's slow watch holds: the flushes registered, the ones
// armed, and whether its timer is set.
func watchState(e *Engine) (runs, armed int, timed bool) {
	e.slow.mu.Lock()
	defer e.slow.mu.Unlock()

	for _, run := range e.slow.runs {
		if run.slow.stop != nil {
			armed++
		}
	}

	return len(e.slow.runs), armed, e.slow.timed
}

// startAbandonWarnings returns the warnings that say a save was abandoned, which
// an engine on dir logs as it loads its services. It counts none of the others
// the start logs.
func startAbandonWarnings(t *testing.T, dir string, services ...string) []string {
	t.Helper()

	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})

	for _, name := range services {
		e.Register(name, &stateSnap{})
	}

	require.NoError(t, e.Load(t.Context()))

	return abandonWarnings(logs.all())
}

// Test_Engine_FlushMarksAContextWithNoDeadlineThatEndsWithADeadlineCause checks
// when a Flush whose ctx has no deadline, and that ends with the cause of one,
// marks the service it holds and the one changed since: with the Snapshot it is
// inside not returned, the same as a ctx that ends at a deadline, since the kill
// the deadline stands for would come then, but not before the flush has lasted
// markWatchAfter, which is what keeps a flush in time from paying for it. A ctx
// that ends with any other cause marks nothing, being no deadline. The clock is
// the fake clock of a bubble, so the instants are asserted to the nanosecond.
func Test_Engine_FlushMarksAContextWithNoDeadlineThatEndsWithADeadlineCause(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		// end is when ctx ends, and marked when the markers are there, both from the
		// start of the flush; marked is zero when there are none.
		end, marked time.Duration
	}{
		{"deadline cause before the watch begins", context.DeadlineExceeded, 0, markWatchAfter},
		{"deadline cause after the watch began", context.DeadlineExceeded, 10 * markWatchAfter, 10 * markWatchAfter},
		{"another cause", context.Canceled, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				e := New(quietConfig(dir, time.Hour))
				snap, gate := gatedSnap("x")
				e.Register("svc", snap)
				e.Register("other", &stateSnap{state: "o"})
				e.MarkDirty("svc")

				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)

				done := make(chan error, 1)

				go func() { done <- e.Flush(ctx) }()

				<-snap.entered

				begun := time.Now()

				// A change to a service the flush does not hold.
				e.MarkDirty("other")

				marked := func() bool {
					return exists(markerOf(dir, "svc")) || exists(markerOf(dir, "other"))
				}

				time.Sleep(tt.end)
				synctest.Wait()
				assert.False(t, marked(), "nothing has ended")

				cancel(tt.cause)
				synctest.Wait()

				if tt.marked == 0 {
					time.Sleep(10 * markWatchAfter)
					synctest.Wait()
					assert.False(t, marked(), "no deadline ended it")
				} else {
					if wait := tt.marked - time.Since(begun); wait > 0 {
						time.Sleep(wait - time.Nanosecond)
						synctest.Wait()
						assert.False(t, marked(), "1 ns before the watch begins")

						time.Sleep(time.Nanosecond)
						synctest.Wait()
					}

					assert.FileExists(t, markerOf(dir, "svc"), "held, with its Snapshot not returned")
					assert.FileExists(t, markerOf(dir, "other"), "changed since")
					assert.Equal(t, tt.marked, time.Since(begun), "at the instant it was due")
				}

				// The Snapshot returns, and its save completes: what it saved needs no
				// marker, and the service changed since is still unsaved.
				close(gate)
				require.NoError(t, <-done)

				assert.NoFileExists(t, markerOf(dir, "svc"), "its save completed")

				if tt.marked > 0 {
					assert.FileExists(t, markerOf(dir, "other"), "changed since, and unsaved")
				}

				runs, armed, timed := watchState(e)
				assert.Zero(t, runs, "the flush that ended is no longer watched")
				assert.Zero(t, armed)
				assert.False(t, timed, "nothing to look at")

				require.NoError(t, e.Stop(t.Context()))
			})
		})
	}
}

// Test_Engine_SlowWatchLooksAgainForAFlushThatBeganLater checks that the watch,
// having armed one flush, sets its timer for the next one that has not yet lasted
// as long: two Flushes that began 20 ms apart, beside a flush by the timer, are
// armed at 50 ms and at 70 ms of the first's start, and the one armed last
// marks when it ends with a deadline's cause.
func Test_Engine_SlowWatchLooksAgainForAFlushThatBeganLater(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, time.Millisecond))

		timerSnap, timerGate := gatedSnap("t")
		first, firstGate := gatedSnap("a")
		second, secondGate := gatedSnap("b")
		e.Register("ttt", timerSnap)
		e.Register("aaa", first)
		e.Register("bbb", second)

		// The flush by the timer holds ttt, so a Flush by hand can run beside it.
		e.MarkDirty("ttt")
		<-timerSnap.entered

		aCtx, aCancel := context.WithCancelCause(t.Context())
		bCtx, bCancel := context.WithCancelCause(t.Context())
		aDone, bDone := make(chan error, 1), make(chan error, 1)

		e.MarkDirty("aaa")

		go func() { aDone <- e.Flush(aCtx) }()

		<-first.entered

		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		e.MarkDirty("bbb")

		go func() { bDone <- e.Flush(bCtx) }()

		<-second.entered

		// The first began at 0, the second at 20 ms, and the clock is at 20 ms.
		steps := []struct {
			sleep time.Duration
			armed int
			timed bool
			what  string
		}{
			{0, 0, true, "at 20 ms, neither has lasted 50 ms"},
			{30*time.Millisecond - time.Nanosecond, 0, true, "1 ns before 50 ms"},
			{time.Nanosecond, 1, true, "at 50 ms the first is armed, and the timer is set for the second"},
			{20*time.Millisecond - time.Nanosecond, 1, true, "1 ns before 70 ms"},
			{time.Nanosecond, 2, false, "at 70 ms the second is armed, and nothing is left to look at"},
		}

		for _, step := range steps {
			time.Sleep(step.sleep)
			synctest.Wait()

			runs, armed, timed := watchState(e)
			assert.Equal(t, 2, runs, step.what)
			assert.Equal(t, step.armed, armed, step.what)
			assert.Equal(t, step.timed, timed, step.what)
		}

		bCancel(context.DeadlineExceeded)
		synctest.Wait()

		assert.FileExists(t, markerOf(dir, "bbb"), "the one armed last ended with a deadline's cause")
		assert.FileExists(t, markerOf(dir, "aaa"), "and so does what the other holds, which is stuck")
		assert.FileExists(t, markerOf(dir, "ttt"), "and what the flush by the timer holds")

		// What the flushes end with is no concern of this test: one that ended with a
		// deadline's cause returns it.
		close(secondGate)
		t.Log("the second flush ended with:", <-bDone)
		aCancel(context.Canceled)
		close(firstGate)
		t.Log("the first flush ended with:", <-aDone)
		close(timerGate)

		require.NoError(t, e.Stop(t.Context()))

		runs, armed, isSet := watchState(e)
		assert.Zero(t, runs)
		assert.Zero(t, armed)
		assert.False(t, isSet)
	})
}

// Test_Engine_FlushInTimeIsNotWatchedLongerThanItRuns checks that a flush with a
// ctx that can end, and no deadline, is registered with the slow watch for as
// long as it runs and no longer, and that the timer is stopped once none is.
func Test_Engine_FlushInTimeIsNotWatchedLongerThanItRuns(t *testing.T) {
	e := New(quietConfig(t.TempDir(), time.Hour))
	e.Register("svc", &stateSnap{state: "x"})

	e.MarkDirty("svc")
	require.NoError(t, e.Flush(t.Context()))

	e.slow.mu.Lock()
	defer e.slow.mu.Unlock()

	assert.Empty(t, e.slow.runs)
	assert.False(t, e.slow.timed, "nothing to look at")
	assert.NoFileExists(t, markerOf(e.dir, "svc"))
}

// lingerSnap is a Snapshot that takes d.
type lingerSnap struct{ d time.Duration }

func (s lingerSnap) Snapshot(w io.Writer) error {
	time.Sleep(s.d)

	_, err := io.WriteString(w, "x")

	return err //nolint:wrapcheck // a test double
}

func (lingerSnap) Restore(io.Reader) error { return nil }

// Test_Engine_SlowWatchHoldsUnderConcurrentFlushes checks that flushes that end
// at once, some with a deadline's cause while the watch looks at them, leave
// nothing registered and the timer unset, and that none of them hangs.
func Test_Engine_SlowWatchHoldsUnderConcurrentFlushes(t *testing.T) {
	e := New(quietConfig(t.TempDir(), time.Hour))
	e.watchAfter = time.Millisecond

	const callers = 4

	var wg sync.WaitGroup

	for i := range callers {
		name := "svc" + strconv.Itoa(i)
		e.Register(name, lingerSnap{d: 2 * time.Millisecond})

		wg.Go(func() {
			for j := range 15 {
				ctx, cancel := context.WithCancelCause(t.Context())

				e.MarkDirty(name)

				if j%2 == 0 {
					time.AfterFunc(time.Millisecond, func() { cancel(context.DeadlineExceeded) })
				}

				_ = e.Flush(ctx)

				cancel(nil)
			}
		})
	}

	wg.Wait()

	e.slow.mu.Lock()
	defer e.slow.mu.Unlock()

	assert.Empty(t, e.slow.runs)
	assert.False(t, e.slow.timed)
}

// Test_Engine_StopMarksTheServiceAFlushHoldsWhateverItsContext checks that a Stop
// whose deadline ends while a Flush is stuck in a Snapshot marks the service
// that Flush holds, besides the one that is dirty, whatever ctx that Flush has:
// none, one that can end, one that ends with a deadline's cause, and one with a
// deadline that is later than the Stop's, which comes after the kill. Nothing
// else would mark it, and the process is on its way out. A request's save (POST
// /_gostack/state/save) is such a Flush.
func Test_Engine_StopMarksTheServiceAFlushHoldsWhateverItsContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  func(parent context.Context) (context.Context, context.CancelFunc)
	}{
		{"no deadline, cannot end", func(context.Context) (context.Context, context.CancelFunc) {
			return context.Background(), func() {}
		}},
		{"no deadline, can end", context.WithCancel},
		{"no deadline, ends with a cause", func(parent context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancelCause(parent)

			return ctx, func() { cancel(nil) }
		}},
		{"a deadline 300 ms away, later than the Stop's", func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, 300*time.Millisecond)
		}},
		{"a deadline an hour away", func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				e := New(quietConfig(dir, time.Hour))
				snap, gate := gatedSnap("x")
				e.Register("aaa", snap)
				e.Register("bbb", &stateSnap{state: "b"})
				e.MarkDirty("aaa")

				held, release := tt.ctx(t.Context())
				defer release()

				done := make(chan error, 1)

				go func() { done <- e.Flush(held) }()

				<-snap.entered
				e.MarkDirty("bbb")

				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()

				require.Error(t, e.Stop(ctx))

				assert.FileExists(t, markerOf(dir, "aaa"), "held by the Flush that is stuck")
				assert.FileExists(t, markerOf(dir, "bbb"), "dirty")

				close(gate)
				require.NoError(t, <-done)

				assert.NoFileExists(t, markerOf(dir, "aaa"), "the save that completed removed it")
			})
		})
	}
}

// renamedState is the state the save of a committingSnap puts on disk.
const renamedState = "renamed"

// committingSnap is a Snapshotter and a [Committer] whose Committed waits: its
// save is on disk, and the flush that made it still holds the service.
type committingSnap struct {
	*stateSnap

	in   chan struct{}
	gate chan struct{}
}

func (c *committingSnap) Committed() {
	c.in <- struct{}{}
	<-c.gate
}

// Test_Engine_DeadlineMarksNothingForAServiceWhoseSaveHasRenamed checks that a
// deadline that comes after a save renamed its snapshot into place, and before
// the flush that made it lets go of the service, marks no service: its state is
// on disk, so a marker would be a warning at the next start for a loss there is
// not. It is the deadline of a Stop that waits behind the flush, and the Stop's
// own, which marks what it holds.
func Test_Engine_DeadlineMarksNothingForAServiceWhoseSaveHasRenamed(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run("own="+strconv.FormatBool(own), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				e := New(quietConfig(dir, time.Hour))
				c := &committingSnap{
					stateSnap: &stateSnap{state: renamedState}, in: make(chan struct{}, 1), gate: make(chan struct{}),
				}
				e.Register("aaa", c)
				e.MarkDirty("aaa")

				done := make(chan error, 1)
				stopCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()

				if own {
					go func() { done <- e.Stop(stopCtx) }()

					<-c.in // the save is on disk, and the Stop holds aaa

					<-stopCtx.Done()
					synctest.Wait()
				} else {
					go func() { done <- e.Flush(context.Background()) }()

					<-c.in

					require.Error(t, e.Stop(stopCtx), "the Stop gave up behind the flush")
				}

				got, err := os.ReadFile(filepath.Join(dir, "aaa", FormatJSON.fileName()))
				require.NoError(t, err)
				assert.Equal(t, renamedState, string(got), "the save is on disk")
				assert.NoFileExists(t, markerOf(dir, "aaa"), "so there is no loss to mark")

				close(c.gate)
				t.Log("the holder ended with:", <-done)

				assert.Empty(t, startAbandonWarnings(t, dir, "aaa"), "and the next start says nothing")
			})
		})
	}
}

// Test_Engine_FlushRefusesAStateFileLinkedToTheMarker checks that a state file
// that is a link to the name of the marker is refused, with an error, and the
// file behind it is left as it was, where the save used to rename its snapshot
// onto it and then remove it as the marker. The refusal is by the base name of
// where the link leads: in another directory, and in another case, as well.
func Test_Engine_FlushRefusesAStateFileLinkedToTheMarker(t *testing.T) {
	tests := []struct {
		name string
		link string
	}{
		{"the marker's name", abandonedFile},
		{"the same, from here", "./" + abandonedFile},
		{"the name in other case", strings.ToUpper(abandonedFile)},
		{"the name in a sub directory", "sub/" + abandonedFile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			e := New(quietConfig(dir, time.Hour))
			e.Register("svc", &stateSnap{state: "new"})
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "svc", "sub"), 0o750))

			err := os.Symlink(tt.link, filepath.Join(dir, "svc", FormatJSON.fileName()))
			if err != nil {
				t.Skipf("no links here: %v", err)
			}

			require.NoError(t, os.WriteFile(filepath.Join(dir, "svc", tt.link), []byte("precious"), 0o600))

			e.MarkDirty("svc")

			err = e.Flush(t.Context())
			require.ErrorIs(t, err, errStateIsMarker)

			got, readErr := os.ReadFile(filepath.Join(dir, "svc", FormatJSON.fileName()))
			require.NoError(t, readErr)
			assert.Equal(t, "precious", string(got), "what the link leads to is left alone")
		})
	}
}

// allocRuns is how many single Flushes flushAllocs measures.
const allocRuns = 60

// flushAllocs returns the allocations of a change to one service and the Flush
// of it with ctx, that being in time: the least of allocRuns single runs. What
// another goroutine of the process allocates in the meantime only adds to a run,
// and so does an allocation the race detector's runtime makes now and then
// (about a Flush in four of eight, on Linux), so the least run is the Flush's
// own, which is the same each time. An average of runs would carry the extra.
func flushAllocs(ctx context.Context, t *testing.T) float64 {
	t.Helper()

	e := New(quietConfig(t.TempDir(), time.Hour))
	// A Flush that a loaded machine drags past the watch's bound is not in time.
	e.watchAfter = time.Hour
	e.Register("svc", &stateSnap{state: "x"})

	least := math.Inf(1)

	for range allocRuns {
		least = min(least, testing.AllocsPerRun(1, func() {
			e.MarkDirty("svc") //nolint:contextcheck // arms a timer, whose callback has no caller context

			err := e.Flush(ctx)
			if err != nil {
				t.Error(err)
			}
		}))
	}

	return least
}

// allocChild is set in the environment of the process that measures the
// allocations of a Flush in time.
const allocChild = "GOSTACK_PERSIST_FLUSH_ALLOCS"

// deadlineAllocs is what a deadline adds to a Flush in time: the allowance,
// [context.AfterFunc], and what that registers, in one flush.
const deadlineAllocs = 9

// Test_Engine_FlushInTimeAllocations checks the allocations of a whole Flush in
// time, so that what the abandoned-save marker cost one (SQ-461) cannot return
// unseen: the allowance, the map of the services lost, and the lstat of the
// marker each added to it, and only the benchmark gate read them. The figure is
// what a change and a Flush of one service allocate with a ctx that can end and
// has no deadline, which is the ctx of a request and of a benchmark; the same
// with a deadline is that and deadlineAllocs more.
//
// The figures are those of a build with the race detector, which `make test`
// always runs with; a plain build allocates one less on Linux, so it is held to
// the figure as a ceiling and not as a floor. Under the race detector a Flush
// that allocates less than the figure fails as well as one that allocates more:
// a Go release that takes allocations out must update the figure, or the slack
// it leaves would hide the next one put in.
//
// The count is of the whole process, so the test runs alone in a process of its
// own: the engines other tests of the package leave flushing on their timers
// added seven allocations to each Flush when it ran with them. The child takes
// the least of many single Flushes, which a regression raises in every one, and
// is run once.
func Test_Engine_FlushInTimeAllocations(t *testing.T) {
	figures := map[string]float64{"darwin": 108, "linux": 105}

	figure, ok := figures[runtime.GOOS]
	if !ok {
		t.Skipf("no figure for %s", runtime.GOOS)
	}

	if os.Getenv(allocChild) == "" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^Test_Engine_FlushInTimeAllocations$", "-test.v")
		cmd.Env = append(os.Environ(), allocChild+"=1")

		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		require.NoError(t, err, "alone, the Flush in time allocates what its figure says")

		return
	}

	plain := flushAllocs(t.Context(), t)
	background := flushAllocs(context.Background(), t)
	timed, cancel := context.WithTimeout(t.Context(), time.Hour)

	defer cancel()

	withDeadline := flushAllocs(timed, t)
	t.Logf("allocations: %v with a ctx that can end, %v with one that cannot, %v with a deadline (race build: %v)",
		plain, background, withDeadline, raceBuild)

	assert.LessOrEqual(t, plain, figure, "allocations of a Flush in time, no deadline")
	assert.LessOrEqual(t, background, figure, "a ctx that cannot end costs no more")
	assert.LessOrEqual(t, withDeadline, figure+deadlineAllocs, "a deadline arms its marking")

	if raceBuild {
		assert.InDelta(t, figure, plain, 0, "fewer than the figure: update it, and the docs that name it")
		assert.InDelta(t, figure, background, 0, "fewer than the figure: update it")
		assert.InDelta(t, figure+deadlineAllocs, withDeadline, 0, "fewer than the figure: update it")
	}
}

// Test_Engine_SaveClearsAMarkerOfAnotherCase checks that a completed save removes
// a marker whose name differs in case only on a volume that folds case, as it
// does the exact name: left, it would warn at every start. A volume that keeps
// case apart holds it as another file, which the save leaves.
func Test_Engine_SaveClearsAMarkerOfAnotherCase(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "CaseProbe")
	require.NoError(t, os.WriteFile(probe, nil, 0o600))

	_, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	if err != nil {
		t.Skip("this volume keeps case apart")
	}

	e := New(quietConfig(dir, time.Hour))
	e.Register("svc", &stateSnap{state: "x"})
	require.NoError(t, os.Mkdir(filepath.Join(dir, "svc"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc", strings.ToUpper(abandonedFile)), nil, 0o600))

	e.MarkDirty("svc")
	require.NoError(t, e.Flush(t.Context()))

	assert.NoFileExists(t, markerOf(dir, "svc"), "the save that completed removed the marker")
}
