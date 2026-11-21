//go:build darwin

package persist

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Test_Assets_DarwinFlushOpenFailureIsRetried checks that a save that has no
// descriptor left for the final flush's open of the assets directory fails
// without turning the service off: the process runs out of descriptors right
// after the last directory fsync (RLIMIT_NOFILE is lowered to zero there and
// restored after the save), and the next save fsyncs the assets again, flushes,
// and writes the state file (SQ-431 M1). The open is retryable like a per-file
// one; the flush's own errors stay final (Test_Assets_DarwinFlushFailureFailsTheSave).
func Test_Assets_DarwinFlushOpenFailureIsRetried(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	events := record(f.a, f.stateFile, nil)

	f.commit(t, "body")

	var old unix.Rlimit

	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &old))

	restore := func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &old) }
	t.Cleanup(restore)

	var lowered atomic.Bool

	recorded := f.a.fsync
	f.a.fsync = func(file *os.File) error {
		err := recorded(file)

		info, statErr := file.Stat()
		if statErr == nil && info.IsDir() && lowered.CompareAndSwap(false, true) {
			_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 0, Max: old.Max})
		}

		return err
	}

	err := f.e.Flush(t.Context())

	restore()

	require.True(t, lowered.Load(), "the descriptors ran out after a directory fsync")
	require.ErrorIs(t, err, unix.EMFILE)
	require.ErrorIs(t, err, errNotOpened, "an open that failed lost no write")
	assert.NoFileExists(t, f.stateFile, "a save whose flush did not run renames nothing")
	assert.Equal(t, 0, events.count("flush"), "the flush never ran")
	assert.True(t, f.e.Enabled(syncService), "the service stays on")
	assert.Empty(t, f.logs.errors())

	fsyncs := events.count("fsync")

	f.e.MarkDirty(syncService)
	require.NoError(t, f.e.Flush(t.Context()), "the next save, with descriptors again, succeeds")
	assert.Greater(t, events.count("fsync"), fsyncs, "it fsyncs the assets again, which were kept pending")
	assert.Equal(t, 1, events.count("flush"), "and flushes them")
	assert.Equal(t, "flush", events.last())
	assert.Len(t, f.onDisk(t), 1)
}

// Test_Assets_DarwinFlushENOTSUPFallsBackToFsync checks that a drive flush
// that answers ENOTSUP, as a filesystem with none does (an SMB mount, go#64215),
// leaves the save to succeed with an fsync of the assets directory, as Go's own
// Sync does there (SQ-431 S1).
func Test_Assets_DarwinFlushENOTSUPFallsBackToFsync(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	events := record(f.a, f.stateFile, fmt.Errorf("F_FULLFSYNC: %w", unix.ENOTSUP))

	f.commit(t, "body")

	require.NoError(t, f.e.Flush(t.Context()))
	assert.FileExists(t, f.stateFile)
	assert.True(t, f.e.Enabled(syncService))
	assert.Equal(t, 1, events.count("flush"))
	assert.Equal(t, "fsync", events.last(), "the directory is fsynced after the flush that was not supported")
	assert.Empty(t, f.logs.errors())
}
