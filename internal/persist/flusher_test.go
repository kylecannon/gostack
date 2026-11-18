package persist_test

import (
	"context"
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

// attempts is a Snapshotter that records when each Snapshot started and fails
// until it is healed. A Snapshot takes a few milliseconds, so changes land
// while a flush runs.
type attempts struct {
	mu      sync.Mutex
	at      []time.Time
	state   string
	failing bool
}

func (a *attempts) Snapshot(w io.Writer) error {
	a.mu.Lock()
	a.at = append(a.at, time.Now())
	failing, state := a.failing, a.state
	a.mu.Unlock()

	time.Sleep(5 * time.Millisecond)

	if failing {
		return errSnapshot
	}

	_, err := io.WriteString(w, state)

	return err //nolint:wrapcheck // a test double
}

func (*attempts) Restore(io.Reader) error { return nil }

func (a *attempts) set(state string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = state
}

func (a *attempts) heal() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.failing = false
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
		s := &attempts{failing: true}
		e.Register(sqs, s)

		// A change every millisecond for three seconds, against a disk that fails.
		for i := range 3000 {
			s.set("state:" + strconv.Itoa(i))
			e.MarkDirty(sqs)
			time.Sleep(time.Millisecond)
		}

		during := len(s.times())

		// The changes stop. The state is still unsaved, so the retries go on, at the
		// same pace and with no backlog of queued flushes.
		time.Sleep(2500 * time.Millisecond)
		synctest.Wait()

		at := s.times()
		require.Len(t, at, 6, "one attempt a second: three while changing, three after")

		for i := 1; i < len(at); i++ {
			assert.GreaterOrEqual(t, at[i].Sub(at[i-1]), time.Second, "attempt %d came early", i)
		}

		assert.LessOrEqual(t, len(at)-during, 3, "no burst once the changes stop")

		// The disk recovers: the next retry saves the last change, and the retries stop.
		s.heal()
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, "state:2999", readFile(t, filepath.Join(base, sqs, jsonFile)))

		saved := len(s.times())

		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Len(t, s.times(), saved, "no retry once the state is saved")
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
