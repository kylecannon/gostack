package persist_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sqs = "sqs"
	sts = "sts"

	dataDir  = "/data"
	jsonFile = "store.json"
	binFile  = "store.bin"
	stateA   = "state:a"
	stateB   = "state:b"
	stateJ   = "state:j"
	stateX   = "state:x"
	garbled  = "{garbage"

	flushEvery = 5 * time.Second
)

var errSnapshot = errors.New("snapshot failed")

// fake is a Snapshotter whose state is one string. Restore accepts only text that
// starts with "state:", and leaves the state empty for anything else.
type fake struct {
	mu        sync.Mutex
	state     string
	snapshots int
	failWith  error
}

func (f *fake) Snapshot(w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.snapshots++

	if f.failWith != nil {
		_, _ = io.WriteString(w, "partial")

		return f.failWith
	}

	_, err := io.WriteString(w, f.state)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}

func (f *fake) Restore(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if !strings.HasPrefix(string(b), "state:") {
		f.state = ""

		return errors.New("not a state file")
	}

	f.state = string(b)

	return nil
}

func (f *fake) set(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.state = state
}

func (f *fake) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.state
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.snapshots
}

func (f *fake) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failWith = err
}

func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func baseCfg(dir string) persist.Config {
	return persist.Config{
		PersistVarSet: true,
		BaseDir:       dir,
		Frequency:     flushEvery,
		Logger:        discard(),
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(b)
}

// names lists the entry names of dir, sorted.
func names(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}

	slices.Sort(out)

	return out
}

func Test_New_BaseDir(t *testing.T) {
	tests := []struct {
		name          string
		giveCfg       persist.Config
		giveMount     bool
		wantDir       string
		wantMountCall bool
	}{
		{
			name:          "nothing set",
			wantDir:       "",
			wantMountCall: true,
		},
		{
			name:          "mount only",
			giveMount:     true,
			wantDir:       persist.DefaultBaseDir,
			wantMountCall: true,
		},
		{
			name:          "mount and base dir",
			giveCfg:       persist.Config{BaseDir: dataDir},
			giveMount:     true,
			wantDir:       dataDir,
			wantMountCall: true,
		},
		{
			name:    "PERSIST var, default base dir",
			giveCfg: persist.Config{PersistVarSet: true},
			wantDir: persist.DefaultBaseDir,
		},
		{
			name:    "PERSIST var and base dir",
			giveCfg: persist.Config{PersistVarSet: true, BaseDir: dataDir},
			wantDir: dataDir,
		},
		{
			name:    "PERSIST var beats PERSISTENCE",
			giveCfg: persist.Config{PersistVarSet: true, Persistence: true, BaseDir: dataDir},
			wantDir: dataDir,
		},
		{
			name:          "mount beats PERSISTENCE",
			giveCfg:       persist.Config{Persistence: true},
			giveMount:     true,
			wantDir:       persist.DefaultBaseDir,
			wantMountCall: true,
		},
		{
			name:          "PERSISTENCE only",
			giveCfg:       persist.Config{Persistence: true},
			wantDir:       persist.StateDir,
			wantMountCall: true,
		},
		{
			name:          "PERSISTENCE only ignores base dir",
			giveCfg:       persist.Config{Persistence: true, BaseDir: dataDir},
			wantDir:       persist.StateDir,
			wantMountCall: true,
		},
		{
			name:          "base dir alone does not enable persistence",
			giveCfg:       persist.Config{BaseDir: dataDir},
			wantDir:       "",
			wantMountCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []string

			tt.giveCfg.MountDetector = func(path string) bool {
				asked = append(asked, path)

				return tt.giveMount
			}

			e := persist.New(tt.giveCfg)

			require.NotNil(t, e)
			assert.Equal(t, tt.wantDir, e.BaseDir())

			if tt.wantMountCall {
				assert.Equal(t, []string{persist.DefaultBaseDir}, asked)
			} else {
				assert.Empty(t, asked)
			}
		})
	}
}

func Test_Engine_Enabled(t *testing.T) {
	on, off := true, false

	tests := []struct {
		name        string
		giveDefault *bool
		giveService map[string]bool
		wantSQS     bool
		wantSTS     bool
	}{
		{name: "unset defaults to on", wantSQS: true, wantSTS: true},
		{name: "default on", giveDefault: &on, wantSQS: true, wantSTS: true},
		{name: "default off", giveDefault: &off, wantSQS: false, wantSTS: false},
		{
			name:        "service off beats default on",
			giveDefault: &on,
			giveService: map[string]bool{sqs: false},
			wantSQS:     false,
			wantSTS:     true,
		},
		{
			name:        "service on beats default off",
			giveDefault: &off,
			giveService: map[string]bool{sqs: true},
			wantSQS:     true,
			wantSTS:     false,
		},
		{
			name:        "service off with default unset",
			giveService: map[string]bool{sts: false},
			wantSQS:     true,
			wantSTS:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg(t.TempDir())
			cfg.Default = tt.giveDefault
			cfg.Services = tt.giveService

			e := persist.New(cfg)

			assert.Equal(t, tt.wantSQS, e.Enabled(sqs))
			assert.Equal(t, tt.wantSTS, e.Enabled(sts))

			s, other := &fake{state: "state:s"}, &fake{state: "state:o"}
			e.Register(sqs, s)
			e.Register(sts, other)
			e.MarkDirty(sqs)
			e.MarkDirty(sts)
			require.NoError(t, e.Flush(t.Context()))

			assert.Equal(t, tt.wantSQS, s.count() == 1)
			assert.Equal(t, tt.wantSTS, other.count() == 1)
			assert.Equal(t, tt.wantSQS, fileExists(filepath.Join(cfg.BaseDir, sqs, jsonFile)))
			assert.Equal(t, tt.wantSTS, fileExists(filepath.Join(cfg.BaseDir, sts, jsonFile)))
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func Test_Engine_Disabled(t *testing.T) {
	e := persist.New(persist.Config{MountDetector: func(string) bool { return false }})
	require.NotNil(t, e)

	s := &fake{state: stateX}
	e.Register(sqs, s)
	e.MarkDirty(sqs)

	require.NoError(t, e.Load(t.Context()))
	require.NoError(t, e.Flush(t.Context()))
	require.NoError(t, e.Close(t.Context()))

	assert.False(t, e.Enabled(sqs))
	assert.Empty(t, e.BaseDir())
	assert.Zero(t, s.count())
	assert.Equal(t, stateX, s.get())
}

func Test_Engine_Flush(t *testing.T) {
	tests := []struct {
		name        string
		giveFormats []persist.Format
		wantFiles   []string
	}{
		{name: "default is json", wantFiles: []string{jsonFile}},
		{name: "json", giveFormats: []persist.Format{persist.FormatJSON}, wantFiles: []string{jsonFile}},
		{name: "binary", giveFormats: []persist.Format{persist.FormatBinary}, wantFiles: []string{binFile}},
		{
			name:        "both",
			giveFormats: []persist.Format{persist.FormatJSON, persist.FormatBinary},
			wantFiles:   []string{binFile, jsonFile},
		},
		{
			name:        "both listed twice",
			giveFormats: []persist.Format{persist.FormatBinary, persist.FormatJSON, persist.FormatBinary},
			wantFiles:   []string{binFile, jsonFile},
		},
		{name: "unknown only falls back to json", giveFormats: []persist.Format{"yaml"}, wantFiles: []string{jsonFile}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg(t.TempDir())
			cfg.Formats = tt.giveFormats

			e := persist.New(cfg)
			s := &fake{state: "state:one"}
			e.Register(sqs, s)
			e.MarkDirty(sqs)

			require.NoError(t, e.Flush(t.Context()))

			dir := filepath.Join(cfg.BaseDir, sqs)
			assert.Equal(t, tt.wantFiles, names(t, dir))

			for _, f := range tt.wantFiles {
				assert.Equal(t, "state:one", readFile(t, filepath.Join(dir, f)))
			}

			assert.Equal(t, 1, s.count(), "one snapshot serves every listed format")
		})
	}
}

func Test_Engine_Flush_FormatSwitching(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	json, bin := []persist.Format{persist.FormatJSON}, []persist.Format{persist.FormatBinary}

	steps := []struct {
		giveFormats []persist.Format
		giveState   string
		wantFiles   []string
	}{
		{giveFormats: json, giveState: "state:1", wantFiles: []string{jsonFile}},
		{giveFormats: bin, giveState: "state:2", wantFiles: []string{binFile}},
		{
			giveFormats: []persist.Format{persist.FormatJSON, persist.FormatBinary},
			giveState:   "state:3",
			wantFiles:   []string{binFile, jsonFile},
		},
		{giveFormats: json, giveState: "state:4", wantFiles: []string{jsonFile}},
	}

	for _, step := range steps {
		cfg := baseCfg(base)
		cfg.Formats = step.giveFormats

		e := persist.New(cfg)
		e.Register(sqs, &fake{state: step.giveState})
		e.MarkDirty(sqs)

		require.NoError(t, e.Flush(t.Context()))
		assert.Equal(t, step.wantFiles, names(t, dir))

		for _, f := range step.wantFiles {
			assert.Equal(t, step.giveState, readFile(t, filepath.Join(dir, f)))
		}
	}
}

func Test_Engine_Flush_CrashLeavesOldFileAndNextFlushCleansTemp(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	// The state a process killed between creating the temp file and renaming it
	// leaves behind: the old file untouched next to a half-written temp file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, jsonFile), []byte("state:old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "store.json.tmp"), []byte("state:half"), 0o600))

	e := persist.New(baseCfg(base))
	s := &fake{}
	e.Register(sqs, s)

	require.NoError(t, e.Load(t.Context()))
	assert.Equal(t, "state:old", s.get(), "the crash left the old file intact")

	s.set("state:new")
	e.MarkDirty(sqs)
	require.NoError(t, e.Flush(t.Context()))

	assert.Equal(t, "state:new", readFile(t, filepath.Join(dir, jsonFile)))
	assert.Equal(t, []string{jsonFile}, names(t, dir), "the temp file is gone")
}

func Test_Engine_Flush_UnlistedFormatTempIsRemoved(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, binFile), []byte("state:old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "store.bin.tmp"), []byte("half"), 0o600))

	e := persist.New(baseCfg(base))
	e.Register(sqs, &fake{state: "state:new"})
	e.MarkDirty(sqs)

	require.NoError(t, e.Flush(t.Context()))
	assert.Equal(t, []string{jsonFile}, names(t, dir))
}

func Test_Engine_Flush_SnapshotErrorKeepsOldFileAndMarksDirty(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, sqs, jsonFile)

	e := persist.New(baseCfg(base))
	s := &fake{state: "state:old"}
	e.Register(sqs, s)
	e.MarkDirty(sqs)
	require.NoError(t, e.Flush(t.Context()))

	s.set("state:new")
	s.fail(errSnapshot)
	e.MarkDirty(sqs)

	require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)
	assert.Equal(t, "state:old", readFile(t, path), "a failed snapshot writes nothing")
	assert.Equal(t, []string{jsonFile}, names(t, filepath.Dir(path)))

	s.fail(nil)
	before := s.count()

	require.NoError(t, e.Flush(t.Context()))
	assert.Equal(t, before+1, s.count(), "the service was dirty again")
	assert.Equal(t, "state:new", readFile(t, path))

	require.NoError(t, e.Flush(t.Context()))
	assert.Equal(t, before+1, s.count(), "a clean service is not snapshotted")
}

func Test_Engine_Flush_AttemptsEveryDirtyService(t *testing.T) {
	base := t.TempDir()
	e := persist.New(baseCfg(base))
	bad, good := &fake{state: stateB}, &fake{state: "state:g"}
	bad.fail(errSnapshot)
	e.Register(sqs, bad)
	e.Register(sts, good)
	e.MarkDirty(sqs)
	e.MarkDirty(sts)
	// The failed flush re-arms the timer; Close stops it.
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)
	assert.Equal(t, "state:g", readFile(t, filepath.Join(base, sts, jsonFile)))
	assert.False(t, fileExists(filepath.Join(base, sqs, jsonFile)))
}

func Test_Engine_Flush_CanceledContext(t *testing.T) {
	base := t.TempDir()
	e := persist.New(baseCfg(base))
	s := &fake{state: stateX}
	e.Register(sqs, s)
	e.MarkDirty(sqs)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, e.Flush(ctx), context.Canceled)
	assert.Zero(t, s.count())

	require.NoError(t, e.Flush(t.Context()), "the service stayed dirty")
	assert.Equal(t, 1, s.count())
}

func Test_Engine_Flush_BaseDirIsAFile(t *testing.T) {
	base := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(base, nil, 0o600))

	e := persist.New(baseCfg(base))
	s := &fake{state: stateX}
	e.Register(sqs, s)
	e.MarkDirty(sqs)
	// The failed flush re-arms the timer; Close stops it.
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	require.Error(t, e.Flush(t.Context()))
	assert.Zero(t, s.count())
}

func Test_Engine_Timer(t *testing.T) {
	t.Run("one flush per burst, none while idle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			base := t.TempDir()
			e := persist.New(baseCfg(base))
			s := &fake{state: stateA}
			e.Register(sqs, s)

			time.Sleep(time.Hour)
			assert.Zero(t, s.count(), "no timer while nothing is dirty")

			for range 50 {
				e.MarkDirty(sqs)
				time.Sleep(flushEvery / 100)
			}

			time.Sleep(flushEvery - 50*flushEvery/100 - time.Nanosecond)
			synctest.Wait()
			assert.Zero(t, s.count(), "the timer has not fired yet")

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			assert.Equal(t, 1, s.count(), "one flush for the whole burst")
			assert.Equal(t, stateA, readFile(t, filepath.Join(base, sqs, jsonFile)))

			time.Sleep(time.Hour)
			assert.Equal(t, 1, s.count(), "no timer after the flush")

			e.MarkDirty(sqs)
			time.Sleep(flushEvery)
			synctest.Wait()
			assert.Equal(t, 2, s.count(), "the next burst arms a new timer")
		})
	})

	t.Run("one flush covers every dirty service", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := persist.New(baseCfg(t.TempDir()))
			a, b := &fake{state: stateA}, &fake{state: stateB}
			e.Register(sqs, a)
			e.Register(sts, b)

			e.MarkDirty(sqs)
			time.Sleep(flushEvery / 2)
			e.MarkDirty(sts)
			time.Sleep(flushEvery / 2)
			synctest.Wait()

			assert.Equal(t, 1, a.count())
			assert.Equal(t, 1, b.count())
		})
	})

	t.Run("the delay counts from the first change", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := persist.New(baseCfg(t.TempDir()))
			s := &fake{state: stateA}
			e.Register(sqs, s)

			e.MarkDirty(sqs)
			time.Sleep(flushEvery - time.Nanosecond)
			e.MarkDirty(sqs)
			time.Sleep(time.Nanosecond)
			synctest.Wait()

			assert.Equal(t, 1, s.count(), "a later change does not push the flush back")
		})
	})

	t.Run("a failed flush retries on the next tick", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			base := t.TempDir()
			e := persist.New(baseCfg(base))
			s := &fake{state: stateA}
			s.fail(errSnapshot)
			e.Register(sqs, s)

			e.MarkDirty(sqs)
			time.Sleep(flushEvery)
			synctest.Wait()
			assert.Equal(t, 1, s.count())
			assert.False(t, fileExists(filepath.Join(base, sqs, jsonFile)))

			s.fail(nil)
			time.Sleep(flushEvery)
			synctest.Wait()
			assert.Equal(t, 2, s.count())
			assert.Equal(t, stateA, readFile(t, filepath.Join(base, sqs, jsonFile)))

			time.Sleep(time.Hour)
			assert.Equal(t, 2, s.count(), "idle again once the retry succeeds")
		})
	})

	t.Run("a flush by hand cancels the timer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := persist.New(baseCfg(t.TempDir()))
			s := &fake{state: stateA}
			e.Register(sqs, s)

			e.MarkDirty(sqs)
			time.Sleep(flushEvery / 2)
			require.NoError(t, e.Flush(t.Context()))
			assert.Equal(t, 1, s.count())

			e.MarkDirty(sqs)
			time.Sleep(flushEvery / 2)
			synctest.Wait()
			assert.Equal(t, 1, s.count(), "the first timer is gone")

			time.Sleep(flushEvery / 2)
			synctest.Wait()
			assert.Equal(t, 2, s.count(), "the second change flushes on its own full delay")
		})
	})

	t.Run("default frequency", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cfg := baseCfg(t.TempDir())
			cfg.Frequency = 0
			e := persist.New(cfg)
			s := &fake{state: stateA}
			e.Register(sqs, s)

			e.MarkDirty(sqs)
			time.Sleep(persist.DefaultFrequency - time.Nanosecond)
			assert.Zero(t, s.count())

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			assert.Equal(t, 1, s.count())
		})
	})

	for _, giveFrequency := range []time.Duration{persist.Immediate, -time.Second} {
		t.Run("no wait when the frequency is "+giveFrequency.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := t.TempDir()
				cfg := baseCfg(base)
				cfg.Frequency = giveFrequency
				e := persist.New(cfg)
				s := &fake{state: stateA}
				e.Register(sqs, s)

				synctest.Wait()
				assert.Zero(t, s.count(), "nothing runs while idle")

				e.MarkDirty(sqs)
				synctest.Wait()
				assert.Equal(t, 1, s.count(), "a change is written at once")
				assert.Equal(t, stateA, readFile(t, filepath.Join(base, sqs, jsonFile)))

				time.Sleep(time.Hour)
				synctest.Wait()
				assert.Equal(t, 1, s.count(), "and the engine goes idle again: no polling")

				s.set(stateB)
				e.MarkDirty(sqs)
				synctest.Wait()
				assert.Equal(t, 2, s.count(), "every change is its own write")
				assert.Equal(t, stateB, readFile(t, filepath.Join(base, sqs, jsonFile)))
			})
		})
	}

	t.Run("a failing flush is not retried in a loop when changes are written at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cfg := baseCfg(t.TempDir())
			cfg.Frequency = persist.Immediate
			e := persist.New(cfg)
			s := &fake{state: stateA}
			s.fail(errSnapshot)
			e.Register(sqs, s)

			e.MarkDirty(sqs)
			synctest.Wait()
			assert.Equal(t, 1, s.count())

			time.Sleep(time.Second - time.Nanosecond)
			synctest.Wait()
			assert.Equal(t, 1, s.count(), "the retry waits")

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			assert.Equal(t, 2, s.count(), "and comes after a second")

			time.Sleep(2*time.Second - time.Nanosecond)
			synctest.Wait()
			assert.Equal(t, 2, s.count(), "the next waits twice as long")

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			assert.Equal(t, 3, s.count())

			_ = e.Close(t.Context())
		})
	})
}

func Test_Engine_MarkDirty_UnknownService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := persist.New(baseCfg(t.TempDir()))
		e.MarkDirty("nope")

		time.Sleep(time.Hour)
		synctest.Wait()
		require.NoError(t, e.Flush(t.Context()))
	})
}

func Test_Engine_Close(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := t.TempDir()
		e := persist.New(baseCfg(base))
		s := &fake{state: "state:last"}
		e.Register(sqs, s)

		e.MarkDirty(sqs)
		require.NoError(t, e.Close(t.Context()))
		assert.Equal(t, "state:last", readFile(t, filepath.Join(base, sqs, jsonFile)))
		assert.Equal(t, 1, s.count())

		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal(t, 1, s.count(), "the timer was stopped")

		e.MarkDirty(sqs)
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal(t, 1, s.count(), "no timer is armed after Close")

		require.NoError(t, e.Close(t.Context()), "Close can run twice")
		assert.Equal(t, 2, s.count(), "a second Close flushes what changed since")
	})
}

func Test_Engine_Load(t *testing.T) {
	tests := []struct {
		name      string
		giveFiles map[string]string
		wantState string
		wantLog   bool
	}{
		{name: "no files", wantState: ""},
		{name: "json", giveFiles: map[string]string{jsonFile: stateJ}, wantState: stateJ},
		{name: "binary", giveFiles: map[string]string{binFile: stateB}, wantState: stateB},
		{
			name:      "json is preferred",
			giveFiles: map[string]string{jsonFile: stateJ, binFile: stateB},
			wantState: stateJ,
		},
		{
			name:      "corrupt json is logged and the other format loads",
			giveFiles: map[string]string{jsonFile: garbled, binFile: stateB},
			wantState: stateB,
			wantLog:   true,
		},
		{
			name:      "corrupt json alone leaves the service empty",
			giveFiles: map[string]string{jsonFile: garbled},
			wantState: "",
			wantLog:   true,
		},
		{name: "empty file is corrupt", giveFiles: map[string]string{jsonFile: ""}, wantState: "", wantLog: true},
		{name: "temp file is ignored", giveFiles: map[string]string{"store.json.tmp": "state:t"}, wantState: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, sqs)
			require.NoError(t, os.MkdirAll(dir, 0o750))

			for name, content := range tt.giveFiles {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
			}

			var logs bytes.Buffer

			cfg := baseCfg(base)
			cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))

			e := persist.New(cfg)
			s, other := &fake{}, &fake{state: "state:untouched"}
			e.Register(sqs, s)
			e.Register(sts, other)

			require.NoError(t, e.Load(t.Context()))

			assert.Equal(t, tt.wantState, s.get())
			assert.Equal(t, "state:untouched", other.get(), "a service with no file is left alone")

			if tt.wantLog {
				assert.Contains(t, logs.String(), "service="+sqs)
				assert.Contains(t, logs.String(), "level=ERROR")
			} else {
				assert.Empty(t, logs.String())
			}
		})
	}
}

func Test_Engine_Load_CorruptServiceDoesNotStopTheOthers(t *testing.T) {
	base := t.TempDir()

	for svc, content := range map[string]string{sqs: "garbage", sts: "state:ok"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, svc), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(base, svc, jsonFile), []byte(content), 0o600))
	}

	e := persist.New(baseCfg(base))
	a, b := &fake{}, &fake{}
	e.Register(sqs, a)
	e.Register(sts, b)

	require.NoError(t, e.Load(t.Context()))
	assert.Empty(t, a.get())
	assert.Equal(t, "state:ok", b.get())
}

func Test_Engine_Load_MissingBaseDir(t *testing.T) {
	e := persist.New(baseCfg(filepath.Join(t.TempDir(), "absent")))
	s := &fake{}
	e.Register(sqs, s)

	require.NoError(t, e.Load(t.Context()))
	assert.Empty(t, s.get())
}

func Test_Engine_Load_CanceledContext(t *testing.T) {
	base := t.TempDir()
	e := persist.New(baseCfg(base))
	e.Register(sqs, &fake{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, e.Load(ctx), context.Canceled)
}

func Test_Engine_RoundTrip(t *testing.T) {
	base := t.TempDir()

	first := persist.New(baseCfg(base))
	first.Register(sqs, &fake{state: "state:kept"})
	first.MarkDirty(sqs)
	require.NoError(t, first.Close(t.Context()))

	second := persist.New(baseCfg(base))
	s := &fake{}
	second.Register(sqs, s)

	require.NoError(t, second.Load(t.Context()))
	assert.Equal(t, "state:kept", s.get())
}

func Test_Engine_Concurrent(t *testing.T) {
	base := t.TempDir()
	cfg := baseCfg(base)
	cfg.Frequency = time.Millisecond

	e := persist.New(cfg)
	s := &fake{}
	e.Register(sqs, s)

	var wg sync.WaitGroup

	var writes atomic.Int64

	for range 4 {
		wg.Go(func() {
			for range 200 {
				s.set("state:" + string(rune('a'+writes.Add(1)%26)))
				e.MarkDirty(sqs)
			}
		})
	}

	wg.Go(func() {
		for range 20 {
			_ = e.Flush(t.Context())
		}
	})

	wg.Wait()
	require.NoError(t, e.Close(t.Context()))

	assert.Equal(t, s.get(), readFile(t, filepath.Join(base, sqs, jsonFile)),
		"the last flush holds the final state")
	assert.Equal(t, []string{jsonFile}, names(t, filepath.Join(base, sqs)))
}
