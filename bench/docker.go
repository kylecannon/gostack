package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	dockerBin   = "docker"
	apiTimeout  = 30 * time.Second
	stopTimeout = 30 * time.Second
	readyLine   = "Ready."
	scanBuffer  = 1 << 20
	statsPath   = "/containers/%s/stats?stream=false&one-shot=true"
)

// errNotReady reports a container that exited or timed out before it logged
// the ready line.
var errNotReady = errors.New("container did not become ready")

// dockerOutput runs the docker CLI and returns its stdout. A failure carries
// the CLI's stderr.
func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, dockerBin, args...) //nolint:gosec // the arguments are built by this program
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		// What the command wrote before it failed comes back with the error: the load
		// generator prints how far a failed fill got.
		return out, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}

// dockerAPI reads the Docker Engine API for counters the CLI rounds away.
type dockerAPI struct {
	client *http.Client
	base   string
}

// engineInfo is the part of GET /info the report keeps.
type engineInfo struct {
	ServerVersion   string `json:"ServerVersion"`
	OperatingSystem string `json:"OperatingSystem"`
	KernelVersion   string `json:"KernelVersion"`
	Architecture    string `json:"Architecture"`
	OSType          string `json:"OSType"`
	NCPU            int    `json:"NCPU"`
	MemTotal        uint64 `json:"MemTotal"`
}

// containerStats is the part of GET /containers/<id>/stats the benchmark uses.
type containerStats struct {
	Read        time.Time   `json:"read"`
	CPUStats    cpuStats    `json:"cpu_stats"`
	MemoryStats memoryStats `json:"memory_stats"`
}

type cpuStats struct {
	CPUUsage cpuUsage `json:"cpu_usage"`
}

type cpuUsage struct {
	TotalUsage uint64 `json:"total_usage"`
}

type memoryStats struct {
	Stats map[string]uint64 `json:"stats"`
	Usage uint64            `json:"usage"`
	// MaxUsage is the high-water mark the daemon keeps where the kernel has one
	// (cgroup v1); cgroup v2 reports none.
	MaxUsage uint64 `json:"max_usage"`
}

// rss is the resident memory of the container: anonymous pages plus mapped
// file pages, which is what the processes hold in their resident sets. Page
// cache the kernel keeps for files that no process maps is left out, so a
// container that wrote a large log does not look big.
func (s containerStats) rss() uint64 {
	stats := s.MemoryStats.Stats
	if anon, ok := stats["anon"]; ok { // cgroup v2
		return anon + stats["file_mapped"]
	}

	if rss, ok := stats["rss"]; ok { // cgroup v1
		return rss + stats["mapped_file"]
	}

	return s.MemoryStats.Usage
}

// usage is the memory of the container as its cgroup counts it against the limit:
// anonymous pages and every page of the page cache, mapped or not. rss leaves the
// cache out, but a container that persists to disk fills it, and the kernel
// reclaims it only when the limit is near.
func (s containerStats) usage() uint64 {
	return max(s.MemoryStats.Usage, s.MemoryStats.MaxUsage)
}

// zero reports the counters a container that has stopped returns: nothing at
// all, which is no sample of its memory.
func (s containerStats) zero() bool {
	return s.rss() == 0 && s.usage() == 0
}

// newDockerAPI connects to the daemon the CLI uses: DOCKER_HOST, else the
// endpoint of the current docker context.
func newDockerAPI(ctx context.Context) (*dockerAPI, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		out, err := dockerOutput(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return nil, fmt.Errorf("find the docker endpoint: %w", err)
		}

		host = strings.TrimSpace(string(out))
	}

	endpoint, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse the docker endpoint %q: %w", host, err)
	}

	switch endpoint.Scheme {
	case "unix":
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer

			return dialer.DialContext(ctx, "unix", endpoint.Path)
		}

		return &dockerAPI{
			client: &http.Client{Transport: &http.Transport{DialContext: dial}},
			base:   "http://docker", //nolint:revive // the daemon socket is a local unix socket; the host name is a placeholder
		}, nil
	case "tcp":
		return &dockerAPI{client: &http.Client{}, base: "http://" + endpoint.Host}, nil
	default:
		return nil, fmt.Errorf("docker endpoint %q: scheme %q is not supported, want unix or tcp", host, endpoint.Scheme)
	}
}

func (a *dockerAPI) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %s", path, resp.Status)
	}

	err = json.NewDecoder(resp.Body).Decode(out)
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}

func (a *dockerAPI) info(ctx context.Context) (engineInfo, error) {
	var info engineInfo
	err := a.get(ctx, "/info", &info)

	return info, err
}

func (a *dockerAPI) stats(ctx context.Context, container string) (containerStats, error) {
	var stats containerStats
	err := a.get(ctx, fmt.Sprintf(statsPath, container), &stats)

	return stats, err
}

// imageBytes is the size of an image as the daemon stores it.
func imageBytes(ctx context.Context, image string) (uint64, error) {
	out, err := dockerOutput(ctx, "image", "inspect", "--format", "{{.Size}}", image)
	if err != nil {
		return 0, err
	}

	size, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse the size of %s: %w", image, err)
	}

	return size, nil
}

// instance is one target container, started with an attached `docker run` so
// its log can be watched for the ready line.
type instance struct {
	name    string
	ready   chan time.Duration
	exited  chan struct{}
	scanned chan struct{}
	waitErr error
}

// launch starts a container and returns at once. The ready channel receives the
// time from the start of `docker run` to the ready line.
func launch(ctx context.Context, name, image string, extra ...string) (*instance, error) {
	args := append([]string{cmdRun, "--name", name, "-e", "DISABLE_EVENTS=1"}, extra...)
	//nolint:gosec // the arguments are built by this program
	cmd := exec.CommandContext(ctx, dockerBin, append(args, image)...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	inst := &instance{
		name:    name,
		ready:   make(chan time.Duration, 1),
		exited:  make(chan struct{}),
		scanned: make(chan struct{}),
	}

	started := time.Now()
	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %w", image, err)
	}

	go inst.scan(pr, started)
	go func() {
		inst.waitErr = cmd.Wait()
		_ = pw.Close()

		close(inst.exited)
	}()

	return inst, nil
}

// scan reads the container's output until it closes. It keeps reading after the
// ready line so the container never blocks on a full pipe.
func (i *instance) scan(r io.Reader, started time.Time) {
	defer close(i.scanned)

	sent := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, scanBuffer), scanBuffer)

	for scanner.Scan() {
		if !sent && strings.TrimSpace(scanner.Text()) == readyLine {
			i.ready <- time.Since(started)
			sent = true
		}
	}

	_, _ = io.Copy(io.Discard, r)
}

// awaitReady waits for the ready line.
func (i *instance) awaitReady(ctx context.Context, timeout time.Duration) (time.Duration, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case took := <-i.ready:
		return took, nil
	case <-i.exited:
		return 0, fmt.Errorf("%w: %s exited first: %w", errNotReady, i.name, i.waitErr)
	case <-timer.C:
		return 0, fmt.Errorf("%w: %s after %s", errNotReady, i.name, timeout)
	case <-ctx.Done():
		return 0, fmt.Errorf("wait for %s: %w", i.name, ctx.Err())
	}
}

// stop removes the container and waits for the attached docker run to end. It
// runs after a cancelled context too, so an interrupt leaves no container.
func (i *instance) stop(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	_, _ = dockerOutput(ctx, removeArgs(i.name)...)

	select {
	case <-i.exited:
	case <-ctx.Done():
	}

	select {
	case <-i.scanned:
	case <-ctx.Done():
	}
}

// removeContainer removes a container by name and ignores a failure, for
// cleanup of a container that may not exist.
func removeContainer(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	_, _ = dockerOutput(ctx, removeArgs(name)...)
}

// removeArgs is the `docker rm` that ends a container: forced, and with -v so the
// anonymous volume the image declares goes with it. Without -v every target
// container leaves one volume behind (230 in one quick run).
func removeArgs(name string) []string {
	return []string{"rm", "-f", "-v", name}
}

// errTargetDied reports a target container that was not running when a run of
// a measurement ended: it exited, or was removed, during the run.
var errTargetDied = errors.New("the target container died during the run")

// errOOMKilled reports a target container the kernel killed for going over its
// memory limit. It wraps errTargetDied, so a caller that does not tell the two
// apart treats it as any other death.
var errOOMKilled = fmt.Errorf("%w: it was OOM-killed", errTargetDied)

const (
	// oomSettle is how long a run that failed waits for its container to stop before
	// it reads the container's state. The load generator fails the moment the
	// connection drops, which can be before the daemon has recorded why the
	// container stopped.
	oomSettle = 5 * time.Second
	oomPoll   = 250 * time.Millisecond

	// exitKilled is the exit status of a process the kernel ended with SIGKILL, as
	// its out-of-memory killer does.
	exitKilled = "137"
	// oomFlag is State.OOMKilled of a container the kernel killed.
	oomFlag = "true"

	// stateFields are the words of a state line: status, exit code, OOM-killed.
	stateFields = 3
)

// state is the container's status, exit code and OOM-killed flag, as
// `docker inspect` prints them.
func (i *instance) state(ctx context.Context) (string, error) {
	out, err := dockerOutput(ctx, "inspect", "--format",
		"{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}}", i.name)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", errTargetDied, i.name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// alive returns errTargetDied unless the container is still running, and
// errOOMKilled when the kernel's out-of-memory killer ended it. A run against a
// target that went away has numbers from before and errors from after it went,
// so the run is not a measurement of the image.
func (i *instance) alive(ctx context.Context) error {
	return i.aliveWithin(ctx, 0)
}

// aliveWithin is alive for a run that failed: a container that is still running
// is given up to wait to stop first, as the daemon records the death a moment
// after the connections drop.
func (i *instance) aliveWithin(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)

	for {
		state, err := i.state(ctx)
		if err != nil {
			return err
		}

		if !strings.HasPrefix(state, "running ") || !time.Now().Before(deadline) {
			return targetState(i.name, state)
		}

		err = sleep(ctx, oomPoll)
		if err != nil {
			return err
		}
	}
}

// targetState reads a container's state line: nil for a running one, errOOMKilled
// for one the kernel's out-of-memory killer ended, errTargetDied for any other
// that stopped. The flag alone is not enough: Docker sets it when any process of
// the container was killed, a healthcheck's among them, so a later exit of the
// container for another reason would read as one. The kill of the container's own
// process also has exit code 137.
func targetState(name, state string) error {
	fields := strings.Fields(state)
	if len(fields) == stateFields && fields[0] == "running" {
		return nil
	}

	if len(fields) == stateFields && fields[1] == exitKilled && fields[2] == oomFlag {
		return fmt.Errorf("%w: %s is %s (status, exit code, OOM-killed)", errOOMKilled, name, state)
	}

	return fmt.Errorf("%w: %s is %s (status, exit code, OOM-killed)", errTargetDied, name, state)
}
