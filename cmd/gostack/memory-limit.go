package main

import (
	"log/slog"
	"os"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
)

// cgroupV1Unlimited is where cgroup v1 reports a limit that is not set: the
// largest page-aligned int64, 2^63 less a page. Any limit past 2^60 is not a
// limit anyone set.
const cgroupV1Unlimited = 1 << 60

// setMemoryLimit reads the memory the process may use (processMemory), the
// smaller of its cgroup's memory limit and its machine's memory, and returns
// protocol.SoftLimit of it, the cap on what the decode budget takes as the Go
// memory limit (protocol.SetMemoryCap). When GOMEMLIMIT is not set it also
// gives the Go runtime that soft memory limit, and logs it once: the collector
// then works harder as the heap nears it, in place of letting the container's
// OOM killer end the process with a request half decoded. A GOMEMLIMIT in the
// environment, whatever its value (off included), is the user's and is left as
// the runtime read it; the cap still bounds what the decode budget takes from
// it. It returns 0 when it reads neither (darwin), or when the memory is too
// small to keep. root is the file system root the files are read below; set is
// debug.SetMemoryLimit.
func setMemoryLimit(getenv func(string) string, root string, set func(int64) int64) int64 {
	memory, source, ok := processMemory(root)
	if !ok {
		return 0
	}

	// Memory under 100 bytes rounds to zero, which the runtime takes as a
	// limit of nothing; it is no limit anyone could run under.
	soft := protocol.SoftLimit(memory)
	if soft <= 0 {
		return 0
	}

	if getenv("GOMEMLIMIT") != "" {
		slog.Info("gostack caps the decode budget's memory at the memory it may use; GOMEMLIMIT is left as set",
			"memory_cap", soft, "memory", memory, "percent", protocol.MemoryPercent, "source", source)

		return soft
	}

	set(soft)

	slog.Info("gostack set the Go memory limit from the memory it may use",
		"memory_limit", soft, "memory", memory, "percent", protocol.MemoryPercent, "source", source)

	return soft
}

// processMemory returns the memory the process may use, read below root, and
// the file it came from: the smaller of its cgroup's limit (cgroupMemoryLimit)
// and the machine's memory (machineMemory), since a limit set above the
// machine's memory leaves the machine's. It reports false when it reads
// neither.
func processMemory(root string) (memory int64, source string, ok bool) {
	limit, limitSource, limited := cgroupMemoryLimit(root)
	machine, machineSource, known := machineMemory(root)

	switch {
	case limited && (!known || limit <= machine):
		return limit, limitSource, true
	case known:
		return machine, machineSource, true
	default:
		return 0, "", false
	}
}

// machineMemory returns the memory of the machine, MemTotal in /proc/meminfo
// read below root, and the file it came from. It reports false when the file
// is missing or names no MemTotal (darwin).
func machineMemory(root string) (memory int64, source string, ok bool) {
	fsys, err := os.OpenRoot(root)
	if err != nil {
		return 0, "", false
	}

	defer func() { _ = fsys.Close() }()

	const file = "proc/meminfo"

	text, err := fsys.ReadFile(file)
	if err != nil {
		return 0, "", false
	}

	for line := range strings.SplitSeq(string(text), "\n") {
		rest, found := strings.CutPrefix(line, "MemTotal:")
		if !found {
			continue
		}

		kib, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
		if err != nil || kib <= 0 || kib >= cgroupV1Unlimited>>10 {
			return 0, "", false
		}

		return kib << 10, "/" + file, true //nolint:mnd // meminfo counts KiB
	}

	return 0, "", false
}

// cgroupMemoryLimit returns the memory limit in bytes of the cgroup this
// process runs in, read below root, and the file it came from. cgroup v2's
// memory.max is read in the process's own cgroup (/proc/self/cgroup) and in
// each cgroup above it up to the mount's root, which is the container's own
// cgroup when it has a namespace of its own; the kernel holds the process to
// every one of them, so the smallest gives the limit. Only when none of them
// sets one is cgroup v1's memory.limit_in_bytes read the same way, in the
// process's own cgroup of the memory controller and each above it up to that
// mount's root, a pod's or a slice's limit included. A file that says max, or
// no limit, or is missing, sets none. It reports false when none sets one.
func cgroupMemoryLimit(root string) (limit int64, source string, ok bool) {
	fsys, err := os.OpenRoot(root)
	if err != nil {
		return 0, "", false
	}

	defer func() { _ = fsys.Close() }()

	own, _ := fsys.ReadFile("proc/self/cgroup")
	v2, v1 := ownCgroups(string(own))

	limit, source, ok = smallestCgroupLimit(fsys, "sys/fs/cgroup", v2, "memory.max")
	if ok {
		return limit, source, true
	}

	return smallestCgroupLimit(fsys, "sys/fs/cgroup/memory", v1, "memory.limit_in_bytes")
}

// smallestCgroupLimit returns the smallest limit name sets in the cgroup dir
// below the mount and in each cgroup above it up to the mount's root, and the
// file it came from; it reports false when none sets one.
func smallestCgroupLimit(fsys *os.Root, mount, dir, name string) (limit int64, source string, ok bool) {
	for ; ; dir = path.Dir(dir) {
		if dir == "/" {
			dir = ""
		}

		file := mount + dir + "/" + name
		if n, set := readCgroupLimit(fsys, file); set && (!ok || n < limit) {
			limit, source, ok = n, "/"+file, true
		}

		if dir == "" {
			return limit, source, ok
		}
	}
}

// readCgroupLimit is parseCgroupLimit of file below fsys; a file it cannot
// read sets no limit.
func readCgroupLimit(fsys *os.Root, file string) (int64, bool) {
	text, err := fsys.ReadFile(file)
	if err != nil {
		return 0, false
	}

	return parseCgroupLimit(string(text))
}

// ownCgroups returns the path of the process's cgroup in cgroup v2's hierarchy
// and in cgroup v1's memory controller, from the text of /proc/self/cgroup, each
// cleaned so that it is below the mount ("" for the root). A hierarchy the
// process is not in gives "".
func ownCgroups(proc string) (v2, v1 string) {
	for line := range strings.SplitSeq(proc, "\n") {
		parts := strings.SplitN(line, ":", 3) //nolint:mnd // hierarchy id, controllers, path
		if len(parts) != 3 {                  //nolint:mnd // see above
			continue
		}

		dir := path.Clean("/" + parts[2])
		if dir == "/" {
			dir = ""
		}

		switch {
		case parts[0] == "0" && parts[1] == "":
			v2 = dir
		case slices.Contains(strings.Split(parts[1], ","), "memory"):
			v1 = dir
		default:
		}
	}

	return v2, v1
}

// parseCgroupLimit reads the text of memory.max or memory.limit_in_bytes: a
// number of bytes, or max in cgroup v2. A limit of zero, below zero or too
// large to be one is not a limit.
func parseCgroupLimit(text string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || n <= 0 || n >= cgroupV1Unlimited {
		return 0, false
	}

	return n, true
}

// applyMemoryLimit is setMemoryLimit for this process, which also caps what
// the decode budget takes as the Go memory limit.
func applyMemoryLimit() {
	protocol.SetMemoryCap(setMemoryLimit(os.Getenv, "/", debug.SetMemoryLimit))
}
