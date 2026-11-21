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
			files:  map[string]string{rootFile: "max\n", procCgroup: "0::/\n"},
			wantOK: false,
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
				"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": "1073741824\n",
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

// Test_setMemoryLimit sets 85% of the container's limit, once, unless the user
// set GOMEMLIMIT, whatever its value.
func Test_setMemoryLimit(t *testing.T) {
	root := cgroupRoot(t, map[string]string{rootFile: container512})

	tests := []struct {
		name    string
		env     map[string]string
		root    string
		want    int64
		wantSet bool
	}{
		{"a container limit", nil, root, 512 << 20 / 100 * 85, true},
		{"GOMEMLIMIT wins", map[string]string{"GOMEMLIMIT": "300MiB"}, root, 0, false},
		{"GOMEMLIMIT=off wins too", map[string]string{"GOMEMLIMIT": "off"}, root, 0, false},
		{"no container limit", nil, cgroupRoot(t, nil), 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []int64

			setMemoryLimit(func(k string) string { return tt.env[k] }, tt.root, func(n int64) int64 {
				calls = append(calls, n)

				return math.MaxInt64
			})

			if !tt.wantSet {
				assert.Empty(t, calls)

				return
			}

			assert.Equal(t, []int64{tt.want}, calls)
		})
	}
}
