package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cgroupRoot is a file system root holding files, by path below it.
func cgroupRoot(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, text := range files {
		full := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(text), 0o600))
	}

	return root
}

const (
	container512 = "536870912\n"
	container1Gi = "1073741824\n"
	procCgroup   = "proc/self/cgroup"
	rootFile     = "sys/fs/cgroup/memory.max"
	rootMemory   = "/sys/fs/cgroup/memory.max"
)

// Test_cgroupMemoryLimit reads the limit a container runs under, in cgroup v2
// and v1, and reports none when there is no limit to read.
func Test_cgroupMemoryLimit(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		wantLimit  int64
		wantSource string
		wantOK     bool
	}{
		{
			name:       "v2 at the mount's root, as in a container with a cgroup namespace",
			files:      map[string]string{rootFile: container512, procCgroup: "0::/\n"},
			wantLimit:  512 << 20,
			wantSource: rootMemory,
			wantOK:     true,
		},
		{
			name:       "v2 without a proc file",
			files:      map[string]string{rootFile: container512},
			wantLimit:  512 << 20,
			wantSource: rootMemory,
			wantOK:     true,
		},
		{
			name: "v2 in the process's own cgroup below the mount",
			files: map[string]string{
				"sys/fs/cgroup/system.slice/gostack.service/memory.max": "268435456\n",
				procCgroup: "0::/system.slice/gostack.service\n",
			},
			wantLimit:  256 << 20,
			wantSource: "/sys/fs/cgroup/system.slice/gostack.service/memory.max",
			wantOK:     true,
		},
		{
			name:   "v2 max is no limit",
			files:  map[string]string{rootFile: unlimited, procCgroup: "0::/\n"},
			wantOK: false,
		},
		{
			name: "v2 max in the process's own cgroup passes to the mount's root",
			files: map[string]string{
				"sys/fs/cgroup/system.slice/gostack.service/memory.max": unlimited,
				rootFile:   container512,
				procCgroup: "0::/system.slice/gostack.service\n",
			},
			wantLimit:  512 << 20,
			wantSource: rootMemory,
			wantOK:     true,
		},
		{
			name: "v2 limit of a cgroup between the process's own and the mount's root",
			files: map[string]string{
				"sys/fs/cgroup/kubepods/pod1/memory.max":         container1Gi,
				"sys/fs/cgroup/kubepods/pod1/gostack/memory.max": unlimited,
				"sys/fs/cgroup/kubepods/memory.max":              unlimited,
				procCgroup:                                       "0::/kubepods/pod1/gostack\n",
			},
			wantLimit:  1 << 30,
			wantSource: "/sys/fs/cgroup/kubepods/pod1/memory.max",
			wantOK:     true,
		},
		{
			name: "v2 the smallest of the limits above the process holds it",
			files: map[string]string{
				"sys/fs/cgroup/kubepods/pod1/gostack/memory.max": container1Gi,
				"sys/fs/cgroup/kubepods/pod1/memory.max":         container512,
				rootFile:                                         "2147483648\n",
				procCgroup:                                       "0::/kubepods/pod1/gostack\n",
			},
			wantLimit:  512 << 20,
			wantSource: "/sys/fs/cgroup/kubepods/pod1/memory.max",
			wantOK:     true,
		},
		{
			name:       "v1",
			files:      map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": container512},
			wantLimit:  512 << 20,
			wantSource: "/sys/fs/cgroup/memory/memory.limit_in_bytes",
			wantOK:     true,
		},
		{
			name: "v1 in the process's own cgroup",
			files: map[string]string{
				"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": container1Gi,
				procCgroup: "12:cpu,cpuacct:/docker/abc\n4:memory:/docker/abc\n",
			},
			wantLimit:  1 << 30,
			wantSource: "/sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes",
			wantOK:     true,
		},
		{
			name:   "v1 reports no limit as the largest page-aligned int64",
			files:  map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"},
			wantOK: false,
		},
		{
			name:   "no cgroup files",
			files:  map[string]string{},
			wantOK: false,
		},
		{
			name:   "text that is not a number",
			files:  map[string]string{rootFile: "lots\n"},
			wantOK: false,
		},
		{
			name:   "zero is not a limit",
			files:  map[string]string{rootFile: "0\n"},
			wantOK: false,
		},
		{
			name: "a path that climbs out of the mount stays below it",
			files: map[string]string{
				rootFile:   container512,
				procCgroup: "0::/../../etc\n",
			},
			// The cleaned path is /etc, which has no file, so the mount's own
			// answers.
			wantLimit:  512 << 20,
			wantSource: rootMemory,
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, source, ok := cgroupMemoryLimit(cgroupRoot(t, tt.files))

			assert.Equal(t, tt.wantOK, ok)

			if tt.wantOK {
				assert.Equal(t, tt.wantLimit, limit)
				assert.Equal(t, tt.wantSource, source)
			}
		})
	}
}

const (
	meminfo    = "proc/meminfo"
	unlimited  = "max\n"
	goMemLimit = "GOMEMLIMIT"
)

// Test_machineMemory reads MemTotal from /proc/meminfo, and reports none when
// the file is missing (darwin) or names no MemTotal.
func Test_machineMemory(t *testing.T) {
	memory, source, ok := machineMemory(cgroupRoot(t, map[string]string{
		meminfo: "MemTotal:        8034212 kB\nMemFree:         1234567 kB\n",
	}))
	require.True(t, ok)
	assert.Equal(t, int64(8034212)<<10, memory)
	assert.Equal(t, "/"+meminfo, source)

	_, _, ok = machineMemory(cgroupRoot(t, nil))
	assert.False(t, ok, "no /proc/meminfo")

	_, _, ok = machineMemory(cgroupRoot(t, map[string]string{meminfo: "MemFree: 1 kB\n"}))
	assert.False(t, ok, "no MemTotal")

	_, _, ok = machineMemory(cgroupRoot(t, map[string]string{meminfo: "MemTotal: lots kB\n"}))
	assert.False(t, ok, "MemTotal is no number")
}

// Test_setMemoryLimit sets 85% of the container's limit, or else of the
// machine's memory, once, unless the user set GOMEMLIMIT, whatever its value;
// it returns that 85% as the decode budget's memory cap either way, so a
// GOMEMLIMIT above the container's limit does not raise the decode budget's
// room past it.
func Test_setMemoryLimit(t *testing.T) {
	const (
		soft512   = 512 << 20 / 100 * 85
		machine   = "MemTotal:        2097152 kB\n" // 2 GiB
		softTwoGi = 2 << 30 / 100 * 85
	)

	root := cgroupRoot(t, map[string]string{rootFile: container512, meminfo: machine})

	tests := []struct {
		name    string
		env     map[string]string
		root    string
		want    int64
		wantSet bool
	}{
		{"a container limit", nil, root, soft512, true},
		{"GOMEMLIMIT wins", map[string]string{goMemLimit: "300MiB"}, root, soft512, false},
		{"GOMEMLIMIT above the container limit", map[string]string{goMemLimit: "4GiB"}, root, soft512, false},
		{"GOMEMLIMIT=off wins too", map[string]string{goMemLimit: "off"}, root, soft512, false},
		{"no container limit: the machine's memory", nil, cgroupRoot(t, map[string]string{
			rootFile: unlimited, meminfo: machine,
		}), softTwoGi, true},
		{"a container limit above the machine's memory: the machine's", nil, cgroupRoot(t, map[string]string{
			rootFile: "68719476736\n", meminfo: machine, // 64 GiB on a 2 GiB machine
		}), softTwoGi, true},
		{"a container limit and no machine memory read", nil, cgroupRoot(t, map[string]string{
			rootFile: container512,
		}), soft512, true},
		{"no container limit and no machine memory read", nil, cgroupRoot(t, nil), 0, false},
		// 85% of 99 bytes rounds to 0, which the runtime would take as a limit
		// of nothing.
		{"a limit too small to keep", nil, cgroupRoot(t, map[string]string{rootFile: "99\n"}), 0, false},
		{"the smallest limit kept", nil, cgroupRoot(t, map[string]string{rootFile: "100\n"}), 85, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []int64

			memoryCap := setMemoryLimit(func(k string) string { return tt.env[k] }, tt.root, func(n int64) int64 {
				calls = append(calls, n)

				return math.MaxInt64
			})

			assert.Equal(t, tt.want, memoryCap, "the decode budget's memory cap")

			if !tt.wantSet {
				assert.Empty(t, calls)

				return
			}

			assert.Equal(t, []int64{tt.want}, calls)
		})
	}
}
