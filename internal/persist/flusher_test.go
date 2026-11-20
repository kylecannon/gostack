package persist_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxRetry is the longest wait between failed flushes when PERSIST_FREQUENCY is
// shorter.
const maxRetry = 30 * time.Second

// sns is a second service, next to sqs.
const sns = "sns"

// errDisk is a second failure, with an error of its own.
var errDisk = errors.New("disk full")

// attempts is a Snapshotter that records when each Snapshot started, takes
// took to run, and fails with err until it is healed.
type attempts struct {
	mu    sync.Mutex
	at    []time.Time
	state string
	err   error
	took  time.Duration
}

func (a *attempts) Snapshot(w io.Writer) error {
	a.mu.Lock()
	a.at = append(a.at, time.Now())
	err, state, took := a.err, a.state, a.took
	a.mu.Unlock()

	time.Sleep(took)

	if err != nil {
		return err
	}

	_, err = io.WriteString(w, state)

	return err //nolint:wrapcheck // a test double
}

func (*attempts) Restore(io.Reader) error { return nil }

func (a *attempts) set(state string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = state
}

func (a *attempts) failWith(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.err = err
}

func (a *attempts) heal() { a.failWith(nil) }

// gaps returns the time between each attempt and the one before.
func (a *attempts) gaps() []time.Duration {
	at := a.times()
	gaps := make([]time.Duration, 0, len(at))

	for i := 1; i < len(at); i++ {
		gaps = append(gaps, at[i].Sub(at[i-1]))
	}

	return gaps
}

func (a *attempts) times() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]time.Time(nil), a.at...)
}

func Test_Engine_Immediate_FailingFlushBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := t.TempDir()
		cfg := baseCfg(base)
		cfg.Frequency = persist.Immediate

		e := persist.New(cfg)
		s := &attempts{err: errSnapshot, took: 5 * time.Millisecond}
		e.Register(sqs, s)

		// A change every millisecond for three seconds, against a disk that fails.
		for i := range 3000 {
			s.set("state:" + strconv.Itoa(i))
			e.MarkDirty(sqs)
			time.Sleep(time.Millisecond)
		}

		// The first attempt is at once, the second 1 s after it failed, and the third
		// is due 2 s after that, just after the changes stop.
		assert.Len(t, s.times(), 2, "two attempts for three thousand changes")

		// The changes stop. The state is still unsaved, so the retries go on, with
		// no backlog of queued flushes: the next waits 4 s, the one after 8 s.
		time.Sleep(13 * time.Second)
		synctest.Wait()

		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
		gaps := s.gaps()
		require.Len(t, gaps, len(want))

		for i, gap := range gaps {
			// The wait starts when the failed flush ends, took after it starts.
			assert.Equal(t, want[i]+s.took, gap, "wait %d", i)
		}

		// The disk recovers: the next retry saves the last change, and the retries stop.
		s.heal()
		time.Sleep(16 * time.Second)
		synctest.Wait()
		assert.Equal(t, "state:2999", readFile(t, filepath.Join(base, sqs, jsonFile)))

		saved := len(s.times())

		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Len(t, s.times(), saved, "no retry once the state is saved")
	})
}

func Test_Engine_FailedFlushBackoff(t *testing.T) {
	tests := []struct {
		name          string
		giveFrequency time.Duration
		// wantFirst is when the first attempt comes, after one change at zero.
		wantFirst time.Duration
		wantGaps  []time.Duration
	}{
		{
			name:          "immediate: from 1 s, doubling, up to 30 s",
			giveFrequency: persist.Immediate,
			wantGaps: []time.Duration{
				time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
				30 * time.Second, 30 * time.Second, 30 * time.Second,
			},
		},
		{
			name:          "a frequency under 30 s starts the backoff",
			giveFrequency: 10 * time.Second,
			wantFirst:     10 * time.Second,
			wantGaps:      []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second},
		},
		{
			name:          "a frequency over 30 s is also the cap",
			giveFrequency: time.Minute,
			wantFirst:     time.Minute,
			wantGaps:      []time.Duration{time.Minute, time.Minute, time.Minute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := baseCfg(t.TempDir())
				cfg.Frequency = tt.giveFrequency

				e := persist.New(cfg)
				s := &attempts{err: errSnapshot}
				e.Register(sqs, s)

				began := time.Now()
				e.MarkDirty(sqs)

				// A change every 100 ms does not bring a pending retry forward.
				stop := make(chan struct{})

				go func() {
					for {
						select {
						case <-stop:
							return
						case <-time.After(100 * time.Millisecond):
							e.MarkDirty(sqs)
						}
					}
				}()

				var total time.Duration
				for _, gap := range tt.wantGaps {
					total += gap
				}

				// Up to half a second past the last attempt, well short of the next.
				time.Sleep(tt.wantFirst + total + 500*time.Millisecond)
				close(stop)
				synctest.Wait()

				require.NotEmpty(t, s.times())
				assert.Equal(t, tt.wantFirst, s.times()[0].Sub(began))
				assert.Equal(t, tt.wantGaps, s.gaps())

				_ = e.Close(t.Context())
			})
		})
	}
}

func Test_Engine_FailedFlushBackoffResetsAfterASave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := t.TempDir()
		cfg := baseCfg(base)
		cfg.Frequency = persist.Immediate

		e := persist.New(cfg)
		s := &attempts{err: errSnapshot, state: stateA}
		e.Register(sqs, s)

		began := time.Now()
		since := func() []time.Duration {
			times := s.times()
			out := make([]time.Duration, 0, len(times))

			for _, at := range times {
				out = append(out, at.Sub(began))
			}

			return out
		}

		// Fails at 0, 1 s and 3 s; the disk heals before the retry at 7 s.
		e.MarkDirty(sqs)
		time.Sleep(3500 * time.Millisecond)
		s.heal()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		assert.Equal(t, stateA, readFile(t, filepath.Join(base, sqs, jsonFile)))

		// At 8 s the disk fails again. A change is written at once, as before the
		// failures, and the retries start over from 1 s.
		time.Sleep(500 * time.Millisecond)
		s.failWith(errSnapshot)
		s.set(stateB)
		e.MarkDirty(sqs)
		time.Sleep(3500 * time.Millisecond)
		synctest.Wait()

		sec := time.Second
		assert.Equal(t, []time.Duration{0, sec, 3 * sec, 7 * sec, 8 * sec, 9 * sec, 11 * sec}, since())

		_ = e.Close(t.Context())
	})
}

// Test_Engine_FailedFlushBackoffIsPerService checks that a service whose save
// keeps failing backs off alone: another service's change is saved within
// PERSIST_FREQUENCY, whatever wait the failing one has reached, and the
// failing one is still tried at its own pace and at shutdown.
func Test_Engine_FailedFlushBackoffIsPerService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := t.TempDir()
		cfg := baseCfg(base)
		cfg.Frequency = 10 * time.Second

		e := persist.New(cfg)
		bad := &attempts{err: errSnapshot}
		good := &attempts{state: stateA}
		e.Register(sqs, bad)
		e.Register(sns, good)

		began := time.Now()
		sec := time.Second

		// sqs fails at 10 s, 20 s and 40 s. Its next retry is due at 70 s.
		e.MarkDirty(sqs)
		time.Sleep(45 * sec)
		synctest.Wait()
		require.Len(t, bad.times(), 3)

		// sns changes at 45 s: it is saved by 55 s, not at 70 s with sqs's retry.
		good.set(stateB)
		e.MarkDirty(sns)
		time.Sleep(10*sec + 500*time.Millisecond)
		synctest.Wait()

		assert.Equal(t, stateB, readFile(t, filepath.Join(base, sns, jsonFile)),
			"sns is saved within PERSIST_FREQUENCY of its change")
		require.Len(t, good.times(), 1)
		assert.Equal(t, 55*sec, good.times()[0].Sub(began))
		assert.Len(t, bad.times(), 3, "sqs is not tried again before its own retry")

		// sqs keeps its own pace: 10 s, 20 s, then 30 s.
		time.Sleep(15 * sec)
		synctest.Wait()
		assert.Equal(t, []time.Duration{10 * sec, 20 * sec, 30 * sec}, bad.gaps())

		// A failing service is still tried at shutdown, and its failure is reported.
		require.ErrorIs(t, e.Close(t.Context()), errSnapshot)
		assert.Len(t, bad.times(), 5, "the retry at 70 s and the shutdown's")
	})
}

func Test_Engine_FailedFlushLogging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer

		cfg := loggedCfg(t.TempDir(), &logs)
		cfg.Frequency = persist.Immediate

		e := persist.New(cfg)
		s := &attempts{err: errSnapshot, state: stateA}
		e.Register(sqs, s)

		// waitFor lets the retries run until n attempts have failed. The retries
		// are at least a second apart, so nothing else happens in between.
		waitFor := func(n int) {
			for len(s.times()) < n {
				time.Sleep(time.Second)
				synctest.Wait()
			}
		}

		e.MarkDirty(sqs)
		waitFor(24)
		s.failWith(errDisk)
		waitFor(50)
		s.heal()
		time.Sleep(maxRetry)
		synctest.Wait()
		require.Len(t, s.times(), 51, "50 failures and the save that ends them")

		var errorLines, recovered int

		for line := range strings.Lines(logs.String()) {
			switch {
			case strings.Contains(line, "level=ERROR"):
				errorLines++
			case strings.Contains(line, "save recovered after 50 failures"):
				recovered++
			default:
			}
		}

		// The first failure, the 25th whose error changed, and the 10th, 20th, 30th,
		// 40th and 50th.
		assert.Equal(t, 7, errorLines, "%s", logs.String())
		assert.Equal(t, 1, recovered, "%s", logs.String())
		assert.Contains(t, logs.String(), "failures=50")
		assert.Contains(t, logs.String(), "service=sqs", "each line names the service whose save failed")
	})
}

// stalledLoad is a Snapshotter whose Restore takes two seconds unless it is
// released, as a big load does.
type stalledLoad struct {
	fake

	started chan struct{}
	release chan struct{}
}

func (s *stalledLoad) Restore(r io.Reader) error {
	close(s.started)

	select {
	case <-s.release:
	case <-time.After(2 * time.Second):
	}

	return s.fake.Restore(r)
}

// Test_Engine_WaitingForALoadHonorsTheDeadline runs in real time: on a sync.Mutex
// the wait would stop synctest's clock.
func Test_Engine_WaitingForALoadHonorsTheDeadline(t *testing.T) {
	tests := []struct {
		name string
		call func(*persist.Engine, context.Context) error
	}{
		{name: "Flush", call: (*persist.Engine).Flush},
		{name: "Close", call: (*persist.Engine).Close},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(base, sqs), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(base, sqs, jsonFile), []byte(stateA), 0o600))

			e := persist.New(baseCfg(base))
			s := &stalledLoad{started: make(chan struct{}), release: make(chan struct{})}
			e.Register(sqs, s)

			loaded := make(chan error, 1)

			go func() { loaded <- e.Load(t.Context()) }()

			<-s.started

			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()

			began := time.Now()
			err := tt.call(e, ctx)
			waited := time.Since(began)

			close(s.release)
			require.NoError(t, <-loaded)
			assert.Equal(t, stateA, s.get())

			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Less(t, waited, 300*time.Millisecond)
		})
	}
}

// peeking is a Snapshotter that looks into the service's directory halfway
// through its write, to see where the bytes go.
type peeking struct {
	dir      string
	state    string
	calls    int
	sizes    map[string]int64
	failWith error
}

func (p *peeking) Snapshot(w io.Writer) error {
	p.calls++

	half := len(p.state) / 2

	_, err := io.WriteString(w, p.state[:half])
	if err != nil {
		return err //nolint:wrapcheck // a test double
	}

	p.sizes = map[string]int64{}

	entries, _ := os.ReadDir(p.dir)
	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr == nil {
			p.sizes[entry.Name()] = info.Size()
		}
	}

	if p.failWith != nil {
		return p.failWith
	}

	_, err = io.WriteString(w, p.state[half:])

	return err //nolint:wrapcheck // a test double
}

func (*peeking) Restore(io.Reader) error { return nil }

func Test_Engine_Flush_StreamsIntoTheTempFiles(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)

	cfg := baseCfg(base)
	cfg.Formats = []persist.Format{persist.FormatJSON, persist.FormatBinary}

	e := persist.New(cfg)
	s := &peeking{dir: dir, state: "state:" + strings.Repeat("0123456789abcdef", 1<<16)}
	e.Register(sqs, s)
	// The failed flush below re-arms the timer; Close stops it.
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	e.MarkDirty(sqs)
	require.NoError(t, e.Flush(t.Context()))

	assert.Equal(t, 1, s.calls, "one Snapshot feeds both formats")

	for _, name := range []string{jsonFile, binFile} {
		assert.Equal(t, s.state, readFile(t, filepath.Join(dir, name)))

		var written int64

		for seen, size := range s.sizes {
			if strings.HasPrefix(seen, name+".tmp") {
				written = size
			}
		}

		// Half the state was written when Snapshot looked; all but what a write
		// buffer holds must already be in the file, not in memory.
		assert.Greater(t, written, int64(len(s.state)/4), "%s is written while Snapshot runs", name)
	}

	assert.Equal(t, []string{binFile, jsonFile}, names(t, dir))

	t.Run("a failed Snapshot leaves the old files and no temp file", func(t *testing.T) {
		old := s.state
		s.state = "state:" + strings.Repeat("new!", 1<<16)
		s.failWith = errSnapshot

		e.MarkDirty(sqs)
		require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)

		assert.Equal(t, []string{binFile, jsonFile}, names(t, dir))
		assert.Equal(t, old, readFile(t, filepath.Join(dir, jsonFile)))
		assert.Equal(t, old, readFile(t, filepath.Join(dir, binFile)))
	})
}
