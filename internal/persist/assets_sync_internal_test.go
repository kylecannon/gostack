package persist

import (
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

// syncRecord is one fsync of an asset or a fanout directory, with the state file
// on disk when it ran.
type syncRecord struct {
	path   string
	onDisk string
}

// recordSyncs replaces the fsync of a with one that records each call and the
// state file on disk at the time.
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

		mu.Lock()
		records = append(records, syncRecord{path: filepath.ToSlash(f.Name()), onDisk: string(onDisk)})
		mu.Unlock()

		return f.Sync()
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

// assertSyncedFirst checks that path was fsynced while the state file on disk
// did not yet name id.
func assertSyncedFirst(t *testing.T, records []syncRecord, path, id string) {
	t.Helper()

	for _, r := range records {
		if strings.HasSuffix(r.path, path) && !strings.Contains(r.onDisk, id) {
			return
		}
	}

	t.Errorf("%s was not fsynced before a snapshot naming %s was on disk; syncs: %v", path, id, records)
}

// Test_Assets_FailedSyncKeepsTheSnapshotOff checks that a save whose asset
// fsync fails leaves the state file as it was, and that the next save syncs
// the asset again.
func Test_Assets_FailedSyncKeepsTheSnapshotOff(t *testing.T) {
	dir := t.TempDir()
	e := New(Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour,
		Logger: slog.New(slog.DiscardHandler),
	})
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	id := commitAsset(t, a, "body")
	n.add(id)

	a.fsync = func(*os.File) error { return fs.ErrPermission }

	e.MarkDirty(syncService)
	require.ErrorIs(t, e.Flush(t.Context()), fs.ErrPermission)
	assert.NoFileExists(t, filepath.Join(dir, syncService, FormatJSON.fileName()))

	syncs := recordSyncs(a, filepath.Join(dir, syncService, FormatJSON.fileName()))

	require.NoError(t, e.Flush(t.Context()))

	file, _ := assetPath(id)
	assertSyncedFirst(t, syncs(), file, id)
}
