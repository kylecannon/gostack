package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	readyTimeout      = 5 * time.Minute
	sampleEvery       = 100 * time.Millisecond
	loadgenSlack      = 2 * time.Minute
	agenticTimeout    = 10 * time.Minute
	loadgenDockerfile = "FROM scratch\nCOPY bench /bench\nENTRYPOINT [\"/bench\"]\n"
	dateLayout        = "2006-01-02"
	defaultStartups   = 5
	defaultSettle     = 10 * time.Second
	half              = 2
)

// target is one image to measure.
type target struct{ name, image string }

// options are the settings of `bench run`.
type options struct {
	targets          []target
	outDir           string
	loadgenImage     string
	suffix           string
	startupRuns      int
	settle           time.Duration
	idle             time.Duration
	duration         time.Duration
	concurrencies    []int
	cycles           int
	cycleConcurrency int
}

func parseOptions(args []string) (options, error) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	gostack := flags.String("gostack-image", "gostack:dev", "the full gostack image")
	slim := flags.String("slim-image", "gostack:dev-slim", "the slim gostack image")
	localstack := flags.String("localstack-image", "gostack-parity-localstack:8b9a79f",
		"the LocalStack image the parity harness builds")
	only := flags.String("only", "", "comma separated targets to measure: gostack, gostack-slim, localstack (default all)")
	concurrency := flags.String("concurrency", "1,16,64", "comma separated SQS latency concurrency levels")

	var opts options

	flags.StringVar(&opts.loadgenImage, "loadgen-image", "gostack-bench-loadgen:dev",
		"tag for the load generator image the run builds")
	flags.StringVar(&opts.outDir, "out", "bench/results", "directory for the result files")
	flags.StringVar(&opts.suffix, "name-suffix", "", "appended to container names, to keep parallel runs apart")
	flags.IntVar(&opts.startupRuns, "startup-runs", defaultStartups, "containers started to take the median startup time")
	flags.DurationVar(&opts.settle, "settle", defaultSettle, "wait after ready before the idle window")
	flags.DurationVar(&opts.idle, "idle", time.Minute, "idle window")
	flags.DurationVar(&opts.duration, "duration", defaultLoadDuration, "length of each SQS latency run")
	flags.IntVar(&opts.cycles, "cycles", defaultCycles, "agentic cycles")
	flags.IntVar(&opts.cycleConcurrency, "cycle-concurrency", defaultCycleConc, "agentic concurrency")

	err := flags.Parse(args)
	if err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}

	all := []target{{"gostack", *gostack}, {"gostack-slim", *slim}, {"localstack", *localstack}}
	for _, t := range all {
		if *only == "" || slices.Contains(strings.Split(*only, ","), t.name) {
			opts.targets = append(opts.targets, t)
		}
	}

	if len(opts.targets) == 0 {
		return options{}, fmt.Errorf("-only %q names no target", *only)
	}

	for field := range strings.SplitSeq(*concurrency, ",") {
		level, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || level < 1 {
			return options{}, fmt.Errorf("-concurrency %q: %q is not a positive integer", *concurrency, field)
		}

		opts.concurrencies = append(opts.concurrencies, level)
	}

	return opts, nil
}

// runner holds what one `bench run` shares between its measurements.
type runner struct {
	opts options
	api  *dockerAPI
	log  io.Writer
	seq  atomic.Int64
}

// orchestrate is `bench run`.
func orchestrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}

	api, err := newDockerAPI(ctx)
	if err != nil {
		return err
	}

	run := &runner{opts: opts, api: api, log: stderr}

	rep, err := run.measureAll(ctx)
	if err != nil {
		return err
	}

	path, err := writeReport(opts.outDir, rep)
	if err != nil {
		return err
	}

	_, err = io.WriteString(stdout, rep.markdown()+"\nresults: "+path+".json\n")
	if err != nil {
		return fmt.Errorf("write summary: %w", err)
	}

	return nil
}

func (r *runner) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.log, format+"\n", args...)
}

// name returns a container name no other run uses.
func (r *runner) name(kind string) string {
	return fmt.Sprintf("gostack-bench-%s-%d-%d%s", kind, os.Getpid(), r.seq.Add(1), r.opts.suffix)
}

func (r *runner) measureAll(ctx context.Context) (report, error) {
	info, err := r.api.info(ctx)
	if err != nil {
		return report{}, err
	}

	commit, dirty, err := gitState(ctx)
	if err != nil {
		return report{}, err
	}

	err = r.buildLoadgen(ctx)
	if err != nil {
		return report{}, err
	}

	defer func() { _, _ = dockerOutput(context.WithoutCancel(ctx), "rmi", "-f", r.opts.loadgenImage) }()

	rep := report{
		Date:   time.Now().UTC().Format(dateLayout),
		Commit: commit,
		Dirty:  dirty,
		Host: hostInfo{
			Docker: info.ServerVersion, OS: info.OperatingSystem, Kernel: info.KernelVersion,
			Arch: info.Architecture, CPUs: info.NCPU, MemoryBytes: info.MemTotal,
		},
		Settings: settings{
			StartupRuns: r.opts.startupRuns, SettleSeconds: r.opts.settle.Seconds(), IdleSeconds: r.opts.idle.Seconds(),
			LoadSeconds: r.opts.duration.Seconds(), Concurrencies: r.opts.concurrencies,
			Cycles: r.opts.cycles, CycleConcurrency: r.opts.cycleConcurrency,
		},
	}

	for _, t := range r.opts.targets {
		err := ctx.Err()
		if err != nil {
			return rep, fmt.Errorf("interrupted: %w", err)
		}

		rep.Targets = append(rep.Targets, r.measureTarget(ctx, t))
	}

	return rep, nil
}

// buildLoadgen cross-compiles this program for the daemon's architecture and
// wraps it in a scratch image.
func (r *runner) buildLoadgen(ctx context.Context) error {
	arch, err := dockerOutput(ctx, "version", "--format", "{{.Server.Arch}}")
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "gostack-bench-loadgen-")
	if err != nil {
		return fmt.Errorf("create build context: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	//nolint:gosec // the arguments are built by this program
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(dir, "bench"), "./bench")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)))

	out, err := build.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build the load generator: %w: %s", err, strings.TrimSpace(string(out)))
	}

	err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(loadgenDockerfile), fileMode)
	if err != nil {
		return fmt.Errorf("write the load generator Dockerfile: %w", err)
	}

	_, err = dockerOutput(ctx, "build", "-q", "-t", r.opts.loadgenImage, dir)

	return err
}

// measureTarget runs every measurement against one image. A measurement that
// fails is recorded as a problem and the rest still run.
func (r *runner) measureTarget(ctx context.Context, t target) targetResult {
	res := targetResult{Name: t.name, Image: t.image}
	r.logf("== %s (%s)", t.name, t.image)

	size, err := imageBytes(ctx, t.image)
	res.problem("image size", err)

	res.ImageBytes = size

	startup, inst, err := r.startup(ctx, t)
	res.Startup = startup

	res.problem("startup", err)

	if inst != nil {
		r.measureRunning(ctx, inst, &res)
		inst.stop(ctx)
	}

	agentic, err := r.agentic(ctx, t)
	res.Agentic = agentic

	res.problem("agentic workload", err)

	return res
}

// measureRunning takes the idle and latency measurements on a started container.
func (r *runner) measureRunning(ctx context.Context, inst *instance, res *targetResult) {
	var err error

	res.Idle, err = r.idle(ctx, inst)
	res.problem("idle", err)

	res.Latency, err = r.latency(ctx, inst)
	res.problem("latency", err)
}

// start launches a container of the target and waits for the ready line.
func (r *runner) start(ctx context.Context, t target) (*instance, time.Duration, error) {
	inst, err := launch(ctx, r.name(t.name), t.image)
	if err != nil {
		return nil, 0, err
	}

	took, err := inst.awaitReady(ctx, readyTimeout)
	if err != nil {
		inst.stop(ctx)

		return nil, 0, err
	}

	return inst, took, nil
}

// startup times startupRuns containers and returns the last one still running,
// for the idle and latency measurements.
func (r *runner) startup(ctx context.Context, t target) (startupResult, *instance, error) {
	var (
		res  startupResult
		prev *instance
	)

	for i := range r.opts.startupRuns {
		if prev != nil {
			prev.stop(ctx)
		}

		inst, took, err := r.start(ctx, t)
		if err != nil {
			return res, nil, fmt.Errorf("run %d: %w", i+1, err)
		}

		r.logf("%s: ready in %s (run %d of %d)", t.name, took.Round(time.Millisecond), i+1, r.opts.startupRuns)
		res.SamplesMS = append(res.SamplesMS, float64(took.Microseconds())/microsPerMilli)
		prev = inst
	}

	res.MedianMS = median(res.SamplesMS)

	return res, prev, nil
}

func (r *runner) idle(ctx context.Context, inst *instance) (idleResult, error) {
	r.logf("%s: settling %s, then idle for %s", inst.name, r.opts.settle, r.opts.idle)

	err := sleep(ctx, r.opts.settle)
	if err != nil {
		return idleResult{}, err
	}

	used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error { return sleep(ctx, r.opts.idle) })

	return idleResult{SettleSeconds: r.opts.settle.Seconds(), Seconds: r.opts.idle.Seconds(), Usage: used}, err
}

// latency runs the SQS latency workload at each concurrency level.
func (r *runner) latency(ctx context.Context, inst *instance) ([]latencyRun, error) {
	var runs []latencyRun

	for _, level := range r.opts.concurrencies {
		r.logf("%s: SQS latency at concurrency %d for %s", inst.name, level, r.opts.duration)

		var run latencyRun

		used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error {
			return r.loadgen(ctx, inst.name, r.opts.duration+loadgenSlack, &run,
				"sqs", "-concurrency", strconv.Itoa(level), "-duration", r.opts.duration.String())
		})
		if err != nil {
			return runs, fmt.Errorf("concurrency %d: %w", level, err)
		}

		run.Usage = used
		runs = append(runs, run)
	}

	return runs, nil
}

// agentic runs the create, use and delete workload on a container of its own,
// so the peak memory is the workload's and not left over from the latency runs.
func (r *runner) agentic(ctx context.Context, t target) (agenticRun, error) {
	inst, _, err := r.start(ctx, t)
	if err != nil {
		return agenticRun{}, err
	}

	defer inst.stop(ctx)

	r.logf("%s: %d agentic cycles at concurrency %d", inst.name, r.opts.cycles, r.opts.cycleConcurrency)

	var run agenticRun

	used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error {
		return r.loadgen(ctx, inst.name, agenticTimeout, &run,
			"agentic", "-cycles", strconv.Itoa(r.opts.cycles), "-concurrency", strconv.Itoa(r.opts.cycleConcurrency))
	})
	run.Usage = used

	return run, err
}

// loadgen runs the load generator container in the target's network namespace
// and decodes the JSON it prints.
func (r *runner) loadgen(ctx context.Context, targetName string, timeout time.Duration, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name := r.name("loadgen")
	defer removeContainer(ctx, name)

	cmd := []string{"run", "--rm", "--name", name, "--network", "container:" + targetName, r.opts.loadgenImage, "loadgen"}

	raw, err := dockerOutput(ctx, append(cmd, args...)...)
	if err != nil {
		return err
	}

	err = json.Unmarshal(raw, out)
	if err != nil {
		return fmt.Errorf("decode the load generator output %q: %w", raw, err)
	}

	return nil
}

// sampled runs work while it reads the container's counters: once before, every
// sampleEvery during, once after. It returns the memory and CPU of the window.
func (r *runner) sampled(ctx context.Context, container string, work func(context.Context) error) (usage, error) {
	first, err := r.api.stats(ctx, container)
	if err != nil {
		return usage{}, fmt.Errorf("read stats: %w", err)
	}

	var peak atomic.Uint64

	peak.Store(first.rss())

	sampleCtx, stop := context.WithCancel(ctx)
	defer stop()

	var wg sync.WaitGroup

	wg.Go(func() {
		ticker := time.NewTicker(sampleEvery)
		defer ticker.Stop()

		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				now, statsErr := r.api.stats(sampleCtx, container)
				if statsErr == nil {
					peak.Store(max(peak.Load(), now.rss()))
				}
			}
		}
	})

	workErr := work(ctx)

	stop()
	wg.Wait()

	last, err := r.api.stats(ctx, container)
	if err != nil {
		return usage{}, errors.Join(workErr, fmt.Errorf("read stats: %w", err))
	}

	peak.Store(max(peak.Load(), last.rss()))

	used := usage{BaselineRSSBytes: first.rss(), EndRSSBytes: last.rss(), PeakRSSBytes: peak.Load()}
	if wall := last.Read.Sub(first.Read); wall > 0 {
		cpuNanos := last.CPUStats.CPUUsage.TotalUsage - first.CPUStats.CPUUsage.TotalUsage
		used.CPUPercent = float64(cpuNanos) / float64(wall.Nanoseconds()) * percentScale
	}

	return used, workErr
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("interrupted: %w", ctx.Err())
	}
}

// median returns the middle value, or the mean of the two middle values.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := slices.Sorted(slices.Values(values))
	mid := len(sorted) / half

	if len(sorted)%half == 1 {
		return sorted[mid]
	}

	return (sorted[mid-1] + sorted[mid]) / half
}

// gitState returns the short commit the benchmark ran on and whether the tree,
// results aside, had uncommitted changes.
func gitState(ctx context.Context) (string, bool, error) {
	sha, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", false, fmt.Errorf("read the commit: %w", err)
	}

	status, err := exec.CommandContext(ctx, "git", "status", "--porcelain", "--", ".", ":(exclude)bench/results").Output()
	if err != nil {
		return "", false, fmt.Errorf("read the tree state: %w", err)
	}

	return strings.TrimSpace(string(sha)), len(strings.TrimSpace(string(status))) > 0, nil
}
