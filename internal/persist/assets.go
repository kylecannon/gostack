package persist

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const (
	// assetsDir is a service's assets directory, beside its state files.
	// localstack-persist keeps S3's objects in the same directory, at
	// <bucket>/<key>@<version>.
	assetsDir = "assets"
	// ownDir is gostack's directory under assetsDir, the only one it touches. A
	// bucket name starts with a letter or a digit, so no bucket of
	// localstack-persist's is ever named like it.
	ownDir = ".gostack"
	// blobsDir holds the committed assets, at blobs/<id[0:2]>/<id>.
	blobsDir = "blobs"
	// tmpDir holds the files of the assets being written.
	tmpDir = "tmp"

	// assetIDBytes is the length of an asset id before hex: 128 random bits.
	assetIDBytes = 16
	// fanout is the number of directories under blobs: one per first byte of an
	// id, all made when the assets are opened.
	fanout = 256
)

var (
	// errNoReferencer is why a service that cannot name the assets its state
	// holds gets none: Load could not tell its assets from orphans.
	errNoReferencer = errors.New("the service's Snapshotter is not a Referencer")
	// errNotRegistered is why a service the engine does not track gets no assets.
	errNotRegistered = errors.New("the service is not registered")
	// errWriterEnded is returned by an AssetWriter after Commit or Abort.
	errWriterEnded = errors.New("the asset writer has ended")
	// errNotDirectory is why an assets directory is refused when tmp, blobs or a
	// fanout directory is a link or a file: a commit would land outside the
	// directory it was meant for.
	errNotDirectory = errors.New("an assets directory is a link or a file where a directory belongs")
)

// Referencer is implemented by a Snapshotter whose state names assets. A
// service needs it to get [Assets]: after its state file loads, the engine
// keeps the assets References or Restored names and removes the others, so
// References has to name every asset the state holds, and Restored every asset
// the file Restore read names.
type Referencer interface {
	// References returns the ids of the assets the service's state names.
	References() iter.Seq[string]
	// Restored returns the ids of the assets the snapshot the last Restore read
	// names, and forgets them. The engine calls it once after each Restore. It
	// covers what References cannot: a request that runs once Restore has
	// returned can drop an id from the state before it calls Remove, while the
	// file on disk still names the asset.
	Restored() iter.Seq[string]
}

// Assets holds the large files of one service, such as S3's object bodies, in
// <base>/<service>/assets/.gostack. An asset is written once, under a random id
// the engine picks, and never changed; the snapshot names it by id. Every file
// it touches is reached through an os.Root on that directory, which it keeps
// open, so no id or link takes it outside, and whatever else sits in assets,
// such as localstack-persist's buckets, is never read, changed or removed.
//
// The files are ordered so that a crash, power loss included, never leaves a
// snapshot naming a file that is gone. A new asset is renamed into place before
// Commit returns, so before any snapshot can name it, and is fsynced, with its
// directory, by the next save, after that save has taken its snapshot and
// before it renames the snapshot into place. So no request waits for an fsync,
// and a snapshot on disk names only assets that are on disk. A removed asset is
// deleted only after a snapshot that no longer names it is on disk, with no
// fsync: an unlink a crash loses leaves an orphan. The exception is an asset
// committed after the last save ended: no snapshot on disk or in flight can
// name it, so Remove deletes it at once, which lets a full disk free what the
// saves that cannot run would not. A crash in between leaves an orphan, which
// the next Load removes whatever a power loss left of it. It is safe for
// concurrent use.
type Assets struct {
	root *os.Root
	// dir is the directory the root is on, for the log.
	dir string
	log *slog.Logger

	mu sync.Mutex
	// writing holds the temp files of the writers that are neither committed nor
	// aborted, which a sweep leaves alone.
	writing map[string]struct{}
	// removed holds the ids given to Remove, deleted after the next commit of a
	// snapshot of the service.
	removed map[string]struct{}
	// committed holds the ids this process committed that no load or save is
	// known to name yet, which a sweep leaves alone: a request may be about to
	// name one. An id leaves it once it is removed or once the state names it.
	// Until the first Create it is empty, so Load removes every asset the state
	// does not name.
	committed map[string]struct{}
	// fresh holds the ids committed since the last save ended, with no save
	// under way. No snapshot on disk or in flight can name them, so Remove
	// deletes them at once.
	fresh map[string]struct{}
	// capturing holds the ids committed before or during the save under way,
	// whose snapshot may name them. It is emptied when the save is durable or
	// has failed, and a Remove of one of its ids waits for the next save.
	capturing map[string]struct{}
	// unsynced holds the ids committed and not yet fsynced. A save fsyncs them,
	// with their directories, before its snapshot is renamed into place.
	unsynced map[string]struct{}
	// saving is true from begin to finish of a save of the service.
	saving bool
	// stuck is the cause of the last save or deletion error that left removals
	// queued, or "" when the queue is not stuck. It is logged once per change.
	stuck string

	// fsync makes what it is given durable: on Linux the whole filesystem of the
	// assets directory, with syncfs(2), elsewhere one file or directory. Tests
	// replace it to record the order of the syncs.
	fsync func(f *os.File) error
}

// AssetWriter writes one asset into a temp file. Exactly one of Commit and
// Abort ends it. It is not safe for concurrent use.
type AssetWriter struct {
	assets *Assets
	tmp    string
	f      *os.File
	size   int64
	err    error
}

// newAssets opens the assets of service under baseDir, making the directories
// that are missing and fsyncing them and their parents, so an asset committed
// into one is not lost with its directory. A link on the way that leaves
// baseDir is refused with errLinkLeaves, and a link or a file where tmp, blobs
// or a fanout directory belongs with errNotDirectory.
func newAssets(baseDir, service string, log *slog.Logger) (*Assets, error) {
	err := os.MkdirAll(baseDir, dirMode)
	if err != nil {
		return nil, fmt.Errorf("create base dir: %w", err)
	}

	base, err := os.OpenRoot(baseDir)
	if err != nil {
		return nil, fmt.Errorf("open base dir: %w", err)
	}

	defer func() { _ = base.Close() }()

	dir := service + "/" + assetsDir + "/" + ownDir

	root, err := openAssetsRoot(base, dir)
	if err != nil {
		if leaves(base, err) {
			return nil, fmt.Errorf("%s: %w", filepath.Join(baseDir, dir), errLinkLeaves)
		}

		return nil, err
	}

	return &Assets{
		root:      root,
		dir:       filepath.Join(baseDir, dir),
		log:       log,
		writing:   make(map[string]struct{}),
		removed:   make(map[string]struct{}),
		committed: make(map[string]struct{}),
		fresh:     make(map[string]struct{}),
		capturing: make(map[string]struct{}),
		unsynced:  make(map[string]struct{}),
		fsync:     defaultSync,
	}, nil
}

// Create starts a new asset.
func (a *Assets) Create() (*AssetWriter, error) {
	tmp := tmpDir + "/" + rand.Text()

	// The temp file is known to be live before it exists, so a sweep running
	// beside Create never sees it unclaimed.
	a.mu.Lock()
	a.writing[tmp] = struct{}{}
	a.mu.Unlock()

	f, err := a.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		a.forget(tmp)

		return nil, fmt.Errorf("create asset: %w", err)
	}

	return &AssetWriter{assets: a, tmp: tmp, f: f}, nil
}

// Open returns the asset named id for reading. An id the engine could not have
// picked is refused with an error wrapping fs.ErrInvalid; a missing asset is
// an error wrapping fs.ErrNotExist.
func (a *Assets) Open(id string) (*os.File, error) {
	path, err := assetPath(id)
	if err != nil {
		return nil, err
	}

	f, err := a.root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open asset: %w", err)
	}

	return f, nil
}

// Remove deletes the asset named id once the next snapshot of the service is
// committed, so a crash before then leaves an orphan and never a snapshot that
// names a missing file. An asset committed after the last save ended is named
// by no snapshot, so it goes at once. The service calls it only once its state
// no longer names id. A reader that has opened the asset keeps reading it. An id
// the engine could not have picked is refused with an error wrapping
// fs.ErrInvalid.
func (a *Assets) Remove(id string) error {
	path, err := assetPath(id)
	if err != nil {
		return err
	}

	a.mu.Lock()

	_, now := a.fresh[id]
	if now {
		delete(a.fresh, id)
		delete(a.committed, id)
		delete(a.removed, id)
		delete(a.unsynced, id)
	} else {
		a.removed[id] = struct{}{}
	}

	a.mu.Unlock()

	if !now {
		return nil
	}

	err = a.root.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// The id waits for the next save like any other removal.
		a.requeue([]string{id})
		a.log.Warn("persist: an asset could not be removed; trying again after the next save",
			"dir", a.dir, "error", err)
	}

	return nil
}

// Write writes p to the asset's temp file.
func (w *AssetWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	n, err := w.f.Write(p)
	w.size += int64(n)

	if err != nil {
		w.err = fmt.Errorf("write asset: %w", err)

		return n, w.err
	}

	return n, nil
}

// Commit renames what was written to blobs/<id[0:2]>/<id> under a new random
// id and returns the id and the size. It does not fsync: the next save does,
// before its snapshot is on disk. On error nothing is stored.
func (w *AssetWriter) Commit() (string, int64, error) {
	if w.err != nil {
		err := w.err
		w.Abort()

		return "", 0, err
	}

	f := w.f
	w.f, w.err = nil, errWriterEnded

	id, err := w.assets.commit(f, w.tmp)
	if err != nil {
		return "", 0, err
	}

	return id, w.size, nil
}

// Abort drops what was written. It does nothing after Commit or Abort.
func (w *AssetWriter) Abort() {
	if w.f == nil {
		return
	}

	_ = w.f.Close()
	_ = w.assets.root.Remove(w.tmp)
	w.assets.forget(w.tmp)
	w.f, w.err = nil, errWriterEnded
}

// commit moves a complete temp file into place under a new id. The temp file
// stays claimed until it is renamed or removed, so a sweep cannot take it.
func (a *Assets) commit(f *os.File, tmp string) (string, error) {
	defer a.forget(tmp)

	err := f.Close()
	if err != nil {
		_ = a.root.Remove(tmp)

		return "", fmt.Errorf("close asset: %w", err)
	}

	id := newAssetID()
	dir := blobsDir + "/" + id[:2]
	path := dir + "/" + id

	// The id is known as committed before its file is in blobs, so a sweep that
	// lists the file also finds the id and keeps it.
	a.mu.Lock()
	a.committed[id] = struct{}{}
	a.mu.Unlock()

	err = a.root.Rename(tmp, path)
	if err != nil {
		_ = a.root.Remove(tmp)
		_ = a.root.Remove(path)

		a.mu.Lock()
		delete(a.committed, id)
		a.mu.Unlock()

		return "", fmt.Errorf("commit asset: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.saving {
		a.capturing[id] = struct{}{}
	} else {
		a.fresh[id] = struct{}{}
	}

	a.unsynced[id] = struct{}{}

	return id, nil
}

// sync makes every asset committed and not yet synced durable, with its
// directory: on Linux with one syncfs of the assets filesystem, elsewhere with
// an fsync of each asset and then of each of their directories once. A save
// calls it after it takes its snapshot and before it renames the snapshot into
// place: a service names an id only once Commit has returned, and Commit
// records the id before it returns, so every asset the snapshot names is synced
// or was synced by an earlier save. An asset deleted since its commit is
// skipped.
//
// It waits for the sync only until ctx ends, and then returns ctx's error. A
// sync cannot be interrupted, so it goes on alone and its result is dropped;
// the next save syncs the same ids again. On error the ids are kept for the
// next save, and the save that called it must not rename its snapshot. It is
// nil-safe, for a service with no assets.
func (a *Assets) sync(ctx context.Context) error {
	if a == nil {
		return nil
	}

	a.mu.Lock()
	ids := a.unsynced
	a.unsynced = make(map[string]struct{})
	a.mu.Unlock()

	if len(ids) == 0 {
		return nil
	}

	// The goroutine ends when its sync does; the buffer lets it end after sync
	// has stopped waiting.
	done := make(chan error, 1)

	go func() { done <- a.syncPending(ctx, ids) }()

	var err error

	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}

	if err != nil {
		a.mu.Lock()
		maps.Copy(a.unsynced, ids)
		a.mu.Unlock()

		return fmt.Errorf("sync assets: %w", err)
	}

	return nil
}

func (a *Assets) forget(tmp string) {
	a.mu.Lock()
	delete(a.writing, tmp)
	a.mu.Unlock()
}

// begin starts a save of the service: it returns the ids removed so far and
// starts a new batch, so the snapshot, taken after it, names none of them. The
// assets committed since the last save move to the ones the snapshot may name,
// and so does every asset committed until finish. It is nil-safe, for a service
// with no assets.
func (a *Assets) begin() []string {
	if a == nil {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.saving = true
	maps.Copy(a.capturing, a.fresh)
	clear(a.fresh)

	ids := make([]string, 0, len(a.removed))
	for id := range a.removed {
		ids = append(ids, id)
	}

	clear(a.removed)

	return ids
}

// finish ends the save begin started, which wrote the snapshot or failed with
// saveErr. The assets of the batch begin returned are deleted when the snapshot
// is on disk, and queued again when it is not. ref is the service's state when
// it names assets. It is nil-safe.
func (a *Assets) finish(batch []string, saveErr error, ref Referencer) {
	if a == nil {
		return
	}

	a.mu.Lock()
	a.saving = false
	clear(a.capturing)
	a.mu.Unlock()

	if saveErr != nil {
		a.requeue(batch)
		a.trackQueue(saveErr)

		return
	}

	a.trackQueue(a.apply(batch))
	a.settle(ref)
}

// trackQueue logs a removal queue that cannot drain, once each time the error
// changes, with the number of ids waiting and the error, and logs once when it
// drains again. A full disk shows here: the saves that would free the queued
// assets cannot write.
func (a *Assets) trackQueue(err error) {
	a.mu.Lock()

	queued, was, now := len(a.removed), a.stuck, ""
	if err != nil && queued > 0 {
		now = rootCause(err).Error()
	}

	a.stuck = now
	a.mu.Unlock()

	switch {
	case now != "" && now != was:
		a.log.Warn("persist: asset removals are waiting for a save that cannot finish",
			"dir", a.dir, "queued", queued, "error", err)
	case now == "" && was != "":
		a.log.Info("persist: asset removals are going through again", "dir", a.dir, "queued", queued)
	default:
	}
}

// rootCause is the innermost error of err's chain. A state file's temp name is
// random and is part of the error text, so only the cause, such as "no space
// left on device", says whether the error is the same one again.
func rootCause(err error) error {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}

		err = inner
	}
}

// settle forgets the committed ids the service's state names now: they are
// linked, so no request is about to name them. It is what bounds committed. The
// state is read without the lock, because a service calls Create and Remove
// under its own locks.
func (a *Assets) settle(ref Referencer) {
	if ref == nil {
		return
	}

	a.mu.Lock()
	pending := maps.Clone(a.committed)
	a.mu.Unlock()

	if len(pending) == 0 {
		return
	}

	var named []string

	for id := range ref.References() {
		if _, ok := pending[id]; ok {
			named = append(named, id)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, id := range named {
		delete(a.committed, id)
	}
}

// requeue puts back ids a flush took and did not delete, for the next one.
func (a *Assets) requeue(ids []string) {
	if a == nil || len(ids) == 0 {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, id := range ids {
		a.removed[id] = struct{}{}
	}
}

// keep drops the queued removals of the ids in refs: a load restored a state
// that names them again.
func (a *Assets) keep(refs map[string]struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for id := range a.removed {
		if _, named := refs[id]; named {
			delete(a.removed, id)
		}
	}
}

// apply deletes the assets a flush took, once the snapshot it wrote is on disk,
// and returns the first error. A missing asset is no failure. One that cannot be
// deleted is queued again for the next commit.
func (a *Assets) apply(ids []string) error {
	var (
		failed []string
		gone   []string
		first  error
	)

	for _, id := range ids {
		path, _ := assetPath(id) // Remove took only valid ids.

		err := a.root.Remove(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, id)
			first = cmp.Or(first, err)

			continue
		}

		gone = append(gone, id)
	}

	a.requeue(failed)

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, id := range gone {
		delete(a.committed, id)
		delete(a.unsynced, id)
	}

	if first != nil {
		return fmt.Errorf("remove assets: %w", first)
	}

	return nil
}

// list returns the id of every committed asset.
func (a *Assets) list() ([]string, error) {
	var ids []string

	for id, err := range a.blobs() {
		if err != nil {
			return ids, err
		}

		ids = append(ids, id.name)
	}

	return ids, nil
}

// removeTemps deletes the temp files of no live writer: those of a writer that
// was cut off by a crash. A temp file that is a link is removed as a link.
func (a *Assets) removeTemps() error {
	entries, err := readDir(a.root, tmpDir)
	if err != nil {
		return err
	}

	// Read after the listing: a temp file that was live when listed and is not
	// any more was renamed or removed by its writer.
	a.mu.Lock()
	live := maps.Clone(a.writing)
	a.mu.Unlock()

	for _, entry := range entries {
		tmp := tmpDir + "/" + entry.Name()
		if _, writing := live[tmp]; writing {
			continue
		}

		err = removeIfPresent(a.root, tmp)
		if err != nil {
			return err
		}
	}

	return nil
}

// sweep removes the committed assets that neither refs, the live state, nor
// loaded, the state file just loaded, names, and returns how many it removed.
// An asset the loaded file names is kept even when the live state has dropped
// it: the file is on disk until the next save, and the service may not have
// called Remove yet. An asset this process committed that refs does not name is
// kept: a request may still be about to name it. So is one queued for removal,
// which waits for the next save. No file time decides this, because a file's
// time is the end of its last write and says nothing about its commit. Before
// the first Create nothing is committed, so a start removes every orphan the
// last run left. The ids refs names leave the committed set: the state names
// them now.
func (a *Assets) sweep(refs, loaded map[string]struct{}) (int, error) {
	var orphans []blobEntry

	for entry, err := range a.blobs() {
		if err != nil {
			return 0, err
		}

		_, named := refs[entry.name]
		_, saved := loaded[entry.name]

		if !named && !saved {
			orphans = append(orphans, entry)
		}
	}

	// Read after the listing: commit marks an id committed before it renames the
	// file into blobs, so every listed asset this process committed is here.
	a.mu.Lock()
	maps.DeleteFunc(a.committed, func(id string, _ struct{}) bool {
		_, named := refs[id]

		return named
	})

	kept := maps.Clone(a.committed)
	maps.Copy(kept, a.removed)
	a.mu.Unlock()

	removed := 0

	for _, orphan := range orphans {
		if _, committed := kept[orphan.name]; committed {
			continue
		}

		err := removeIfPresent(a.root, orphan.path)
		if err != nil {
			return removed, err
		}

		removed++
	}

	return removed, nil
}

// blobEntry is one committed asset found on disk.
type blobEntry struct {
	name string
	path string
}

// blobs yields every committed asset under blobs: each entry of a fanout
// directory whose name is an id that belongs there. Anything else is left
// alone.
func (a *Assets) blobs() iter.Seq2[blobEntry, error] {
	return func(yield func(blobEntry, error) bool) {
		dirs, err := readDir(a.root, blobsDir)
		if err != nil {
			yield(blobEntry{}, err)

			return
		}

		for _, d := range dirs {
			if !d.IsDir() || len(d.Name()) != 2 || !isHex(d.Name()) {
				continue
			}

			dir := blobsDir + "/" + d.Name()

			entries, err := readDir(a.root, dir)
			if err != nil {
				yield(blobEntry{}, err)

				return
			}

			for _, entry := range entries {
				id := entry.Name()
				if !validAssetID(id) || id[:2] != d.Name() {
					continue
				}

				if !yield(blobEntry{name: id, path: dir + "/" + id}, nil) {
					return
				}
			}
		}
	}
}

// openAssetsRoot makes dir under base with its tmp and blobs directories and
// every fanout directory, fsyncs them and every parent of dir up to base, and
// opens a root on it.
func openAssetsRoot(base *os.Root, dir string) (*os.Root, error) {
	err := base.MkdirAll(dir, dirMode)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	root, err := base.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}

	err = makeAssetDirs(root)
	if err == nil {
		err = syncDirs(base, parentsOf(dir)...)
	}

	if err != nil {
		_ = root.Close()

		return nil, fmt.Errorf("prepare %s: %w", dir, err)
	}

	return root, nil
}

// makeAssetDirs makes tmp, blobs and the fanout directories that are missing,
// and checks that each one is a real directory, not a link or a file: a link
// from a fanout directory to tmp would send a commit into the directory Load
// empties. tmp and blobs are checked before the directories under blobs, so the
// check never goes through a link.
func makeAssetDirs(root *os.Root) error {
	dirs := slices.Grow([]string{tmpDir, blobsDir}, fanout)

	for i := range fanout {
		dirs = append(dirs, fmt.Sprintf("%s/%02x", blobsDir, i))
	}

	for _, dir := range dirs {
		err := root.Mkdir(dir, dirMode)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", dir, err)
		}

		info, err := root.Lstat(dir)
		if err != nil {
			return fmt.Errorf("check %s: %w", dir, err)
		}

		if !info.IsDir() {
			return fmt.Errorf("%s: %w", dir, errNotDirectory)
		}
	}

	return syncDirs(root, blobsDir, ".")
}

// parentsOf returns every directory above dir, nearest first, ending with ".".
func parentsOf(dir string) []string {
	var parents []string

	for dir != "." {
		dir = dirOf(dir)
		parents = append(parents, dir)
	}

	return parents
}

func syncDirs(root *os.Root, dirs ...string) error {
	for _, dir := range dirs {
		err := syncDir(root, dir)
		if err != nil {
			return err
		}
	}

	return nil
}

func readDir(root *os.Root, dir string) ([]fs.DirEntry, error) {
	d, err := root.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}

	entries, err := d.ReadDir(-1)
	_ = d.Close()

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	return entries, nil
}

// assetPath returns the path of the asset named id in the assets root, or an
// error wrapping fs.ErrInvalid when id is not one the engine could have picked:
// only 32 lower-case hex digits ever reach the file system.
func assetPath(id string) (string, error) {
	if !validAssetID(id) {
		return "", fmt.Errorf("asset id %q: %w", id, fs.ErrInvalid)
	}

	return blobsDir + "/" + id[:2] + "/" + id, nil
}

func validAssetID(id string) bool {
	return len(id) == 2*assetIDBytes && isHex(id)
}

func isHex(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}

func newAssetID() string {
	var raw [assetIDBytes]byte

	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(raw[:])

	return hex.EncodeToString(raw[:])
}
