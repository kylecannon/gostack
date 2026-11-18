package persist_test

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	unloadableInfix = ".unloadable-"
	restoreWait     = 100 * time.Millisecond
)

// syncBuffer is a log sink that background flushes can write to while a test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // a bytes.Buffer write never fails
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func loggedCfg(dir string, logs *syncBuffer) persist.Config {
	cfg := baseCfg(dir)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))

	return cfg
}

// quarantined lists the files in dir that Load moved aside from name.
func quarantined(t *testing.T, dir, name string) []string {
	t.Helper()

	found, err := filepath.Glob(filepath.Join(dir, name+unloadableInfix+"*"))
	require.NoError(t, err)

	return found
}

func skipIfRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("file modes do not bind root")
	}
}

func Test_Engine_Load_MovesAnUnloadableFileAside(t *testing.T) {
	tests := []struct {
		name      string
		giveBytes string
		giveMode  os.FileMode
	}{
		{name: "a newer version", giveBytes: `{"v":2,"queues":[]}`, giveMode: 0o600},
		{name: "truncated", giveBytes: "state", giveMode: 0o600},
		{name: "unreadable", giveBytes: stateA, giveMode: 0o000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.giveMode == 0 {
				skipIfRoot(t)
			}

			base := t.TempDir()
			dir := filepath.Join(base, sqs)
			require.NoError(t, os.MkdirAll(dir, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, jsonFile), []byte(tt.giveBytes), tt.giveMode))

			var logs syncBuffer

			e := persist.New(loggedCfg(base, &logs))
			s := &fake{}
			e.Register(sqs, s)

			require.NoError(t, e.Load(t.Context()))
			assert.Empty(t, s.get(), "the service starts empty")

			moved := quarantined(t, dir, jsonFile)
			require.Len(t, moved, 1)
			assert.NoFileExists(t, filepath.Join(dir, jsonFile))
			assert.Contains(t, logs.String(), "level=ERROR")
			assert.Contains(t, logs.String(), filepath.Join(dir, jsonFile), "the log names the file")
			assert.Contains(t, logs.String(), moved[0], "the log names where it went")
			assert.True(t, e.Enabled(sqs), "saving stays on once the file is safe")

			// The first change after the failed load must not touch the moved file.
			s.set(stateB)
			e.MarkDirty(sqs)
			require.NoError(t, e.Flush(t.Context()))

			assert.Equal(t, stateB, readFile(t, filepath.Join(dir, jsonFile)))

			require.NoError(t, os.Chmod(moved[0], 0o600))
			assert.Equal(t, tt.giveBytes, readFile(t, moved[0]), "the original bytes are untouched")
		})
	}
}

func Test_Engine_Load_QuarantinesNeverReplaceEarlierOnes(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))

	e := persist.New(baseCfg(base))
	e.Register(sqs, &fake{})

	for _, content := range []string{"first", "second", "third"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, jsonFile), []byte(content), 0o600))
		require.NoError(t, e.Load(t.Context()))
	}

	moved := quarantined(t, dir, jsonFile)
	kept := make([]string, 0, len(moved))

	for _, path := range moved {
		kept = append(kept, readFile(t, path))
	}

	assert.ElementsMatch(t, []string{"first", "second", "third"}, kept)
}

// panicking is a Snapshotter whose Restore panics, as SQS's did on a null entry
// in its state file.
type panicking struct {
	fake
}

func (*panicking) Restore(io.Reader) error {
	panic("restore blew up on a null entry")
}

func Test_Engine_Load_RestorePanicIsAnUnloadableFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, sqs)
	require.NoError(t, os.MkdirAll(dir, 0o750))

	bad := `{"queues":[null]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, jsonFile), []byte(bad), 0o600))

	var logs syncBuffer

	e := persist.New(loggedCfg(base, &logs))
	s := &panicking{}
	e.Register(sqs, s)
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	require.NoError(t, e.Load(t.Context()), "the panic is an unloadable file, not a crash")

	moved := quarantined(t, dir, jsonFile)
	require.Len(t, moved, 1, "the file is moved aside")
	assert.Equal(t, bad, readFile(t, moved[0]))
	assert.Empty(t, s.get(), "the service starts empty")
	assert.Contains(t, logs.String(), "restore blew up on a null entry")
	assert.Contains(t, logs.String(), "restoreFile", "the log carries the stack")
	assert.True(t, e.Enabled(sqs), "saving stays on")

	s.set(stateB)
	e.MarkDirty(sqs)
	require.NoError(t, e.Flush(t.Context()))
	assert.Equal(t, stateB, readFile(t, filepath.Join(dir, jsonFile)), "later saves work")
	assert.Equal(t, bad, readFile(t, moved[0]), "and leave the moved file alone")
}

func Test_Engine_Load_RenameFailureTurnsSavingOff(t *testing.T) {
	skipIfRoot(t)

	base := t.TempDir()
	broken, healthy := filepath.Join(base, sqs), filepath.Join(base, sts)

	for _, dir := range []string{broken, healthy} {
		require.NoError(t, os.MkdirAll(dir, 0o750))
	}

	original := []byte("garbage")
	require.NoError(t, os.WriteFile(filepath.Join(broken, jsonFile), original, 0o600))
	// Renaming a file needs write access to its directory.
	require.NoError(t, os.Chmod(broken, 0o500))
	t.Cleanup(func() { _ = os.Chmod(broken, 0o750) })

	var logs syncBuffer

	e := persist.New(loggedCfg(base, &logs))
	s, other := &fake{state: stateA}, &fake{state: stateB}
	e.Register(sqs, s)
	e.Register(sts, other)

	err := e.Load(t.Context())
	require.Error(t, err, "Load does not report success for a service it could not load")
	assert.Contains(t, err.Error(), sqs)
	assert.NotContains(t, err.Error(), sts)
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.False(t, e.Enabled(sqs), "the health endpoint reports persisted: false")
	assert.True(t, e.Enabled(sts))

	e.MarkDirty(sqs)
	e.MarkDirty(sts)

	err = e.Flush(t.Context())
	require.Error(t, err, "nor does Flush, for a service it does not save")
	assert.Contains(t, err.Error(), sqs)

	assert.Equal(t, original, []byte(readFile(t, filepath.Join(broken, jsonFile))), "the file is never written")
	assert.Equal(t, []string{jsonFile}, names(t, broken))
	assert.Equal(t, stateB, readFile(t, filepath.Join(healthy, jsonFile)), "other services keep saving")
	assert.Equal(t, 1, other.count())
	assert.Zero(t, s.count(), "a service whose saving is off is not even snapshotted")

	require.Error(t, e.Load(t.Context()), "a later load still says so")
	require.Error(t, e.Close(t.Context()))
	assert.Equal(t, original, []byte(readFile(t, filepath.Join(broken, jsonFile))), "nor at shutdown")
}

func Test_Engine_Load_FallsBackAcrossFormats(t *testing.T) {
	both := []persist.Format{persist.FormatJSON, persist.FormatBinary}
	binaryFirst := []persist.Format{persist.FormatBinary, persist.FormatJSON}

	tests := []struct {
		name        string
		giveFormats []persist.Format
		giveFiles   map[string]string
		wantState   string
		wantMoved   []string
		wantKept    []string
	}{
		{
			name:        "the second format loads when the first is garbled",
			giveFormats: both,
			giveFiles:   map[string]string{jsonFile: garbled, binFile: stateB},
			wantState:   stateB,
			wantMoved:   []string{jsonFile},
			wantKept:    []string{binFile},
		},
		{
			name:        "the first good file wins",
			giveFormats: both,
			giveFiles:   map[string]string{jsonFile: stateJ, binFile: stateB},
			wantState:   stateJ,
			wantKept:    []string{binFile, jsonFile},
		},
		{
			name:        "the configured order decides, not json first",
			giveFormats: binaryFirst,
			giveFiles:   map[string]string{jsonFile: stateJ, binFile: stateB},
			wantState:   stateB,
			wantKept:    []string{binFile, jsonFile},
		},
		{
			name:        "binary first and garbled falls back to json",
			giveFormats: binaryFirst,
			giveFiles:   map[string]string{jsonFile: stateJ, binFile: garbled},
			wantState:   stateJ,
			wantMoved:   []string{binFile},
			wantKept:    []string{jsonFile},
		},
		{
			name:        "a missing file is not a failure",
			giveFormats: both,
			giveFiles:   map[string]string{binFile: stateB},
			wantState:   stateB,
			wantKept:    []string{binFile},
		},
		{
			name:        "every format garbled leaves the service empty and keeps both files",
			giveFormats: both,
			giveFiles:   map[string]string{jsonFile: "{one", binFile: "{two"},
			wantState:   "",
			wantMoved:   []string{binFile, jsonFile},
		},
		{
			name:        "a format that is not configured is still tried last",
			giveFormats: []persist.Format{persist.FormatJSON},
			giveFiles:   map[string]string{jsonFile: garbled, binFile: stateB},
			wantState:   stateB,
			wantMoved:   []string{jsonFile},
			wantKept:    []string{binFile},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, sqs)
			require.NoError(t, os.MkdirAll(dir, 0o750))

			for name, content := range tt.giveFiles {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
			}

			cfg := baseCfg(base)
			cfg.Formats = tt.giveFormats

			e := persist.New(cfg)
			s := &fake{}
			e.Register(sqs, s)

			require.NoError(t, e.Load(t.Context()))
			assert.Equal(t, tt.wantState, s.get())

			for _, name := range tt.wantMoved {
				moved := quarantined(t, dir, name)
				require.Len(t, moved, 1, name)
				assert.Equal(t, tt.giveFiles[name], readFile(t, moved[0]))
				assert.NoFileExists(t, filepath.Join(dir, name))
			}

			for _, name := range tt.wantKept {
				assert.Equal(t, tt.giveFiles[name], readFile(t, filepath.Join(dir, name)))
				assert.Empty(t, quarantined(t, dir, name))
			}
		})
	}
}

// Test_Engine_Symlinks covers links that stay inside the base dir; the ones
// that leave it are in confine_test.go.
func Test_Engine_Symlinks(t *testing.T) {
	t.Run("a relative link inside the base dir", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, sqs)
		require.NoError(t, os.MkdirAll(filepath.Join(base, "shared"), 0o750))
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(base, "shared", "real.json"), []byte(stateA), 0o600))
		require.NoError(t, os.Symlink(filepath.Join("..", "shared", "real.json"), filepath.Join(dir, jsonFile)))

		e := persist.New(baseCfg(base))
		s := &fake{}
		e.Register(sqs, s)

		require.NoError(t, e.Load(t.Context()))
		assert.Equal(t, stateA, s.get())

		s.set(stateB)
		e.MarkDirty(sqs)
		require.NoError(t, e.Flush(t.Context()))

		target, err := os.Readlink(filepath.Join(dir, jsonFile))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("..", "shared", "real.json"), target)
		assert.Equal(t, stateB, readFile(t, filepath.Join(base, "shared", "real.json")))
		assert.Equal(t, []string{"real.json"}, names(t, filepath.Join(base, "shared")), "the temp file is gone")
		assert.Equal(t, []string{jsonFile}, names(t, dir))
	})

	t.Run("dot-dot after a directory link resolves as the kernel does", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, sqs)
		shared := filepath.Join(base, "shared")
		require.NoError(t, os.MkdirAll(filepath.Join(shared, "dir"), 0o750))
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(shared, "real.json"), []byte(stateA), 0o600))

		// The kernel follows insub to shared/dir, and .. from there is shared, so
		// the link reads shared/real.json. Read as text it would be sqs/real.json.
		require.NoError(t, os.Symlink(filepath.Join("..", "shared", "dir"), filepath.Join(dir, "insub")))
		require.NoError(t, os.Symlink("insub/../real.json", filepath.Join(dir, jsonFile)))

		e := persist.New(baseCfg(base))
		s := &fake{}
		e.Register(sqs, s)

		require.NoError(t, e.Load(t.Context()))
		assert.Equal(t, stateA, s.get(), "loaded from the file the kernel resolves to")

		s.set(stateB)
		e.MarkDirty(sqs)
		require.NoError(t, e.Flush(t.Context()))

		assert.Equal(t, stateB, readFile(t, filepath.Join(shared, "real.json")), "saved to the same file")
		assert.Equal(t, []string{"dir", "real.json"}, names(t, shared))
		assert.Equal(t, []string{"insub", jsonFile}, names(t, dir), "no sqs/real.json appears")

		target, err := os.Readlink(filepath.Join(dir, jsonFile))
		require.NoError(t, err)
		assert.Equal(t, "insub/../real.json", target, "the link is kept")
	})

	t.Run("a link to a missing target is created on the first save", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, sqs)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(base, "shared"), 0o750))

		target := filepath.Join(base, "shared", "new.json")
		require.NoError(t, os.Symlink(filepath.Join("..", "shared", "new.json"), filepath.Join(dir, jsonFile)))

		e := persist.New(baseCfg(base))
		s := &fake{state: stateA}
		e.Register(sqs, s)

		require.NoError(t, e.Load(t.Context()))
		assert.Equal(t, stateA, s.get(), "a missing file is no failure: nothing moves")
		assert.Empty(t, quarantined(t, dir, jsonFile))

		e.MarkDirty(sqs)
		require.NoError(t, e.Flush(t.Context()))
		assert.Equal(t, stateA, readFile(t, target))
	})

	t.Run("an unloadable target moves the link, not the target", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, sqs)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(base, "shared"), 0o750))

		target := filepath.Join(base, "shared", "real.json")
		text := filepath.Join("..", "shared", "real.json")
		require.NoError(t, os.WriteFile(target, []byte("garbage"), 0o600))
		require.NoError(t, os.Symlink(text, filepath.Join(dir, jsonFile)))

		e := persist.New(baseCfg(base))
		s := &fake{}
		e.Register(sqs, s)

		require.NoError(t, e.Load(t.Context()))
		assert.Equal(t, "garbage", readFile(t, target), "the target is untouched")

		moved := quarantined(t, dir, jsonFile)
		require.Len(t, moved, 1)

		dest, err := os.Readlink(moved[0])
		require.NoError(t, err)
		assert.Equal(t, text, dest, "the moved link still points at the original bytes")

		s.set(stateB)
		e.MarkDirty(sqs)
		require.NoError(t, e.Flush(t.Context()))

		assert.Equal(t, "garbage", readFile(t, target), "the new state is not written through a moved link")
		assert.Equal(t, stateB, readFile(t, filepath.Join(dir, jsonFile)))
	})
}

// entries is a Snapshotter shaped like a service whose Restore empties the
// state before it adds the saved state back, as the SQS provider's does. Between
// two additions it lets other goroutines run, so a Snapshot that is not
// serialized with Restore sees the half-restored state.
type entries struct {
	mu        sync.Mutex
	items     []string
	snapshots atomic.Int64
}

func (e *entries) Snapshot(w io.Writer) error {
	e.snapshots.Add(1)

	e.mu.Lock()
	defer e.mu.Unlock()

	_, err := io.WriteString(w, "state:"+strings.Join(e.items, ","))
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}

func (e *entries) Restore(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	items := strings.Split(strings.TrimPrefix(string(b), "state:"), ",")

	e.mu.Lock()
	e.items = nil
	e.mu.Unlock()

	// Give a save on another thread the chance to take its snapshot while the
	// state is empty, and stop waiting once one has. When saves wait for the
	// restore, as they must, this waits out the whole budget.
	seen, deadline := e.snapshots.Load(), time.Now().Add(restoreWait)
	for e.snapshots.Load() == seen && time.Now().Before(deadline) {
		runtime.Gosched()
	}

	for _, item := range items {
		e.mu.Lock()
		e.items = append(e.items, item)
		e.mu.Unlock()
	}

	return nil
}

func (e *entries) fill(prefix string, n int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.items = e.items[:0]
	for i := range n {
		e.items = append(e.items, prefix+strconv.Itoa(i))
	}
}

func Test_Engine_Load_DoesNotOverlapASave(t *testing.T) {
	const (
		queues = 40
		rounds = 5
	)

	base := t.TempDir()
	file := filepath.Join(base, sqs, jsonFile)

	e := persist.New(baseCfg(base))
	s := &entries{}
	s.fill("old", queues)
	e.Register(sqs, s)

	e.MarkDirty(sqs)
	require.NoError(t, e.Flush(t.Context()))

	// whole reports whether the file on disk holds one complete state.
	whole := func(content string) bool {
		items := strings.Split(strings.TrimPrefix(content, "state:"), ",")
		if len(items) != queues {
			return false
		}

		prefix := strings.TrimRight(items[0], "0123456789")
		for i, item := range items {
			if item != prefix+strconv.Itoa(i) {
				return false
			}
		}

		return true
	}

	var wg sync.WaitGroup

	loaded := make(chan struct{})

	// Load is what POST /_gostack/state/load calls.
	wg.Go(func() {
		defer close(loaded)

		for range rounds {
			_ = e.Load(t.Context())
		}
	})

	// The saver reads the file after each of its own saves, so no later save
	// can hide a torn one.
	var torn []string

	wg.Go(func() {
		for {
			e.MarkDirty(sqs)
			_ = e.Flush(t.Context())

			b, err := os.ReadFile(file)
			if err == nil && !whole(string(b)) {
				torn = append(torn, string(b))
			}

			select {
			case <-loaded:
				return
			default:
			}
		}
	})

	wg.Wait()

	require.Empty(t, torn, "a save wrote a state that is neither whole old nor whole new")

	e.MarkDirty(sqs)
	require.NoError(t, e.Close(t.Context()))
	assert.True(t, whole(readFile(t, file)), "the state on disk after the last save is whole")
}
