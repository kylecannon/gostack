package persist

import (
	"context"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here are the follow-ups of PR #56's review (SQ-465): a ctx with no
// deadline that ends with a deadline's cause, a marker whose name differs in
// case, a Stop behind a Flush that has no deadline, and a state file linked to
// the marker's name.

// markerOf returns where the marker of service is in dir.
func markerOf(dir, service string) string {
	return filepath.Join(dir, service, abandonedFile)
}

// Test_Engine_FlushMarksAContextWithNoDeadlineThatEndsWithADeadlineCause checks
// that a Flush whose ctx has no deadline, and that ends with the cause of one,
// marks the service it holds and the one changed since while the Snapshot it is
// inside has not returned, the same as a ctx that ends at a deadline: the kill
// the deadline stands for would come then. A ctx that ends with any other cause
// marks nothing until the flush ends, which is not a deadline. The watch begins
// once the flush has run for watchAfter, so the ctx ends before it, and after.
func Test_Engine_FlushMarksAContextWithNoDeadlineThatEndsWithADeadlineCause(t *testing.T) {
	const watchAfter = 5 * time.Millisecond

	tests := []struct {
		name   string
		cause  error
		after  time.Duration
		marked bool
	}{
		{"deadline cause before the watch begins", context.DeadlineExceeded, 0, true},
		{"deadline cause after the watch began", context.DeadlineExceeded, 10 * watchAfter, true},
		{"another cause", context.Canceled, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			e := New(quietConfig(dir, time.Hour))
			e.watchAfter = watchAfter
			gate := make(chan struct{})
			snap := &stateSnap{state: "x", gate: gate, entered: make(chan struct{}, 1)}
			e.Register("svc", snap)
			e.Register("other", &stateSnap{state: "o"})
			e.MarkDirty("svc")

			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)

			done := make(chan error, 1)

			go func() { done <- e.Flush(ctx) }()

			<-snap.entered
			// A change to a service the flush does not hold.
			e.MarkDirty("other")
			time.Sleep(tt.after)
			cancel(tt.cause)

			if tt.marked {
				assert.Eventually(t, func() bool {
					_, held := os.Lstat(markerOf(dir, "svc"))
					_, dirty := os.Lstat(markerOf(dir, "other"))

					return held == nil && dirty == nil
				}, 5*time.Second, time.Millisecond, "the markers came with the Snapshot still not returned")
			} else {
				time.Sleep(10 * watchAfter)
				assert.NoFileExists(t, markerOf(dir, "svc"), "no deadline ended it")
			}

			// The Snapshot returns, and its save completes: what it saved needs no
			// marker, and the service changed since is still unsaved.
			close(gate)
			require.NoError(t, <-done)

			if tt.marked {
				assert.NoFileExists(t, markerOf(dir, "svc"), "its save completed")
				assert.FileExists(t, markerOf(dir, "other"), "changed since, and unsaved")
			}

			e.slow.mu.Lock()
			defer e.slow.mu.Unlock()

			assert.Empty(t, e.slow.runs, "the flush that ended is no longer watched")
		})
	}
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

// Test_Engine_StopMarksTheServiceAFlushWithNoDeadlineHolds checks that a Stop
// whose deadline ends while a Flush with none is stuck in a Snapshot, as a
// request's save is, marks the service that Flush holds, not only the one that
// is dirty: nothing else would, and the process is on its way out.
func Test_Engine_StopMarksTheServiceAFlushWithNoDeadlineHolds(t *testing.T) {
	dir := t.TempDir()
	e := New(quietConfig(dir, time.Hour))
	gate := make(chan struct{})
	snap := &stateSnap{state: "x", gate: gate, entered: make(chan struct{}, 1)}
	e.Register("aaa", snap)
	e.Register("bbb", &stateSnap{state: "b"})
	e.MarkDirty("aaa")

	done := make(chan error, 1)

	go func() { done <- e.Flush(context.Background()) }()

	<-snap.entered
	e.MarkDirty("bbb")

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	require.Error(t, e.Stop(ctx))

	assert.FileExists(t, markerOf(dir, "aaa"), "held by the Flush that has no deadline")
	assert.FileExists(t, markerOf(dir, "bbb"), "dirty")

	close(gate)
	require.NoError(t, <-done)

	assert.NoFileExists(t, markerOf(dir, "aaa"), "the save that completed removed it")
}

// Test_Engine_FlushRefusesAStateFileLinkedToTheMarker checks that a state file
// that is a link to the name of the marker is refused, with an error, and the
// file behind it is left as it was, where the save used to rename its snapshot
// onto it and then remove it as the marker.
func Test_Engine_FlushRefusesAStateFileLinkedToTheMarker(t *testing.T) {
	tests := []struct {
		name string
		link string
	}{
		{"the marker's name", abandonedFile},
		{"the same, from here", "./" + abandonedFile},
		{"the name in other case", strings.ToUpper(abandonedFile)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			e := New(quietConfig(dir, time.Hour))
			e.Register("svc", &stateSnap{state: "new"})
			require.NoError(t, os.Mkdir(filepath.Join(dir, "svc"), 0o750))

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

// flushAllocs returns the allocations of a change to one service and the Flush
// of it with ctx, that being in time. It is the least of five runs of five: what
// another goroutine of the process allocates in the meantime only adds to a run,
// and the same code allocates the same each time.
func flushAllocs(ctx context.Context, t *testing.T) float64 {
	t.Helper()

	e := New(quietConfig(t.TempDir(), time.Hour))
	e.Register("svc", &stateSnap{state: "x"})

	least := math.Inf(1)

	for range 5 {
		least = min(least, testing.AllocsPerRun(5, func() {
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

// Test_Engine_FlushInTimeAllocations checks the allocations of a whole Flush in
// time, so that what the abandoned-save marker cost one (SQ-461) cannot return
// unseen: the allowance, the map of the services lost, and the lstat of the
// marker each added to it, and only the benchmark gate read them. The ceiling is
// what a change and a Flush of one service allocate with a ctx that can end and
// has no deadline, which is the ctx of a request and of a benchmark; the same
// with a deadline is that and what arming the marking at it takes, which is
// nine more (the allowance, [context.AfterFunc], and what that registers).
//
// The count is of the whole process, so the test runs alone in a process of its
// own: the engines other tests of the package leave flushing on their timers
// added seven allocations to each Flush when it ran with them.
func Test_Engine_FlushInTimeAllocations(t *testing.T) {
	ceilings := map[string]float64{"darwin": 108, "linux": 105}

	ceiling, ok := ceilings[runtime.GOOS]
	if !ok {
		t.Skipf("no figure for %s", runtime.GOOS)
	}

	if os.Getenv(allocChild) == "" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^Test_Engine_FlushInTimeAllocations$", "-test.v")
		cmd.Env = append(os.Environ(), allocChild+"=1")

		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		require.NoError(t, err, "alone, the Flush in time allocates no more than its ceiling")

		return
	}

	plain := flushAllocs(t.Context(), t)
	background := flushAllocs(context.Background(), t)
	t.Logf("allocations: %v with a ctx that can end, %v with one that cannot", plain, background)

	assert.LessOrEqual(t, plain, ceiling, "allocations of a Flush in time, no deadline")
	assert.LessOrEqual(t, background, ceiling, "a ctx that cannot end costs no more")

	timed, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()

	withDeadline := flushAllocs(timed, t)
	t.Logf("allocations: %v with a deadline", withDeadline)

	assert.LessOrEqual(t, withDeadline, ceiling+10, "a deadline arms its marking, nine allocations and one of slack")
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
