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
func defaultSync(f *os.File) error {
	return f.Sync() //nolint:wrapcheck // syncPath wraps it with the path
}

// syncPending fsyncs the assets named ids that are still there, then the
// directory of each. It starts no fsync once ctx ends; the ones running finish.
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

	dirs := make(map[string]struct{})
	for _, path := range there {
		dirs[dirOf(path)] = struct{}{}
	}

	_, err = a.syncPaths(ctx, slices.Collect(maps.Keys(dirs)))

	return err
}

// syncPaths fsyncs paths, syncWorkers at a time, and returns the ones that are
// there. A disk commits concurrent fsyncs together. Once ctx ends it hands out
// no more paths and returns ctx's error.
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

	return there, errors.Join(append(errs, ctx.Err())...)
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
