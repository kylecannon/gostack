package persist

import (
	"bufio"
	"cmp"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// mountinfoPath is where the kernel lists the mounts of this process.
const mountinfoPath = "/proc/self/mountinfo"

// mount is one line of /proc/self/mountinfo, as far as the checks read it.
type mount struct {
	// root is the path inside the filesystem that is mounted at the point.
	root string
	// fstype is the filesystem type, such as ext4, overlay or tmpfs. It is empty
	// when the MountDetector, which answers only whether a path is a mount point,
	// found the mount.
	fstype string
}

// lookup finds the mount at a path, or reports none. The path must be the mount
// point itself.
type lookup func(path string) (mount, bool)

// mountLookup returns the means to find a mount, or nil when it cannot be told
// where anything is mounted: the mountinfo file is missing or unreadable, as it
// is off Linux, or a read of it fails.
func (c Config) mountLookup() lookup {
	if c.MountDetector != nil {
		return func(path string) (mount, bool) { return mount{}, c.MountDetector(path) }
	}

	f, err := os.Open(cmp.Or(c.mountinfo, mountinfoPath))
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	table, err := parseMountinfo(f)
	if err != nil {
		return nil
	}

	return func(path string) (mount, bool) {
		m, ok := table[path]

		return m, ok
	}
}

// parseMountinfo returns the mounts in the mountinfo text in r, keyed by mount
// point. Where a point is listed twice, the later mount covers the earlier one
// and is the one kept. A line has no length limit. A line that does not read as
// mountinfo is skipped, and an error is a failed read of r.
func parseMountinfo(r io.Reader) (map[string]mount, error) {
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	table := make(map[string]mount)
	br := bufio.NewReader(r)

	for {
		line, err := br.ReadString('\n')
		// Mount id, parent id, device, root, mount point, options, then any
		// number of optional fields, "-", the type, the source, and the options of
		// the superblock.
		fields := strings.Fields(line)
		if sep := slices.Index(fields, "-"); sep >= 6 && sep+1 < len(fields) {
			table[unescape.Replace(fields[4])] = mount{root: unescape.Replace(fields[3]), fstype: fields[sep+1]}
		}

		if errors.Is(err, io.EOF) {
			return table, nil
		}

		if err != nil {
			return nil, err
		}
	}
}

// placement is what a directory's mount says of whether what is written to it
// lasts.
type placement int

const (
	// onVolume is a directory on a mount that holds files: it is a volume, a
	// bind mount or a disk of the host.
	onVolume placement = iota
	// onRoot is a directory with no mount of its own, nor under one: it is in the
	// root filesystem, which is the container's layer in a container.
	onRoot
	// inMemory is a directory on a memory filesystem, tmpfs or ramfs, which holds
	// its files until it is unmounted.
	inMemory
	// onAnonymous is a directory on a volume no one named: Docker's or Podman's
	// own id for it. It outlives a restart of its container and not the
	// container's removal.
	onAnonymous
)

// anonymousVolume matches the root of a mount of an anonymous Docker or Podman
// volume: a directory named for a 64 digit hex id, in a directory of volumes.
// The path before it is the runtime's own, and varies with its storage driver
// and its host, as /docker/volumes/<id>/_data inside an OrbStack VM's btrfs and
// /var/lib/docker/volumes/<id>/_data on ext4.
var anonymousVolume = regexp.MustCompile(`/volumes/[0-9a-f]{64}/_data$`)

// placed reports how dir is placed: by the mount nearest to it, at dir or above
// it short of "/", which holds what is written there.
func placed(find lookup, dir string) placement {
	for p := resolveExisting(dir); p != filepath.Dir(p); p = filepath.Dir(p) {
		m, ok := find(p)
		if !ok {
			continue
		}

		switch {
		case m.fstype == "tmpfs" || m.fstype == "ramfs":
			return inMemory
		case anonymousVolume.MatchString(m.root):
			return onAnonymous
		default:
			return onVolume
		}
	}

	return onRoot
}

// resolveExisting returns path made absolute with the links in it resolved, as
// far as it exists. A link on the way is not a mount point, its target may be
// under one, and /proc/self/mountinfo lists resolved paths. The links are
// resolved before a ".." is cleaned away, as the kernel does, so
// /link/../data is in the directory above the link's target. What is not made
// yet is kept as it is.
func resolveExisting(path string) string {
	var rest []string

	p := path

	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			abs, absErr := filepath.Abs(filepath.Join(append([]string{resolved}, rest...)...))
			if absErr != nil {
				return path
			}

			return abs
		}

		if p == "/" || p == "." {
			return path
		}

		var name string

		p, name = splitLast(p)
		if name != "" {
			rest = append([]string{name}, rest...)
		}
	}
}

// splitLast splits path at its last slash, with no cleaning, so that a ".." is
// left for the links before it. A path in the root has "/" as its parent, and a
// path with no slash has ".". The root has the empty name.
func splitLast(path string) (parent, name string) {
	path = strings.TrimRight(path, "/")

	i := strings.LastIndex(path, "/")

	switch {
	case path == "":
		return "/", ""
	case i < 0:
		return ".", path
	case i == 0:
		return "/", path[1:]
	default:
		return path[:i], path[i+1:]
	}
}

// warnUnmounted logs a warning for the services whose own directory, under the
// base dir, is somewhere what is saved does not last. It checks each service's
// directory, not the base dir: a volume at /persisted-data/s3 holds S3's state
// and nothing else's, so the services that save elsewhere are the ones it names.
// With no service saving it logs nothing, and nor where the mounts cannot be
// read.
//
//   - In the root filesystem, which in a container is its layer, the state is
//     lost when the container is removed.
//   - On a memory filesystem it is lost when that is unmounted.
//   - On an anonymous volume it outlives a restart and not the container.
//
// The image declares VOLUME /var/lib/localstack, so the PERSISTENCE=1 state dir
// is on an anonymous volume unless a volume is mounted there by name.
func (e *Engine) warnUnmounted() {
	e.mu.Lock()
	names := slices.Sorted(maps.Keys(e.services))
	e.mu.Unlock()

	find := e.cfg.mountLookup()
	if len(names) == 0 || find == nil {
		return
	}

	// The path is not cleaned, so a ".." after a link is for resolveExisting.
	base := strings.TrimRight(e.dir, "/")
	byPlace := make(map[placement][]string)

	for _, name := range names {
		p := placed(find, base+"/"+name)
		byPlace[p] = append(byPlace[p], name)
	}

	for _, p := range []placement{onRoot, inMemory, onAnonymous} {
		if services := byPlace[p]; len(services) > 0 {
			e.log.Warn(e.cfg.warning(p), "dir", e.dir, "services", strings.Join(services, ","))
		}
	}
}

// warning returns the message for state saved in the placement p.
func (c Config) warning(p placement) string {
	switch {
	case p == inMemory:
		return "persist: saved state is on a memory filesystem; lost when it is unmounted"
	case p == onAnonymous:
		return "persist: saved state is on an anonymous volume; " +
			"it survives a restart but not docker rm or docker compose down"
	case c.InContainer:
		return "persist: the base dir is not on a mounted volume, so saved state will be lost with the container"
	default:
		return "persist: the base dir is on the root filesystem, not on a mounted volume"
	}
}
