//go:build !linux

package persist

import (
	"os"
	"path/filepath"
)

// syncedPaths returns what one sync on f makes durable. Off Linux it is an
// fsync of the one file or directory f is.
func syncedPaths(_ *Assets, f *os.File) ([]string, error) {
	return []string{filepath.ToSlash(f.Name())}, nil
}

// syncRounds is how many sync calls one sync of assets makes in a row: the
// fsyncs of the files, then the fsyncs of their directories.
const syncRounds = 2
