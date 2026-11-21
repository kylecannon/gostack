package persist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowCounter is a Snapshotter whose state is a counter and whose Snapshot takes
// a while, as a big store's does.
type slowCounter struct {
	n     atomic.Int64
	calls atomic.Int64
	delay time.Duration
}

func (s *slowCounter) Snapshot(w io.Writer) error {
	s.calls.Add(1)
	n := s.n.Load()

	time.Sleep(s.delay)

	_, err := fmt.Fprint(w, n)

	return err //nolint:wrapcheck // a test double
}

func (*slowCounter) Restore(io.Reader) error { return nil }

func (e *Engine) idle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.timer == nil && !e.running && e.flushing == 0
}

// Test_Engine_Immediate_OneFlusher runs in real time, not under synctest. The
// defect it guards against parks timer goroutines on a lock, and a goroutine
// waiting on a sync.Mutex stops synctest's clock, so it would hang there
// instead of failing.
func Test_Engine_Immediate_OneFlusher(t *testing.T) {
	const (
		writers  = 8
		duration = 3 * time.Second
	)

	dir := t.TempDir()
	s := &slowCounter{delay: 20 * time.Millisecond}
	e := New(Config{
		PersistVarSet: true, BaseDir: dir, Frequency: Immediate, Logger: slog.New(slog.DiscardHandler),
	})
	e.Register("sqs", s)
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	start, stop := make(chan struct{}), make(chan struct{})

	var writing sync.WaitGroup

	for range writers {
		writing.Go(func() {
			<-start

			for {
				select {
				case <-stop:
					return
				default:
				}

				s.n.Add(1)
				e.MarkDirty("sqs")
				time.Sleep(100 * time.Microsecond)
			}
		})
	}

	// The writers are counted in the baseline; what may come on top is the one
	// flush goroutine and a timer callback that finds itself disarmed.
	baseline := runtime.NumGoroutine()
	peak := baseline

	close(start)

	for deadline := time.Now().Add(duration); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		peak = max(peak, runtime.NumGoroutine())
		if peak > baseline+2 {
			break // a pile is forming; stop before it holds thousands of goroutines
		}
	}

	close(stop)
	writing.Wait()

	require.LessOrEqual(t, peak, baseline+2, "one flush goroutine at a time (%d snapshots for %d changes)",
		s.calls.Load(), s.n.Load())
	assert.Greater(t, s.calls.Load(), int64(10), "the changes were written as they came")

	want := strconv.FormatInt(s.n.Load(), 10)
	file := filepath.Join(dir, "sqs", "store.json")

	require.Eventually(t, func() bool {
		saved, err := os.ReadFile(file)

		return err == nil && string(saved) == want && e.idle()
	}, 5*time.Second, 10*time.Millisecond, "the last change reaches the disk and the engine goes idle")

	// Nothing is dirty, so nothing wakes up.
	calls := s.calls.Load()

	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, calls, s.calls.Load(), "no flush while nothing is dirty")
	assert.True(t, e.idle(), "no timer is armed while nothing is dirty")
	// Another test's engine may fire a timer meanwhile, so a goroutine that ends
	// soon is no leak. The loop polls by hand: assert.Eventually would count its
	// own goroutine.
	left := runtime.NumGoroutine()
	for deadline := time.Now().Add(time.Second); left > baseline-writers && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)

		left = runtime.NumGoroutine()
	}

	assert.LessOrEqual(t, left, baseline-writers, "no goroutine is left behind")
}

// Test_Engine_TimedFlush_GaveWayServiceIsNotCountedAsFailed checks that one
// flush judges each service on its own: a failed for real, and b, abandoned
// when a caller's flush cancelled the timed one, gave way. b is not counted as
// failed because a failed, and has no backoff. b follows a, so the caller's
// flush cannot save it beside the timed one, which has begun a's save.
func Test_Engine_TimedFlush_GaveWayServiceIsNotCountedAsFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Second))
		errFull := errors.New("no space left on device")
		gate := make(chan struct{})

		a := &stateSnap{
			state: "a", entered: make(chan struct{}, 1), gate: gate,
			during: func() error { return errFull },
		}
		e.Register("a", a)
		e.Register("b", followingSnap{stateSnap: &stateSnap{state: "b"}, leader: "a"})
		e.MarkDirty("a")
		e.MarkDirty("b")

		<-a.entered // the timed flush, at 10 s, is inside a's Snapshot

		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)

		go func() { finished <- e.Flush(ctx) }()

		synctest.Wait()
		cancel() // the caller goes away, and the timed flush is cancelled
		require.Error(t, <-finished)
		close(gate) // a fails for real, and b is abandoned
		synctest.Wait()

		now := time.Now()

		e.mu.Lock()
		ta, tb := e.services["a"], e.services["b"]
		assert.Equal(t, 1, ta.failures, "a failed for real")
		assert.Equal(t, now.Add(10*time.Second), ta.retryAt, "a waits out its own backoff")
		assert.True(t, ta.dirty)
		assert.Zero(t, tb.failures, "b gave way and is not counted as failed")
		assert.True(t, tb.retryAt.IsZero(), "b has no backoff")
		assert.True(t, tb.dirty)
		e.mu.Unlock()

		// Both are saved PERSIST_FREQUENCY on.
		time.Sleep(10*time.Second + time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "a", readState(t, filepath.Join(dir, "a", "store.json")))
		assert.Equal(t, "b", readState(t, filepath.Join(dir, "b", "store.json")))
		require.NoError(t, e.Close(t.Context()))
	})
}
