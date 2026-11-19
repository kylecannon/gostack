//go:build linux

package persist

import (
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
