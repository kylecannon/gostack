package persist

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountsAt is a MountDetector that finds exactly the listed paths mounted.
func mountsAt(paths ...string) func(string) bool {
	return func(p string) bool { return slices.Contains(paths, p) }
}

// findAt is the lookup of the mounts a MountDetector that finds exactly the
// listed paths gives.
func findAt(paths ...string) lookup {
	return Config{MountDetector: mountsAt(paths...)}.mountLookup()
}

// evalPath returns path with its links resolved, as a mount point is listed in
// mountinfo. The temp dir may sit under a link itself, as /var does on macOS.
func evalPath(t *testing.T, path string) string {
	t.Helper()

	r, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)

	return r
}

func Test_placed(t *testing.T) {
	dir := evalPath(t, t.TempDir())
	missing := filepath.Join(dir, "not", "made", "yet")

	link := filepath.Join(evalPath(t, t.TempDir()), "link")
	require.NoError(t, os.Symlink(dir, link))

	tests := []struct {
		name    string
		giveDir string
		giveAt  []string
		want    placement
	}{
		{name: "nothing is mounted", giveDir: dir, want: onRoot},
		{name: "only the root is mounted", giveDir: dir, giveAt: []string{"/"}, want: onRoot},
		{name: "the dir is a mount point", giveDir: dir, giveAt: []string{dir}},
		{name: "a directory above is a mount point", giveDir: missing, giveAt: []string{dir}},
		{name: "a mount elsewhere", giveDir: dir, giveAt: []string{filepath.Join(dir, "sibling")}, want: onRoot},
		{name: "the target of a link is a mount point", giveDir: link, giveAt: []string{dir}},
		{name: "a dir not made yet under no mount", giveDir: missing, want: onRoot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, placed(findAt(tt.giveAt...), tt.giveDir))
		})
	}
}

// Test_placed_LinkBeforeDotDot checks that a ".." after a link is taken through
// the link, as the kernel does, not cleaned away as text first. In
// <root>/link/../data, link points into a volume, so the directory is the one
// beside the link's target, in the volume, and not <root>/data.
func Test_placed_LinkBeforeDotDot(t *testing.T) {
	root := evalPath(t, t.TempDir())
	vol := filepath.Join(root, "mnt", "vol")

	require.NoError(t, os.MkdirAll(filepath.Join(vol, "a"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(vol, "data"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(vol, "a"), filepath.Join(root, "link")))

	find := findAt(vol)

	assert.Equal(t, onVolume, placed(find, root+"/link/../data"), "the data dir in the volume")
	assert.Equal(t, onVolume, placed(find, root+"/link/../data/not/made"), "a path not made yet below it")
	assert.Equal(t, onRoot, placed(find, root+"/data"), "the data dir beside the link, which is no volume")
}

func Test_resolveExisting(t *testing.T) {
	root := evalPath(t, t.TempDir())

	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(root, "a", "b"), filepath.Join(root, "link")))

	t.Chdir(root)

	tests := []struct {
		name string
		give string
		want string
	}{
		{name: "a path with no link", give: root + "/a/b", want: root + "/a/b"},
		{name: "a link", give: root + "/link", want: root + "/a/b"},
		{name: "a link, then what is not made yet", give: root + "/link/x/y", want: root + "/a/b/x/y"},
		{name: "a parent step after a link", give: root + "/link/../c", want: root + "/a/c"},
		{name: "a relative path", give: "link/x", want: root + "/a/b/x"},
		{name: "nothing of it made", give: root + "/nope/x", want: root + "/nope/x"},
		{name: "a trailing slash", give: root + "/a/", want: root + "/a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveExisting(tt.give))
		})
	}
}

func Test_parseMountinfo(t *testing.T) {
	const info = `22 1 0:21 / /proc rw,nosuid - proc none rw
23 1 8:1 / /with\040space rw,relatime shared:1 master:2 - ext4 /dev/sda1 rw
24 1 0:22 / /dev/shm rw - tmpfs shm rw,size=64m
too short
25 1 8:1 /sub /twice rw - ext4 /dev/sda1 rw
26 1 8:1 /sup /twice rw - xfs /dev/sda1 rw
27 1 8:1 /no/fstype /noend rw -
`

	got, err := parseMountinfo(strings.NewReader(info))
	require.NoError(t, err)

	assert.Equal(t, map[string]mount{
		"/proc":       {root: "/", fstype: "proc"},
		"/with space": {root: "/", fstype: fsExt4},
		"/dev/shm":    {root: "/", fstype: fsTmpfs},
		"/twice":      {root: "/sup", fstype: fsXFS},
	}, got, "optional fields are skipped, an escape read, a line with no type left out, the later of two mounts kept")
}

// Test_parseMountinfo_LongLine checks that a line over bufio.Scanner's 64 KiB
// token limit, as an overlay root with many lower layers makes, does not end the
// scan: the mount after it is still found.
func Test_parseMountinfo_LongLine(t *testing.T) {
	long := "1234 1100 0:150 / / rw,relatime - overlay ovl rw,lowerdir=" +
		strings.Repeat("/l/AAAAAAAAAAAAAAAAAAAAAAAAAA:", 2500) + "\n"
	require.Greater(t, len(long), 64*1024)

	got, err := parseMountinfo(strings.NewReader(long + "1250 1234 8:1 / /persisted-data rw - ext4 /dev/sda1 rw\n"))
	require.NoError(t, err)

	assert.Contains(t, got, "/persisted-data")
}

// Test_parseMountinfo_ReadError checks that a failed read is an error, not the
// table read so far.
func Test_parseMountinfo_ReadError(t *testing.T) {
	_, err := parseMountinfo(io.MultiReader(strings.NewReader("1 1 0:1 / /a rw - ext4 x rw\n"), errReader{}))

	assert.Error(t, err)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, os.ErrInvalid }

// mountinfoFixture writes the mountinfo captured inside gostack's image under
// Docker (testdata/mountinfo) to a temp file, with every mount point other than
// "/" moved under root, so that the real resolveExisting, the real walk up the
// directories and the real parse run on this host, whatever it has at /var or
// /dev/shm. It returns the file.
func mountinfoFixture(t *testing.T, root, name string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", "mountinfo", name))
	require.NoError(t, err)

	var out strings.Builder

	for line := range strings.Lines(string(b)) {
		fields := strings.SplitN(line, " ", 6)
		require.Len(t, fields, 6)

		if fields[4] != "/" {
			fields[4] = root + fields[4]
		}

		out.WriteString(strings.Join(fields, " "))
	}

	file := filepath.Join(t.TempDir(), "mountinfo")
	require.NoError(t, os.WriteFile(file, []byte(out.String()), 0o600))

	return file
}

// loadWarnings runs the first Load of an engine saving the services to the base
// dir under root and returns the warnings it logged, after a second Load that
// is not to log any.
func loadWarnings(t *testing.T, cfg Config, services ...string) []string {
	t.Helper()

	logs := &warnings{}
	cfg.Logger = slog.New(logs)
	cfg.Frequency = time.Hour

	e := New(cfg)
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	for _, name := range services {
		e.Register(name, nopSnapshotter{})
	}

	require.NoError(t, e.Load(t.Context()))

	first := logs.all()

	require.NoError(t, e.Load(t.Context()))
	require.Equal(t, first, logs.all(), "the second Load logs nothing more")

	return first
}

// anonymous is the message of state on an anonymous volume.
const anonymous = "on an anonymous volume; it survives a restart but not docker rm or docker compose down"

// novolume is the mountinfo captured with no volume mounted.
const novolume = "docker-novolume.mountinfo"

// Test_Engine_Load_Warnings checks the warning of a service's state dir that is
// somewhere saved state does not last, against the mountinfo captured inside
// gostack's image under Docker (OrbStack, 29.8.2, arm64) in each layout, with
// docker run --entrypoint cat <image> /proc/self/mountinfo and the flags in the
// case's name.
func Test_Engine_Load_Warnings(t *testing.T) {
	off := false

	notInContainer := func(c *Config) { c.InContainer = false }
	onlyS3 := func(c *Config) { c.Default, c.Services = &off, map[string]bool{"s3": true} }
	nothing := func(c *Config) { c.Default = &off }

	tests := []struct {
		name         string
		giveFixture  string
		giveBase     string
		giveCfg      func(*Config)
		giveServices []string
		// want holds a substring of each warning, in order, the dir of which is
		// always the base dir.
		want []string
	}{
		{
			name:        "no volume, PERSIST_S3=1 PERSIST_SQS=1",
			giveFixture: novolume, giveBase: DefaultBaseDir,
			want: []string{"so saved state will be lost with the container services=s3,sqs"},
		},
		{
			name:        "no volume, no service persists: PERSIST_DEFAULT=0",
			giveFixture: novolume, giveBase: DefaultBaseDir, giveCfg: nothing,
		},
		{
			name:        "no volume, no service registered",
			giveFixture: novolume, giveBase: DefaultBaseDir, giveServices: []string{},
		},
		{
			name:        "no volume, out of a container",
			giveFixture: novolume, giveBase: DefaultBaseDir, giveCfg: notInContainer,
			want: []string{"the base dir is on the root filesystem, not on a mounted volume services=s3,sqs"},
		},
		{
			name:        "-v /var/lib/localstack, the image's anonymous volume, state dir",
			giveFixture: "docker-anon-var.mountinfo", giveBase: StateDir,
			want: []string{anonymous + " services=s3,sqs"},
		},
		{
			name:        "-v sq437-named:/var/lib/localstack, state dir",
			giveFixture: "docker-named-var.mountinfo", giveBase: StateDir,
		},
		{
			name:        "-v /host/dir:/var/lib/localstack, state dir",
			giveFixture: "docker-bindlib.mountinfo", giveBase: StateDir,
		},
		{
			name:        "-v sq437-named:/persisted-data",
			giveFixture: "docker-named.mountinfo", giveBase: DefaultBaseDir,
		},
		{
			name:        "-v /host/dir:/persisted-data",
			giveFixture: "docker-bind.mountinfo", giveBase: DefaultBaseDir,
		},
		{
			name:        "-v /persisted-data, an anonymous volume",
			giveFixture: "docker-anon-persisted.mountinfo", giveBase: DefaultBaseDir,
			want: []string{anonymous + " services=s3,sqs"},
		},
		{
			name:        "--tmpfs /persisted-data",
			giveFixture: "docker-tmpfs.mountinfo", giveBase: DefaultBaseDir,
			want: []string{"on a memory filesystem; lost when it is unmounted services=s3,sqs"},
		},
		{
			name:        "PERSIST_BASE_DIR=/dev/shm/gostack",
			giveFixture: novolume, giveBase: "/dev/shm/gostack",
			want: []string{"on a memory filesystem; lost when it is unmounted services=s3,sqs"},
		},
		{
			name:        "-v sq437-s3:/persisted-data/s3, s3 and sqs persist",
			giveFixture: "docker-pervolume.mountinfo", giveBase: DefaultBaseDir,
			want: []string{"so saved state will be lost with the container services=sqs"},
		},
		{
			name:        "-v sq437-s3:/persisted-data/s3, only s3 persists",
			giveFixture: "docker-pervolume.mountinfo", giveBase: DefaultBaseDir, giveCfg: onlyS3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := evalPath(t, t.TempDir())
			base := root + tt.giveBase

			cfg := Config{
				PersistVarSet: true, BaseDir: base, InContainer: true,
				mountinfo: mountinfoFixture(t, root, tt.giveFixture),
			}
			if tt.giveCfg != nil {
				tt.giveCfg(&cfg)
			}

			services := []string{"s3", "sqs"}
			if tt.giveServices != nil {
				services = tt.giveServices
			}

			got := loadWarnings(t, cfg, services...)

			require.Len(t, got, len(tt.want))

			for i, want := range tt.want {
				msg, services, _ := strings.Cut(want, " services=")

				assert.Contains(t, got[i], msg)
				assert.Contains(t, got[i], " dir="+base+" ")
				assert.True(t, strings.HasSuffix(got[i], " services="+services), got[i])
			}

			if tt.name == "no volume, out of a container" {
				assert.NotContains(t, got[0], "with the container")
			}
		})
	}
}

// Test_Engine_Load_WarningsInPlace checks the warning of a PERSISTENCE=1 engine,
// whose base dir is the state dir, against a detector that finds nothing
// mounted, and that the warning is logged at the first Load only.
func Test_Engine_Load_WarningsInPlace(t *testing.T) {
	got := loadWarnings(t, Config{Persistence: true, InContainer: true, MountDetector: mountsAt()}, "s3")

	require.Len(t, got, 1)
	assert.Contains(t, got[0], "dir="+StateDir)
	assert.Contains(t, got[0], "services=s3")

	assert.Empty(t, loadWarnings(t, Config{MountDetector: mountsAt()}, "s3"), "persistence is off")
}

// Test_Engine_Load_NoMountinfo checks that no warning is logged where the mounts
// cannot be read: no mountinfo file, as off Linux, and one that opens and cannot be
// read. Where it cannot tell, a service on the root filesystem is not told of.
func Test_Engine_Load_NoMountinfo(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name          string
		giveMountinfo string
	}{
		{name: "no such file", giveMountinfo: filepath.Join(dir, "not-there")},
		{name: "a directory, which opens and cannot be read", giveMountinfo: dir},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				PersistVarSet: true, BaseDir: filepath.Join(dir, "base"), InContainer: true,
				mountinfo: tt.giveMountinfo,
			}

			assert.Nil(t, cfg.mountLookup(), "it cannot tell")
			assert.Empty(t, loadWarnings(t, cfg, "s3", "sqs"))
		})
	}
}

// Test_Engine_Load_LongMountinfoLine checks that a mountinfo with a line over
// 64 KiB before the volume's is read to the end, so the volume is found and
// nothing is warned of.
func Test_Engine_Load_LongMountinfoLine(t *testing.T) {
	root := evalPath(t, t.TempDir())

	long := "1234 1100 0:150 / / rw,relatime - overlay ovl rw,lowerdir=" +
		strings.Repeat("/l/AAAAAAAAAAAAAAAAAAAAAAAAAA:", 2500) + "\n"
	info := long + "1250 1234 8:1 /docker/volumes/gostack-data/_data " + root + "/persisted-data rw - ext4 /dev/sda1 rw\n"

	file := filepath.Join(t.TempDir(), "mountinfo")
	require.NoError(t, os.WriteFile(file, []byte(info), 0o600))

	cfg := Config{PersistVarSet: true, BaseDir: root + "/persisted-data", InContainer: true, mountinfo: file}

	assert.Empty(t, loadWarnings(t, cfg, "s3", "sqs"))
}

// Test_Engine_Load_BaseDirBehindLink checks the base dir given through a link
// and a ".." after it: the volume the link leads into holds the state, so there
// is nothing to warn of.
func Test_Engine_Load_BaseDirBehindLink(t *testing.T) {
	root := evalPath(t, t.TempDir())
	vol := filepath.Join(root, "mnt", "vol")

	require.NoError(t, os.MkdirAll(filepath.Join(vol, "a"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(vol, "data"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(vol, "a"), filepath.Join(root, "link")))

	cfg := Config{PersistVarSet: true, InContainer: true, MountDetector: mountsAt(vol)}

	cfg.BaseDir = root + "/link/../data"
	assert.Empty(t, loadWarnings(t, cfg, "s3"), "the data dir in the volume")

	cfg.BaseDir = root + "/data"
	assert.Len(t, loadWarnings(t, cfg, "s3"), 1, "the data dir beside the link")
}
