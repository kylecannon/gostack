package persist

import (
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

	return e.timer == nil && !e.running && !e.flushing
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
