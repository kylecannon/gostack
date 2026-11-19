//go:build linux

package persist

import (
	"io/fs"
	"os"
	"path/filepath"
)

// syncedPaths returns what one sync on f makes durable. On Linux it is a
// syncfs of the filesystem, so it covers every file and directory in the
// assets directory when it runs, and nothing created after it.
func syncedPaths(a *Assets, _ *os.File) ([]string, error) {
	var paths []string

	err := filepath.WalkDir(a.dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		paths = append(paths, filepath.ToSlash(path))

		return nil
	})

	return paths, err //nolint:wrapcheck // a test helper
}

// syncRounds is how many sync calls one sync of assets makes in a row: one
// syncfs.
const syncRounds = 1
