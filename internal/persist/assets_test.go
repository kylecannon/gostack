package persist_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// shelfService is the name the shelf registers under; S3 is the service the
	// assets are for.
	shelfService = "s3"
	shelfHeader  = "shelf"
)

// shelf is a service whose state is the set of asset ids it names. It snapshots
// them one per line under a header, and is a persist.Referencer.
type shelf struct {
	mu  sync.Mutex
	ids map[string]bool
	// before runs once at the start of the next Snapshot, before the state is
	// read; during runs once inside it, after the state is read.
	before func()
	during func()
	fail   error
	// restored runs once after the next Restore has replaced the state, as a
	// request that lands right after it would.
	restored func()
	// loaded holds the ids the last Restore read, until Restored takes them.
	loaded []string
}

func newShelf() *shelf {
	return &shelf{ids: make(map[string]bool)}
}

func (s *shelf) Snapshot(w io.Writer) error {
	s.mu.Lock()
	before := s.before
	s.before = nil
	s.mu.Unlock()

	if before != nil {
		before()
	}

	s.mu.Lock()
	fail := s.fail
	lines := append([]string{shelfHeader}, slices.Sorted(maps.Keys(s.ids))...)
	during := s.during
	s.during = nil
	s.mu.Unlock()

	if fail != nil {
		return fail
	}

	if during != nil {
		during()
	}

	_, err := io.WriteString(w, strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}

func (s *shelf) Restore(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	lines := strings.Split(string(b), "\n")
	if lines[0] != shelfHeader {
		return errors.New("not a shelf")
	}

	s.mu.Lock()
	clear(s.ids)

	s.loaded = lines[1:]
	for _, id := range s.loaded {
		s.ids[id] = true
	}

	restored := s.restored
	s.restored = nil
	s.mu.Unlock()

	if restored != nil {
		restored()
	}

	return nil
}

func (s *shelf) References() iter.Seq[string] {
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.ids))
	s.mu.Unlock()

	return slices.Values(ids)
}

func (s *shelf) Restored() iter.Seq[string] {
	s.mu.Lock()
	ids := s.loaded
	s.loaded = nil
	s.mu.Unlock()

	return slices.Values(ids)
}

func (s *shelf) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ids[id] = true
}

func (s *shelf) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.ids, id)
}

func (s *shelf) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Sorted(maps.Keys(s.ids))
}

func (s *shelf) setBefore(before func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.before = before
}

func (s *shelf) setRestored(restored func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.restored = restored
}

func (s *shelf) set(during func(), fail error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.during, s.fail = during, fail
}

// openShelf starts a gostack on dir: an engine with a shelf registered and its
// assets open, as a service opens them before the state loads.
func openShelf(t *testing.T, dir string) (*persist.Engine, *shelf, *persist.Assets) {
	t.Helper()

	e := persist.New(baseCfg(dir))
	s := newShelf()
	e.Register(shelfService, s)

	a, err := e.Assets(shelfService)
	require.NoError(t, err)
	require.NotNil(t, a)

	return e, s, a
}

// put stores body as a new asset and returns its id.
func put(t *testing.T, a *persist.Assets, body string) string {
	t.Helper()

	w, err := a.Create()
	require.NoError(t, err)

	_, err = io.WriteString(w, body)
	require.NoError(t, err)

	id, size, err := w.Commit()
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), size)

	return id
}

func assetsOf(dir string) string {
	return filepath.Join(dir, shelfService, "assets", ".gostack")
}

func blobFile(dir, id string) string {
	return filepath.Join(assetsOf(dir), "blobs", id[:2], id)
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()

	return names(t, filepath.Join(assetsOf(dir), "tmp"))
}

func flush(t *testing.T, e *persist.Engine) {
	t.Helper()

	e.MarkDirty(shelfService)
	require.NoError(t, e.Flush(t.Context()))
}

func load(t *testing.T, e *persist.Engine) {
	t.Helper()

	require.NoError(t, e.Load(t.Context()))
}

func Test_Assets_WriteAndOpen(t *testing.T) {
	dir := t.TempDir()
	_, _, a := openShelf(t, dir)

	id := put(t, a, "hello")

	assert.Regexp(t, `^[0-9a-f]{32}$`, id)
	assert.Equal(t, "hello", readFile(t, blobFile(dir, id)), "the asset is at blobs/<id[0:2]>/<id>")
	assert.Empty(t, tempFiles(t, dir), "the temp file was renamed")

	f, err := a.Open(id)
	require.NoError(t, err)

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.Equal(t, "hello", string(got))

	other := put(t, a, "")
	assert.NotEqual(t, id, other, "every asset gets an id of its own")

	_, err = a.Open(strings.Repeat("0", 32))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func Test_Assets_AbortAndEndedWriter(t *testing.T) {
	dir := t.TempDir()
	_, _, a := openShelf(t, dir)

	w, err := a.Create()
	require.NoError(t, err)

	_, err = io.WriteString(w, "half")
	require.NoError(t, err)
	assert.Len(t, tempFiles(t, dir), 1)

	w.Abort()
	w.Abort()

	assert.Empty(t, tempFiles(t, dir), "an aborted asset leaves nothing")

	_, err = io.WriteString(w, "more")
	require.Error(t, err)

	_, _, err = w.Commit()
	require.Error(t, err, "an aborted writer cannot commit")

	id := put(t, a, "x")

	committed, err := a.Create()
	require.NoError(t, err)

	_, _, err = committed.Commit()
	require.NoError(t, err)

	_, _, err = committed.Commit()
	require.Error(t, err, "a writer commits once")
	committed.Abort()

	assert.Equal(t, "x", readFile(t, blobFile(dir, id)), "Abort after Commit drops nothing")
}

// Test_Engine_Assets_FollowsPersistence checks that a service gets assets
// exactly when its state is persisted, as /_gostack/health's persisted flag
// reports through Enabled, and that they live under the base dir that
// PERSIST_BASE_DIR or PERSISTENCE=1 picks (Test_New_BaseDir covers the pick).
func Test_Engine_Assets_FollowsPersistence(t *testing.T) {
	off := false

	tests := []struct {
		name       string
		giveCfg    func(dir string) persist.Config
		wantAssets bool
	}{
		{
			name:    "persistence off",
			giveCfg: func(string) persist.Config { return persist.Config{MountDetector: func(string) bool { return false }} },
		},
		{
			name: "PERSIST_S3=0",
			giveCfg: func(dir string) persist.Config {
				cfg := baseCfg(dir)
				cfg.Services = map[string]bool{shelfService: false}

				return cfg
			},
		},
		{
			name: "PERSIST_DEFAULT=0",
			giveCfg: func(dir string) persist.Config {
				cfg := baseCfg(dir)
				cfg.Default = &off

				return cfg
			},
		},
		{
			name: "PERSIST_DEFAULT=0 PERSIST_S3=1",
			giveCfg: func(dir string) persist.Config {
				cfg := baseCfg(dir)
				cfg.Default, cfg.Services = &off, map[string]bool{shelfService: true}

				return cfg
			},
			wantAssets: true,
		},
		{
			name:       "every service persisted",
			giveCfg:    baseCfg,
			wantAssets: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			e := persist.New(tt.giveCfg(dir))
			e.Register(shelfService, newShelf())

			a, err := e.Assets(shelfService)
			require.NoError(t, err)

			assert.Equal(t, tt.wantAssets, a != nil)
			assert.Equal(t, e.Enabled(shelfService), a != nil, "assets go with health's persisted flag")

			if !tt.wantAssets {
				assert.NoDirExists(t, assetsOf(dir), "a service that is not persisted writes nothing")

				return
			}

			assert.DirExists(t, filepath.Join(e.BaseDir(), shelfService, "assets", ".gostack", "blobs", "ff"))

			again, err := e.Assets(shelfService)
			require.NoError(t, err)
			assert.Same(t, a, again, "the assets are opened once")
		})
	}
}

func Test_Engine_Assets_Refused(t *testing.T) {
	dir := t.TempDir()
	e := persist.New(baseCfg(dir))
	e.Register(sqs, &fake{})

	_, err := e.Assets(sts)
	require.Error(t, err, "an unregistered service gets no assets")

	_, err = e.Assets(sqs)
	require.Error(t, err, "a service that cannot name its assets gets none")
	assert.NoDirExists(t, filepath.Join(dir, sqs, "assets"), "SQS's directory is untouched")

	flushed := persist.New(baseCfg(dir))
	flushed.Register(sqs, &fake{state: stateA})
	flushed.MarkDirty(sqs)
	require.NoError(t, flushed.Flush(t.Context()))
	require.NoError(t, flushed.Load(t.Context()))
	assert.Equal(t, []string{jsonFile}, names(t, filepath.Join(dir, sqs)),
		"a service with no assets saves and loads as before")
}

func Test_Assets_RefusesIDsThatLeave(t *testing.T) {
	dir := t.TempDir()
	_, _, a := openShelf(t, dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, shelfService, jsonFile), []byte("secret"), 0o600))

	for _, id := range []string{
		"",
		"..",
		"../../" + jsonFile,
		"/etc/passwd",
		"blobs/00/" + strings.Repeat("0", 32),
		strings.Repeat("A", 32),
		strings.Repeat("0", 31),
		strings.Repeat("0", 30) + "/.",
		strings.Repeat("0", 33),
	} {
		_, err := a.Open(id)
		require.ErrorIs(t, err, fs.ErrInvalid, "Open(%q)", id)
		require.ErrorIs(t, a.Remove(id), fs.ErrInvalid, "Remove(%q)", id)
	}
}

func Test_Assets_LinkOutOfTheBaseDir(t *testing.T) {
	t.Run("a link planted at an asset's path is not followed out", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

		e, _, a := openShelf(t, dir)
		id := strings.Repeat("ab", 16)

		relative, err := filepath.Rel(filepath.Dir(blobFile(dir, id)), outside)
		require.NoError(t, err)

		for _, link := range []string{outside, relative} {
			require.NoError(t, os.Symlink(link, blobFile(dir, id)))

			_, err := a.Open(id)
			require.Error(t, err, "a link to %s", link)

			// No snapshot names it, so the next load removes the link, not what it
			// points at.
			load(t, e)
			assert.NoFileExists(t, blobFile(dir, id))
			assert.Equal(t, "outside", readFile(t, outside))
		}
	})

	t.Run("an assets directory that leaves the base dir turns the service off", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, shelfService), 0o750))
		require.NoError(t, os.Symlink(outside, filepath.Dir(assetsOf(dir))))

		e := persist.New(baseCfg(dir))
		e.Register(shelfService, newShelf())

		a, err := e.Assets(shelfService)
		require.ErrorIs(t, err, persist.ErrUnsafe)
		assert.Nil(t, a)
		assert.False(t, e.Enabled(shelfService), "health reports the service as not persisted")
		require.ErrorIs(t, e.Load(t.Context()), persist.ErrUnsafe)
		assert.Empty(t, names(t, outside), "nothing is written outside the base dir")
	})

	t.Run("load refuses the service before reading its state", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, shelfService), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, shelfService, jsonFile), []byte(shelfHeader+"\nid"), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Dir(assetsOf(dir))))

		e := persist.New(baseCfg(dir))
		s := newShelf()
		e.Register(shelfService, s)

		require.ErrorIs(t, e.Load(t.Context()), persist.ErrUnsafe)
		assert.Empty(t, s.names(), "the state is not loaded")
		assert.Empty(t, names(t, outside))
	})
}

func Test_Assets_RemoveWaitsForTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	id := put(t, a, "body")
	s.add(id)
	flush(t, e)

	s.drop(id)
	require.NoError(t, a.Remove(id))
	require.NoError(t, a.Remove(id), "a second Remove is the same removal")
	assert.FileExists(t, blobFile(dir, id), "nothing is deleted before a snapshot without it is on disk")

	s.set(nil, errSnapshot)
	e.MarkDirty(shelfService)
	require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)
	assert.FileExists(t, blobFile(dir, id), "a failed save deletes nothing")

	s.set(nil, nil)
	require.NoError(t, e.Flush(t.Context()), "the failed service is still dirty")
	assert.NoFileExists(t, blobFile(dir, id), "the asset goes once the snapshot without it is on disk")
	assert.NotContains(t, readFile(t, filepath.Join(dir, shelfService, jsonFile)), id)
}

func Test_Assets_RemoveDuringSnapshotWaitsForTheNextOne(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	id := put(t, a, "body")
	s.add(id)

	// The state is read with id in it; the delete lands while the snapshot is
	// being written, so this snapshot still names id.
	s.set(func() {
		s.drop(id)
		assert.NoError(t, a.Remove(id))
	}, nil)
	flush(t, e)

	assert.Contains(t, readFile(t, filepath.Join(dir, shelfService, jsonFile)), id)
	assert.FileExists(t, blobFile(dir, id), "the snapshot on disk names it")

	flush(t, e)
	assert.NoFileExists(t, blobFile(dir, id))
}

// Test_Assets_CrashBetweenCommitAndSnapshot is one of the crash-ordering tests.
// An asset committed and then lost with the process before any snapshot named
// it is an orphan, and the next start removes it with the temp file of a write
// the crash cut off.
func Test_Assets_CrashBetweenCommitAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	kept := put(t, a, "kept")
	s.add(kept)
	flush(t, e)

	orphan := put(t, a, "orphan")
	s.add(orphan) // in memory only: the process dies before the next flush

	cut, err := a.Create()
	require.NoError(t, err)

	_, err = io.WriteString(cut, "cut off")
	require.NoError(t, err)

	// The restart.
	e2, s2, _ := openShelf(t, dir)
	load(t, e2)

	assert.Equal(t, []string{kept}, s2.names())
	assert.FileExists(t, blobFile(dir, kept))
	assert.NoFileExists(t, blobFile(dir, orphan), "the orphan is newer than the snapshot and still goes")
	assert.Empty(t, tempFiles(t, dir))
}

// Test_Assets_PowerLossBeforeTheSave is one of the crash-ordering tests. A
// commit does not fsync, so a power loss before the next save can leave the
// renamed file empty or cut short. No snapshot names it, and the next start
// removes it like any orphan.
func Test_Assets_PowerLossBeforeTheSave(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	kept := put(t, a, "kept")
	s.add(kept)
	flush(t, e)

	empty := put(t, a, "lost to the power cut")
	torn := put(t, a, "cut short by the power cut")
	s.add(empty)
	s.add(torn) // in memory only: the power goes before the next flush

	require.NoError(t, os.Truncate(blobFile(dir, empty), 0))
	require.NoError(t, os.Truncate(blobFile(dir, torn), 3))

	// The restart.
	e2, s2, _ := openShelf(t, dir)
	load(t, e2)

	assert.Equal(t, []string{kept}, s2.names())
	assert.FileExists(t, blobFile(dir, kept))
	assert.NoFileExists(t, blobFile(dir, empty))
	assert.NoFileExists(t, blobFile(dir, torn))
}

// Test_Assets_CrashBetweenSnapshotAndRemoval is one of the crash-ordering
// tests. The removal of an asset fails after the snapshot without it is on disk,
// which leaves the disk as a crash at that point does. The next commit removes
// it; so does the next start.
func Test_Assets_CrashBetweenSnapshotAndRemoval(t *testing.T) {
	for _, recovery := range []string{"next commit", "restart"} {
		t.Run(recovery, func(t *testing.T) {
			skipIfRoot(t)

			dir := t.TempDir()
			e, s, a := openShelf(t, dir)

			id := put(t, a, "body")
			s.add(id)
			flush(t, e)

			fanoutDir := filepath.Dir(blobFile(dir, id))
			require.NoError(t, os.Chmod(fanoutDir, 0o500))
			t.Cleanup(func() { _ = os.Chmod(fanoutDir, 0o750) })

			s.drop(id)
			require.NoError(t, a.Remove(id))
			flush(t, e)

			assert.NotContains(t, readFile(t, filepath.Join(dir, shelfService, jsonFile)), id)
			assert.FileExists(t, blobFile(dir, id), "the removal failed after the commit")
			require.NoError(t, os.Chmod(fanoutDir, 0o750))

			if recovery == "next commit" {
				flush(t, e)
			} else {
				e2, _, _ := openShelf(t, dir)
				load(t, e2)
			}

			assert.NoFileExists(t, blobFile(dir, id))
		})
	}
}

// plant writes a committed asset's file as an earlier run left it: this process
// never committed it.
func plant(t *testing.T, dir, body string) string {
	t.Helper()

	id := strings.Repeat("c0", 15) + "ff"
	require.NoError(t, os.WriteFile(blobFile(dir, id), []byte(body), 0o600))

	return id
}

// Test_Assets_LiveLoad is /_gostack/state/load while the service runs: this
// process has written assets, so an orphan an earlier run left goes, and one
// this process committed stays, whatever its file time says.
func Test_Assets_LiveLoad(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	named := put(t, a, "named")
	s.add(named)
	flush(t, e)

	old := plant(t, dir, "old orphan")
	recent := put(t, a, "committed after the snapshot")

	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(blobFile(dir, old), past, past))
	require.NoError(t, os.Chtimes(blobFile(dir, recent), past, past))
	require.NoError(t, os.Chtimes(filepath.Join(dir, shelfService, jsonFile), future, future))

	// A delete that is not saved yet, which the load undoes.
	s.drop(named)
	require.NoError(t, a.Remove(named))

	live, err := a.Create()
	require.NoError(t, err)

	_, err = io.WriteString(live, "still writing")
	require.NoError(t, err)

	stale := filepath.Join(assetsOf(dir), "tmp", "left-by-a-crash")
	require.NoError(t, os.WriteFile(stale, nil, 0o600))

	load(t, e)

	assert.Equal(t, []string{named}, s.names())
	assert.FileExists(t, blobFile(dir, named))
	assert.NoFileExists(t, blobFile(dir, old))
	assert.FileExists(t, blobFile(dir, recent), "an asset this process committed may be on its way into the state")
	assert.NoFileExists(t, stale)

	id, _, err := live.Commit()
	require.NoError(t, err, "a write in progress survives the load")
	assert.Equal(t, "still writing", readFile(t, blobFile(dir, id)))

	flush(t, e)
	assert.FileExists(t, blobFile(dir, named), "the load dropped the removal of an asset it named again")
}

func Test_Assets_SetAsideStateKeepsEveryAsset(t *testing.T) {
	dir := t.TempDir()
	_, _, a := openShelf(t, dir)
	id := put(t, a, "body")

	state := filepath.Join(dir, shelfService, jsonFile)
	require.NoError(t, os.WriteFile(state, []byte(garbled), 0o600))

	e2, _, _ := openShelf(t, dir)
	load(t, e2)
	assert.Len(t, quarantined(t, filepath.Join(dir, shelfService), jsonFile), 1)
	assert.FileExists(t, blobFile(dir, id), "the file moved aside may name it")

	e3, _, _ := openShelf(t, dir)
	load(t, e3)
	assert.FileExists(t, blobFile(dir, id), "kept while the file moved aside is there")

	for _, moved := range quarantined(t, filepath.Join(dir, shelfService), jsonFile) {
		require.NoError(t, os.Remove(moved))
	}

	e4, _, _ := openShelf(t, dir)
	load(t, e4)
	assert.NoFileExists(t, blobFile(dir, id))
}

func Test_Engine_ResetAssets(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	named := put(t, a, "named")
	s.add(named)
	orphan := put(t, a, "orphan")
	flush(t, e)

	var after string

	require.NoError(t, e.ResetAssets(t.Context(), shelfService, func() {
		for _, id := range s.names() {
			s.drop(id)
		}

		// A request served after the wipe.
		after = put(t, a, "after")
		s.add(after)
	}))

	assert.Equal(t, []string{after}, s.names())
	assert.FileExists(t, blobFile(dir, named), "nothing goes before the wipe is saved")
	assert.FileExists(t, blobFile(dir, orphan))

	flush(t, e)
	assert.NoFileExists(t, blobFile(dir, named))
	assert.NoFileExists(t, blobFile(dir, orphan))
	assert.FileExists(t, blobFile(dir, after), "a write after the wipe stays")

	wiped := false
	off := persist.New(persist.Config{MountDetector: func(string) bool { return false }})
	require.NoError(t, off.ResetAssets(t.Context(), shelfService, func() { wiped = true }))
	assert.True(t, wiped, "a service that is not persisted is wiped")
}

func Test_Assets_Concurrent(t *testing.T) {
	const (
		workers   = 4
		perWorker = 8
	)

	dir := t.TempDir()
	cfg := baseCfg(dir)
	cfg.Frequency = persist.Immediate
	e := persist.New(cfg)
	s := newShelf()
	e.Register(shelfService, s)

	a, err := e.Assets(shelfService)
	require.NoError(t, err)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		removed []string
	)

	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				body := fmt.Sprintf("worker %d asset %d", w, i)

				id, err := commit(a, body)
				if err != nil {
					t.Errorf("commit: %v", err)

					return
				}

				s.add(id)
				e.MarkDirty(shelfService)

				err = check(a, id, body)
				if err != nil {
					t.Errorf("check: %v", err)

					return
				}

				if i%2 == 0 {
					continue
				}

				s.drop(id)

				err = a.Remove(id)
				if err != nil {
					t.Errorf("remove: %v", err)

					return
				}

				e.MarkDirty(shelfService)

				mu.Lock()
				removed = append(removed, id)
				mu.Unlock()
			}
		})
	}

	wg.Wait()
	require.NoError(t, e.Stop(t.Context()))
	flush(t, e)

	kept := s.names()
	require.Len(t, kept, workers*perWorker/2)
	require.Len(t, removed, workers*perWorker/2)

	for _, id := range kept {
		assert.FileExists(t, blobFile(dir, id))
	}

	for _, id := range removed {
		assert.NoFileExists(t, blobFile(dir, id))
	}

	assert.Empty(t, tempFiles(t, dir))
}

// commit is put for a goroutine that is not the test's.
func commit(a *persist.Assets, body string) (string, error) {
	w, err := a.Create()
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}

	_, err = io.WriteString(w, body)
	if err != nil {
		w.Abort()

		return "", fmt.Errorf("write: %w", err)
	}

	id, _, err := w.Commit()
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	return id, nil
}

func check(a *persist.Assets, id, want string) error {
	f, err := a.Open(id)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	if string(got) != want {
		return fmt.Errorf("asset %s holds %q, want %q", id, got, want)
	}

	return nil
}
