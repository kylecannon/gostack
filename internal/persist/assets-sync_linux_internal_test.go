//go:build linux

package persist

import (
	"errors"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// syncModes forces each sync path Linux has, for tests that run under both.
var syncModes = map[string]func(*testing.T, *Assets){
	syncByFile:       forcePerFile,
	syncByFilesystem: forceSyncfs,
}

// Test_Assets_SyncPathOfTheFaultFilesystem checks that the filesystem
// scripts/writeback-fault.sh made, named in GOSTACK_WRITEBACK_FAULT_FS, takes
// syncfs only when it is in syncfsProven.
func Test_Assets_SyncPathOfTheFaultFilesystem(t *testing.T) {
	if os.Getenv(forceSyncfsEnv) == "1" {
		t.Skip("the fixture forces syncfs")
	}

	name := os.Getenv("GOSTACK_WRITEBACK_FAULT_FS")
	if name == "" {
		t.Skip("GOSTACK_WRITEBACK_FAULT_FS is not set; scripts/writeback-fault.sh runs this test")
	}

	f := newFaultFixture(t)

	proven := slices.Contains(slices.Collect(maps.Values(syncfsProven)), name)
	assert.Equal(t, proven, f.a.fsDir != nil, "%s takes syncfs only when it is proven", name)
}

// Test_syncChoice checks which sync path each filesystem takes: syncfs for the
// proven ones and for tmpfs and ramfs (no writeback to fail), the per-file path
// for overlay, FUSE, an unknown magic number and an unreadable type.
func Test_syncChoice(t *testing.T) {
	tests := []struct {
		name     string
		magic    uint32
		statErr  error
		wantName string
		wantFS   bool
	}{
		{name: fsExt4, magic: unix.EXT4_SUPER_MAGIC, wantName: fsExt4, wantFS: true},
		{name: fsXFS, magic: unix.XFS_SUPER_MAGIC, wantName: fsXFS, wantFS: true},
		{name: "btrfs", magic: unix.BTRFS_SUPER_MAGIC, wantName: "btrfs", wantFS: true},
		{name: fsTmpfs, magic: unix.TMPFS_MAGIC, wantName: fsTmpfs, wantFS: true},
		{name: fsRamfs, magic: unix.RAMFS_MAGIC, wantName: fsRamfs, wantFS: true},
		{name: "overlay", magic: unix.OVERLAYFS_SUPER_MAGIC, wantName: "overlay"},
		{name: "fuse", magic: unix.FUSE_SUPER_MAGIC, wantName: "fuse"},
		{name: "unknown", magic: 0x1234abcd, wantName: "0x1234abcd"},
		{name: "unreadable", magic: unix.TMPFS_MAGIC, statErr: errors.New("boom"), wantName: "unknown (boom)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, byFS := syncChoice(tt.magic, tt.statErr)

			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantFS, byFS)
		})
	}
}

// forceSyncfs makes a sync its whole filesystem with syncfs, whatever
// filesystem it is on.
func forceSyncfs(t *testing.T, a *Assets) {
	t.Helper()

	if a.fsDir == nil {
		dir, err := a.root.Open(".")
		require.NoError(t, err)

		a.fsDir = dir
	}

	a.fsync = syncFS
}
