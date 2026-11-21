//go:build darwin

package persist

// markerSync and markerFlush make a round of abandoned-save markers durable
// (see markBudget). Each marker and its directory gets fsync(2), which on macOS
// hands the data to the drive and leaves it in the drive's cache, and one
// F_FULLFSYNC on the base dir follows the last of them: Apple's fcntl(2) says it
// "acts as a barrier, data that had been fsync'd on the same device before is
// guaranteed to be persisted when this call returns" (see chooseSync). Go's
// [os.File.Sync] is F_FULLFSYNC on macOS, so a sync of each marker and each
// directory would flush the drive once for each.
//
// An Engine keeps them in fields, which tests replace.
var (
	markerSync  = fsyncFile
	markerFlush = fullFsync
)
