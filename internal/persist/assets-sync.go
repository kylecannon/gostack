package persist

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sync"
)

// syncWorkers is how many fsyncs a per-file sync runs at once.
const syncWorkers = 8

// Names of the two ways a save makes assets durable, for the log.
const (
	syncByFilesystem = "syncfs"
	syncByFile       = "per-file fsync"
)

// syncMode is how a store makes its pending assets durable.
type syncMode struct {
	// dir is the assets directory, kept open for syncfs, or nil when every
	// pending asset and its directory is fsynced on its own.
	dir *os.File
	// sync makes what it is given durable: with dir set, the whole filesystem
	// dir is on, otherwise one file or directory.
	sync func(f *os.File) error
	// fs names the filesystem the assets are on, for the log.
	fs string
}

// syncFile fsyncs one file or directory. On macOS Go's Sync is F_FULLFSYNC. A
// file's writeback error is reported once, so a failed fsync is final; see
// [Assets].
func syncFile(f *os.File) error {
	return f.Sync() //nolint:wrapcheck // syncPath wraps it with the path
}

// syncPending makes the assets named ids durable, with their directories. With
// a directory kept open for syncfs it is one sync of the assets filesystem,
// which needs neither the ids nor their directories and cannot be interrupted:
// sync stops waiting for it when ctx ends. Otherwise it fsyncs each asset that
// is still there, then the directory of each, and starts no fsync once ctx
// ends. It returns the sync errors when there are any, and otherwise ctx's
// error when ctx ended before every path was synced, so a failed sync is never
// taken for a sync cut short.
func (a *Assets) syncPending(ctx context.Context, ids map[string]struct{}) error {
	if a.fsDir != nil {
		err := a.fsync(a.fsDir)
		if err != nil {
			return fmt.Errorf("sync the assets filesystem: %w", err)
		}

		return nil
	}

	files := make([]string, 0, len(ids))

	for id := range ids {
		path, _ := assetPath(id) // commit made only valid ids.
		files = append(files, path)
	}

	there, err := a.syncPaths(ctx, files)
	if err != nil {
		return err
	}

	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // sync wraps it
	}

	dirs := make(map[string]struct{})
	for _, path := range there {
		dirs[dirOf(path)] = struct{}{}
	}

	_, err = a.syncPaths(ctx, slices.Collect(maps.Keys(dirs)))
	if err != nil {
		return err
	}

	return ctx.Err() //nolint:wrapcheck // sync wraps it
}

// syncPaths fsyncs paths, syncWorkers at a time, and returns the ones that are
// there, with the errors joined. A disk commits concurrent fsyncs together.
// Once ctx ends it hands out no more paths.
//
// A path that is gone is skipped: an asset Remove or a save deleted after the
// sync took its id. The sync cannot tell that from an asset something else
// deleted, and either way no write of gostack's is lost, so it is no failure;
// a snapshot that names a missing asset fails only that asset, at its read.
//
// When an fsync or its close failed, only those errors are returned, so the
// failure is final however many paths could not be opened beside it. Otherwise
// the open errors are returned, each wrapping errNotOpened.
func (a *Assets) syncPaths(ctx context.Context, paths []string) ([]string, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		there    = make([]string, 0, len(paths))
		failed   []error
		unopened []error
	)

	next := make(chan string)

	for range min(syncWorkers, len(paths)) {
		wg.Go(func() {
			for path := range next {
				err := a.syncPath(path)

				mu.Lock()

				switch {
				case err == nil:
					there = append(there, path)
				case !errors.Is(err, errNotOpened):
					failed = append(failed, err)
				case !errors.Is(err, fs.ErrNotExist):
					unopened = append(unopened, err)
				default:
				}

				mu.Unlock()
			}
		})
	}

	a.feed(ctx, next, paths)
	wg.Wait()

	if len(failed) > 0 {
		return there, errors.Join(failed...)
	}

	return there, errors.Join(unopened...)
}

// feed sends paths to next until ctx ends, then closes it.
func (*Assets) feed(ctx context.Context, next chan<- string, paths []string) {
	defer close(next)

	for _, path := range paths {
		select {
		case next <- path:
		case <-ctx.Done():
			return
		}
	}
}

// syncPath fsyncs the file or the directory at path in the root, on a
// descriptor opened for it. An open that fails wraps errNotOpened: only the
// fsync and the close after it can report a lost write.
func (a *Assets) syncPath(path string) error {
	f, err := a.root.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w: %w", path, errNotOpened, err)
	}

	err = a.fsync(f)
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("sync %s: %w", path, err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	return nil
}
