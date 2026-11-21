//go:build linux

package persist

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sameDevice reports whether the files at a and b are on one device.
func sameDevice(t *testing.T, a, b string) bool {
	t.Helper()

	var sa, sb syscall.Stat_t

	require.NoError(t, syscall.Stat(a, &sa))
	require.NoError(t, syscall.Stat(b, &sb))

	return sa.Dev == sb.Dev
}

// Test_Config_onRootFS_Mountinfo checks the check against the real
// /proc/self/mountinfo, with no MountDetector: a directory on the same device as
// "/" is on the root filesystem, and a mount point other than "/" is not.
func Test_Config_onRootFS_Mountinfo(t *testing.T) {
	_, err := os.Stat(mountinfoPath)
	if err != nil {
		t.Skipf("no mountinfo to read: %v", err)
	}

	c := Config{}

	t.Run("a mount point", func(t *testing.T) {
		// /proc is a mount point, of its own device, on any Linux with a /proc.
		assert.False(t, c.onRootFS("/proc"))
	})

	t.Run("a temp dir", func(t *testing.T) {
		dir := t.TempDir()

		if !sameDevice(t, dir, "/") {
			t.Skip("the temp dir is on a filesystem of its own, so it is not on the root filesystem")
		}

		assert.True(t, c.onRootFS(dir))
	})
}
