package persist

import (
	"context"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const syncService = "svc"

// namer is a Snapshotter whose snapshot is the ids it names, one per line.
type namer struct {
	mu  sync.Mutex
	ids []string
	// during runs inside the next Snapshot, before the ids are read.
	during func() string
}

func (n *namer) Snapshot(w io.Writer) error {
	n.mu.Lock()
	during := n.during
	n.during = nil
	n.mu.Unlock()

	if during != nil {
		n.add(during())
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	_, err := io.WriteString(w, strings.Join(n.ids, "\n"))

	return err //nolint:wrapcheck // a test double
}

func (*namer) Restore(io.Reader) error { return nil }

func (n *namer) References() iter.Seq[string] {
	n.mu.Lock()
	defer n.mu.Unlock()

	return slices.Values(slices.Clone(n.ids))
}

func (*namer) Restored() iter.Seq[string] { return func(func(string) bool) {} }

func (n *namer) add(id string) {
	n.mu.Lock()
	n.ids = append(n.ids, id)
	n.mu.Unlock()
}

// syncRecord is one sync, with the paths it made durable and the state file on
// disk when it ran. A per-file fsync covers its one file or directory; a
// syncfs covers every path in the assets directory at the time.
type syncRecord struct {
	paths  []string
	onDisk string
}

// recordSyncs replaces the sync of a with one that records each call and the
// state file on disk at the time, then runs the real sync.
func recordSyncs(a *Assets, stateFile string) func() []syncRecord {
	var (
		mu      sync.Mutex
		records []syncRecord
	)

	a.fsync = func(f *os.File) error {
		onDisk, err := os.ReadFile(stateFile)
		if err != nil && !os.IsNotExist(err) {
			return err //nolint:wrapcheck // a test double
		}

		paths, err := syncedPaths(a, f)
		if err != nil {
			return err
		}

		mu.Lock()
		records = append(records, syncRecord{paths: paths, onDisk: string(onDisk)})
		mu.Unlock()

		return defaultSync(f)
	}

	return func() []syncRecord {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(records)
	}
}

func commitAsset(t *testing.T, a *Assets, body string) string {
	t.Helper()

	w, err := a.Create()
	require.NoError(t, err)

	_, err = io.WriteString(w, body)
	require.NoError(t, err)

	id, _, err := w.Commit()
	require.NoError(t, err)

	return id
}

// Test_Assets_SnapshotNamesOnlySyncedAssets pins the crash ordering a power loss
// needs: every asset a snapshot names is fsynced, and so is its fanout
// directory, before the snapshot is renamed into place. A commit does not
// fsync. An asset committed while the snapshot is taken, and named by it, is
// synced by that same save.
func Test_Assets_SnapshotNamesOnlySyncedAssets(t *testing.T) {
	dir := t.TempDir()
	e := New(Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour,
		Logger: slog.New(slog.DiscardHandler),
	})
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	syncs := recordSyncs(a, filepath.Join(dir, syncService, FormatJSON.fileName()))

	before := commitAsset(t, a, "before the first save")
	n.add(before)
	assert.Empty(t, syncs(), "a commit does not fsync")

	e.MarkDirty(syncService)
	require.NoError(t, e.Flush(t.Context()))

	between := commitAsset(t, a, "between the saves")
	n.add(between)

	var during string

	n.mu.Lock()
	n.during = func() string {
		during = commitAsset(t, a, "while the snapshot is taken")

		return during
	}
	n.mu.Unlock()

	e.MarkDirty(syncService)
	require.NoError(t, e.Flush(t.Context()))

	onDisk, err := os.ReadFile(filepath.Join(dir, syncService, FormatJSON.fileName()))
	require.NoError(t, err)

	named := strings.Split(string(onDisk), "\n")
	require.ElementsMatch(t, []string{before, between, during}, named)

	records := syncs()

	for _, id := range named {
		file, _ := assetPath(id)

		assertSyncedFirst(t, records, file, id)
		assertSyncedFirst(t, records, dirOf(file), id)
	}
}

// assertSyncedFirst checks that path was synced while the state file on disk
// did not yet name id.
func assertSyncedFirst(t *testing.T, records []syncRecord, path, id string) {
	t.Helper()

	for _, r := range records {
		if strings.Contains(r.onDisk, id) {
			continue
		}

		if slices.ContainsFunc(r.paths, func(p string) bool { return strings.HasSuffix(p, path) }) {
			return
		}
	}

	t.Errorf("%s was not fsynced before a snapshot naming %s was on disk; syncs: %v", path, id, records)
}

// Test_Assets_FailedSyncIsFinal checks that once an asset sync fails, no later
// save renames a snapshot, even when the sync would now report success: the
// kernel reports a lost write once (SQ-164 F1). The service turns off, so
// health reports it not persisted, and one error names the cause.
func Test_Assets_FailedSyncIsFinal(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, syncService, FormatJSON.fileName())
	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	id := commitAsset(t, a, "body")
	n.add(id)

	a.fsync = func(*os.File) error { return fs.ErrPermission }

	e.MarkDirty(syncService)
	require.ErrorIs(t, e.Flush(t.Context()), fs.ErrPermission)
	assert.NoFileExists(t, stateFile)

	syncs := recordSyncs(a, stateFile)

	e.MarkDirty(syncService)
	err = e.Flush(t.Context())
	require.ErrorIs(t, err, ErrUnsafe, "a sync that now succeeds does not make the asset durable")
	assert.NoFileExists(t, stateFile)
	assert.Empty(t, syncs(), "nothing is synced again")
	assert.False(t, e.Enabled(syncService), "health reports the service not persisted")

	errs := logs.errors()
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], fs.ErrPermission.Error())
}

// warnings is a slog handler that keeps the warn and error records.
type warnings struct {
	mu   sync.Mutex
	msgs map[slog.Level][]string
}

func (*warnings) Enabled(context.Context, slog.Level) bool { return true }

func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}

	line := r.Message
	r.Attrs(func(attr slog.Attr) bool {
		line += " " + attr.String()

		return true
	})

	w.mu.Lock()
	if w.msgs == nil {
		w.msgs = make(map[slog.Level][]string)
	}

	w.msgs[r.Level] = append(w.msgs[r.Level], line)
	w.mu.Unlock()

	return nil
}

func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }

func (w *warnings) WithGroup(string) slog.Handler { return w }

// all returns the warn records.
func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.msgs[slog.LevelWarn])
}

// errors returns the error records, each with its attributes.
func (w *warnings) errors() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.msgs[slog.LevelError])
}

// Test_Assets_SaveAbandonedAtItsDeadline checks that a save whose asset sync
// outlasts its deadline fails before its snapshot is renamed into place: the
// old state file stays, the asset stays pending, one warning is logged, and the
// next save syncs the asset and names it.
func Test_Assets_SaveAbandonedAtItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		stateFile := filepath.Join(dir, syncService, FormatJSON.fileName())
		logs := &warnings{}
		e := New(Config{PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Logger: slog.New(logs)})
		n := &namer{}
		e.Register(syncService, n)

		a, err := e.Assets(syncService)
		require.NoError(t, err)

		saved := commitAsset(t, a, "saved")
		n.add(saved)
		e.MarkDirty(syncService)
		require.NoError(t, e.Flush(t.Context()))

		late := commitAsset(t, a, "late")
		n.add(late)

		release := make(chan struct{})
		a.fsync = func(f *os.File) error {
			<-release

			return defaultSync(f)
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		e.MarkDirty(syncService)
		require.ErrorIs(t, e.Flush(ctx), context.DeadlineExceeded)

		onDisk, err := os.ReadFile(stateFile)
		require.NoError(t, err)
		assert.Equal(t, saved, string(onDisk), "the old state file stays")

		a.mu.Lock()
		_, pending := a.unsynced[late]
		a.mu.Unlock()
		assert.True(t, pending, "the asset stays pending")
		assert.Len(t, logs.all(), 1, "one warning for the abandoned save")

		// The abandoned sync ends on its own, after the save gave up on it.
		close(release)
		synctest.Wait()

		syncs := recordSyncs(a, stateFile)

		require.NoError(t, e.Flush(t.Context()))

		onDisk, err = os.ReadFile(stateFile)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{saved, late}, strings.Split(string(onDisk), "\n"))

		file, _ := assetPath(late)
		assertSyncedFirst(t, syncs(), file, late)
		assert.Len(t, logs.all(), 1, "a save in time logs no warning")
	})
}
