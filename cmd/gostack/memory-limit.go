package main

import (
	"log/slog"
	"os"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

const (
	// memoryLimitPercent is how much of a container's memory limit becomes the
	// Go runtime's soft limit (debug.SetMemoryLimit). The runtime counts what
	// it has mapped for heap, stacks and its own tables; the container also
	// counts the binary, the page cache and the services' tmpfs, so the rest is
	// left to them. docs/architecture.md, "Memory under a container limit",
	// has the measurements the number comes from.
	memoryLimitPercent = 85

	// cgroupV1Unlimited is where cgroup v1 reports a limit that is not set:
	// the largest page-aligned int64, 2^63 less a page. Any limit past 2^60 is
	// not a limit anyone set.
	cgroupV1Unlimited = 1 << 60

	percent = 100
)

// setMemoryLimit gives the Go runtime a soft memory limit of
// memoryLimitPercent of the container's, when the process runs under a cgroup
// memory limit and GOMEMLIMIT is not set, and logs it once. The collector then
// works harder as the heap nears the limit, in place of letting the container's
// OOM killer end the process with a request half decoded. A GOMEMLIMIT in the
// environment, whatever its value (off included), is the user's and is left as
// the runtime read it. root is the file system root the cgroup files are read
// below; set is debug.SetMemoryLimit.
func setMemoryLimit(getenv func(string) string, root string, set func(int64) int64) {
	if getenv("GOMEMLIMIT") != "" {
		return
	}

	limit, source, ok := cgroupMemoryLimit(root)
	if !ok {
		return
	}

	// A limit under percent bytes would round to zero, which the runtime
	// takes as a limit of nothing; it is no limit anyone could run under.
	soft := limit / percent * memoryLimitPercent
	if soft <= 0 {
		return
	}

	set(soft)

	slog.Info("gostack set the Go memory limit from the container's memory limit",
		"memory_limit", soft, "container_limit", limit, "percent", memoryLimitPercent, "source", source)
}

// cgroupMemoryLimit returns the memory limit in bytes of the cgroup this
// process runs in, read below root, and the file it came from. cgroup v2's
// memory.max is looked for in the process's own cgroup first
// (/proc/self/cgroup) and then at the mount's root, which is the container's
// own cgroup when it has a namespace of its own; cgroup v1's
// memory.limit_in_bytes the same way. The first of them that sets a limit
// gives it: a file that says max, or no limit, or is missing, passes to the
// next. It reports false when none sets one. The limits of the cgroups between
// the process's own and the mount's root are not read.
func cgroupMemoryLimit(root string) (limit int64, source string, ok bool) {
	fsys, err := os.OpenRoot(root)
	if err != nil {
		return 0, "", false
	}

	defer func() { _ = fsys.Close() }()

	own, _ := fsys.ReadFile("proc/self/cgroup")
	v2, v1 := ownCgroups(string(own))

	candidates := []string{
		"sys/fs/cgroup" + v2 + "/memory.max",
		"sys/fs/cgroup/memory.max",
		"sys/fs/cgroup/memory" + v1 + "/memory.limit_in_bytes",
		"sys/fs/cgroup/memory/memory.limit_in_bytes",
	}

	for _, file := range candidates {
		text, err := fsys.ReadFile(file)
		if err != nil {
			continue
		}

		limit, ok = parseCgroupLimit(string(text))
		if ok {
			return limit, "/" + file, true
		}
	}

	return 0, "", false
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

// applyMemoryLimit is setMemoryLimit for this process.
func applyMemoryLimit() {
	setMemoryLimit(os.Getenv, "/", debug.SetMemoryLimit)
}
