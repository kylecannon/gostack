//go:build !linux

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

// syncWorkers is how many fsyncs a save runs at once.
const syncWorkers = 8

// defaultSync fsyncs one file or directory. On macOS Go's Sync is F_FULLFSYNC.
// A file's writeback error is reported once, so a failed fsync is final; see
// [Assets].
func defaultSync(f *os.File) error {
	return f.Sync() //nolint:wrapcheck // syncPath wraps it with the path
}

// openFSDir returns nil: each fsync opens the file or directory it syncs.
func openFSDir(*os.Root) (*os.File, error) {
	return nil, nil //nolint:nilnil // no directory is kept open off Linux
}

// syncPending fsyncs the assets named ids that are still there, then the
// directory of each. It starts no fsync once ctx ends; the ones running finish.
// It returns the fsync errors when there are any, and otherwise ctx's error
// when ctx ended before every path was synced, so a failed fsync is never
// taken for a sync cut short.
func (a *Assets) syncPending(ctx context.Context, ids map[string]struct{}) error {
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
// there, with the fsync errors joined. A disk commits concurrent fsyncs
// together. Once ctx ends it hands out no more paths.
func (a *Assets) syncPaths(ctx context.Context, paths []string) ([]string, error) {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		there = make([]string, 0, len(paths))
		errs  []error
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
				case !errors.Is(err, fs.ErrNotExist):
					errs = append(errs, err)
				default:
				}

				mu.Unlock()
			}
		})
	}

	a.feed(ctx, next, paths)
	wg.Wait()

	return there, errors.Join(errs...)
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

// syncPath fsyncs the file or the directory at path in the root.
func (a *Assets) syncPath(path string) error {
	f, err := a.root.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
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
