//go:build !darwin

package persist

import "os"

// markerSync and markerFlush make a round of abandoned-save markers durable
// (see markBudget). Off macOS fsync(2) is the whole flush, so each marker and
// its directory is fsynced with [os.File.Sync] and there is no flush after.
// An Engine keeps them in fields, which tests replace.
var (
	markerSync  = syncFile
	markerFlush func(*os.File) error // nil: nothing to flush after the syncs
)
