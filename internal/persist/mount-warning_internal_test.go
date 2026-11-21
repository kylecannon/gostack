package persist

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountsAt is a MountDetector that finds exactly the listed paths mounted.
func mountsAt(paths ...string) func(string) bool {
	return func(p string) bool { return slices.Contains(paths, p) }
}

func Test_Config_onRootFS(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "not", "made", "yet")

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))

	tests := []struct {
		name    string
		giveDir string
		giveAt  []string
		want    bool
	}{
		{name: "nothing is mounted", giveDir: dir, want: true},
		{name: "only the root is mounted", giveDir: dir, giveAt: []string{"/"}, want: true},
		{name: "the dir is a mount point", giveDir: dir, giveAt: []string{dir}},
		{name: "a directory above is a mount point", giveDir: missing, giveAt: []string{dir}},
		{name: "a mount elsewhere", giveDir: dir, giveAt: []string{filepath.Join(dir, "sibling")}, want: true},
		{name: "the target of a link is a mount point", giveDir: link, giveAt: []string{dir}},
		{name: "a dir not made yet under no mount", giveDir: missing, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The temp dir may sit under a link itself, as /var does on macOS, so
			// mount points are given as the resolved path.
			resolved := make([]string, len(tt.giveAt))

			for i, p := range tt.giveAt {
				r, err := filepath.EvalSymlinks(p)
				if err != nil {
					r = p
				}

				resolved[i] = r
			}

			c := Config{MountDetector: mountsAt(resolved...)}

			assert.Equal(t, tt.want, c.onRootFS(tt.giveDir))
		})
	}
}

// Test_Engine_Load_WarnsOfAnUnmountedBaseDir checks that persistence into a
// directory no volume is mounted at logs one warning, naming the directory, at
// the first Load and not at the next, and that a mounted one, or none, logs
// nothing.
func Test_Engine_Load_WarnsOfAnUnmountedBaseDir(t *testing.T) {
	dir := t.TempDir()

	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	tests := []struct {
		name     string
		giveCfg  Config
		wantWarn bool
	}{
		{
			name:     "a base dir on no mount",
			giveCfg:  Config{PersistVarSet: true, BaseDir: dir, MountDetector: mountsAt()},
			wantWarn: true,
		},
		{
			name:    "a base dir that is a mount point",
			giveCfg: Config{PersistVarSet: true, BaseDir: dir, MountDetector: mountsAt(resolved)},
		},
		{
			name:     "PERSISTENCE=1 with its state dir on no mount",
			giveCfg:  Config{Persistence: true, MountDetector: mountsAt()},
			wantWarn: true,
		},
		{
			name:    "persistence off",
			giveCfg: Config{MountDetector: mountsAt()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &warnings{}
			tt.giveCfg.Logger = slog.New(logs)
			tt.giveCfg.Frequency = time.Hour

			e := New(tt.giveCfg)
			t.Cleanup(func() { _ = e.Close(t.Context()) })

			// A state dir the test cannot make is no matter: the warning is
			// logged before the base dir is opened.
			_ = e.Load(t.Context())
			_ = e.Load(t.Context())

			if !tt.wantWarn {
				assert.Empty(t, logs.all())

				return
			}

			require.Len(t, logs.all(), 1, "once, not once per Load")
			assert.Contains(t, logs.all()[0], "dir="+e.BaseDir())
			assert.Contains(t, logs.all()[0], "lost with the container")
		})
	}
}
