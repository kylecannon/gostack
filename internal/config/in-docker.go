package config

import (
	"bufio"
	"io/fs"
	"strings"
)

const (
	markerCommunity = "usr/lib/localstack/.community-version"
	markerPro       = "usr/lib/localstack/.pro-version"
	markerTmp       = "tmp/localstack/.marker"
	pathCgroup      = "proc/1/cgroup"

	valueOne   = "1"
	valueZero  = "0"
	valueTrue  = "true"
	valueFalse = "false"
)

// dockerProbe answers LocalStack's in_docker() (config.py) against a file system
// rooted at "/", so tests can supply their own.
type dockerProbe struct {
	fsys fs.FS
	// override is the value of OVERRIDE_IN_DOCKER.
	override string
	// hostname is the machine's hostname, which a container's cgroup names.
	hostname string
}

// inDocker reports whether the process runs in a container. The first check that
// answers decides, in LocalStack's order: the override, the marker files the
// images create, the container runtime files, /proc/1/cgroup, and the root mount
// in /proc/mounts.
func (p dockerProbe) inDocker() bool {
	if answer, ok := parseBoolean(p.override); ok {
		return answer
	}
	for _, marker := range []string{
		markerCommunity,
		markerPro,
		markerTmp,
	} {
		info, err := fs.Stat(p.fsys, marker)
		if err == nil && !info.IsDir() {
			return true
		}
	}
	if p.exists(".dockerenv") || p.exists("run/.containerenv") {
		return true
	}
	cgroup, err := fs.ReadFile(p.fsys, pathCgroup)
	if err != nil {
		return false
	}
	if p.dockerHostCgroups() {
		return false
	}
	content := string(cgroup)
	if strings.Contains(content, "docker") || strings.Contains(content, "buildkit") {
		return true
	}
	if p.hostname != "" && strings.Contains(content, p.hostname) {
		return true
	}

	return p.rootMountIsContainerd()
}

func (p dockerProbe) exists(path string) bool {
	_, err := fs.Stat(p.fsys, path)

	return err == nil
}

// dockerHostCgroups reports whether the Docker host's own cgroup directories
// exist, which means this process runs on the host, not in a container.
// LocalStack evaluates all four checks inside one try block, so a missing
// system.slice directory abandons the whole test, and so does this.
func (p dockerProbe) dockerHostCgroups() bool {
	memoryDocker := p.exists("sys/fs/cgroup/memory/docker")
	memorySlice, err := p.anyNameContains("sys/fs/cgroup/memory/system.slice", "docker-")
	if err != nil {
		return false
	}
	cgroupDocker := p.exists("sys/fs/cgroup/docker")
	slice, err := p.anyNameContains("sys/fs/cgroup/system.slice", "docker-")
	if err != nil {
		return false
	}

	return memoryDocker || memorySlice || cgroupDocker || slice
}

func (p dockerProbe) anyNameContains(dir, substr string) (bool, error) {
	entries, err := fs.ReadDir(p.fsys, dir)
	if err != nil {
		return false, err //nolint:wrapcheck // only the failure itself matters to the caller
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), substr) {
			return true, nil
		}
	}

	return false, nil
}

// rootMountIsContainerd reports whether the root mount in /proc/mounts carries an
// io.containerd option, which is how containerd marks a container.
func (p dockerProbe) rootMountIsContainerd() bool {
	file, err := p.fsys.Open("proc/mounts")
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// fstab format: <spec> <mount point> <type> <options> <rest>...
		const minFields = 4
		fields := strings.Fields(line)
		if len(fields) < minFields || fields[1] != "/" {
			continue
		}
		if strings.Contains(fields[3], "io.containerd") {
			return true
		}
	}

	return false
}

// parseBoolean reads LocalStack's parse_boolean_env: "1" and "true" are true,
// "0" and "false" are false, and anything else is no answer.
func parseBoolean(value string) (answer, ok bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case valueOne, valueTrue:
		return true, true
	case valueZero, valueFalse:
		return false, true
	default:
		return false, false
	}
}
