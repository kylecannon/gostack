//go:build linux

package persist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nearestMount returns the type of the mount nearest to dir, at it or above it
// short of "/", read from mountinfo with no code of this package, and whether
// there is one.
func nearestMount(t *testing.T, dir string) (fstype string, found bool) {
	t.Helper()

	b, err := os.ReadFile(mountinfoPath)
	require.NoError(t, err)

	types := make(map[string]string)

	for line := range strings.Lines(string(b)) {
		fields := strings.Fields(line)
		sep := 0

		for sep < len(fields) && fields[sep] != "-" {
			sep++
		}

		if sep >= 6 && sep+1 < len(fields) {
			point := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
			types[point] = fields[sep+1]
		}
	}

	for p := dir; p != "/"; p = filepath.Dir(p) {
		if fstype, ok := types[p]; ok {
			return fstype, true
		}
	}

	return "", false
}

// Test_placed_Mountinfo checks the placement against the real
// /proc/self/mountinfo, with no MountDetector: a directory is on the root
// filesystem when no mount point other than "/" is at it or above it, whatever
// its device is, and is in memory when the nearest mount is a tmpfs.
func Test_placed_Mountinfo(t *testing.T) {
	_, err := os.Stat(mountinfoPath)
	if err != nil {
		t.Skipf("no mountinfo to read: %v", err)
	}

	find := Config{}.mountLookup()
	require.NotNil(t, find)

	t.Run("a mount point", func(t *testing.T) {
		// /proc is a mount point, of a filesystem that is no memory one, on any
		// Linux with a /proc.
		assert.Equal(t, onVolume, placed(find, "/proc"))
	})

	t.Run("a temp dir", func(t *testing.T) {
		dir := evalPath(t, t.TempDir())

		fstype, found := nearestMount(t, dir)

		switch {
		case !found:
			assert.Equal(t, onRoot, placed(find, dir))
		case fstype == fsTmpfs || fstype == fsRamfs:
			assert.Equal(t, inMemory, placed(find, dir))
		default:
			assert.Contains(t, []placement{onVolume, onAnonymous}, placed(find, dir))
		}
	})
}
