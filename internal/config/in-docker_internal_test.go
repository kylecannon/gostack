package config

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func Test_dockerProbe(t *testing.T) {
	file := func(content string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(content)} }
	dir := &fstest.MapFile{Mode: fs.ModeDir | 0o755}
	tests := []struct {
		name         string
		giveFS       fstest.MapFS
		giveOverride string
		giveHostname string
		want         bool
	}{
		{name: "empty file system", giveFS: fstest.MapFS{}, want: false},
		{name: "override true wins", giveFS: fstest.MapFS{}, giveOverride: valueOne, want: true},
		{
			name:         "override false beats the marker file",
			giveFS:       fstest.MapFS{markerCommunity: file("")},
			giveOverride: valueFalse,
			want:         false,
		},
		{
			name:         "unrecognized override is ignored",
			giveFS:       fstest.MapFS{markerTmp: file("")},
			giveOverride: "maybe",
			want:         true,
		},
		{name: "community marker", giveFS: fstest.MapFS{markerCommunity: file("")}, want: true},
		{name: "pro marker", giveFS: fstest.MapFS{markerPro: file("")}, want: true},
		{name: "tmp marker", giveFS: fstest.MapFS{markerTmp: file("")}, want: true},
		{name: "dockerenv", giveFS: fstest.MapFS{".dockerenv": file("")}, want: true},
		{name: "containerenv", giveFS: fstest.MapFS{"run/.containerenv": file("")}, want: true},
		{
			name:   "marker path that is a directory does not count",
			giveFS: fstest.MapFS{"tmp/localstack/.marker/x": file("")},
			want:   false,
		},
		{name: "cgroup names docker", giveFS: fstest.MapFS{pathCgroup: file("0::/docker/abc\n")}, want: true},
		{name: "cgroup names buildkit", giveFS: fstest.MapFS{pathCgroup: file("0::/buildkit/abc\n")}, want: true},
		{
			name:         "cgroup names the hostname",
			giveFS:       fstest.MapFS{pathCgroup: file("0::/kubepods/c0ffee\n")},
			giveHostname: "c0ffee",
			want:         true,
		},
		{name: "cgroup names nothing", giveFS: fstest.MapFS{pathCgroup: file("0::/user.slice\n")}, want: false},
		{
			name: "docker host cgroup directories mean not a container",
			giveFS: fstest.MapFS{
				pathCgroup:                              file("0::/docker/abc\n"),
				"sys/fs/cgroup/memory/docker":           dir,
				"sys/fs/cgroup/memory/system.slice/x":   file(""),
				"sys/fs/cgroup/system.slice/docker-1.s": file(""),
			},
			want: false,
		},
		{
			name: "a missing system.slice abandons the host check",
			giveFS: fstest.MapFS{
				pathCgroup:                    file("0::/docker/abc\n"),
				"sys/fs/cgroup/memory/docker": dir,
			},
			want: true,
		},
		{
			name: "containerd root mount",
			giveFS: fstest.MapFS{
				pathCgroup: file("0::/\n"),
				"proc/mounts": file("# comment\n\nproc /proc proc rw 0 0\n" +
					"overlay / overlay rw,lowerdir=io.containerd.snapshotter.v1.overlayfs 0 0\n"),
			},
			want: true,
		},
		{
			name: "containerd option on another mount does not count",
			giveFS: fstest.MapFS{
				pathCgroup:    file("0::/\n"),
				"proc/mounts": file("overlay /var overlay rw,io.containerd 0 0\nbad line\n"),
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := dockerProbe{fsys: tt.giveFS, override: tt.giveOverride, hostname: tt.giveHostname}
			require.Equal(t, tt.want, probe.inDocker())
		})
	}
}
