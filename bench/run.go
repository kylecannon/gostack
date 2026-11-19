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
	defaultIdleRuns   = 3
	defaultReps       = 3
	half              = 2

	// Idle windows by profile. The quick profile only has to see RSS settle.
	fullSettle  = 5 * time.Second
	fullIdle    = 30 * time.Second
	quickSettle = 3 * time.Second
	quickIdle   = 8 * time.Second

	persistDir = "/persisted-data"

	// loadgenCellFlags are the arguments of `loadgen cell` a cell always has: four
	// flags with a value each.
	loadgenCellFlags = 8
	// argsPerOption is "-opt" and its key=value.
	argsPerOption = 2

	targetGostack    = "gostack"
	targetSlim       = "gostack-slim"
	targetLocalstack = "localstack"

	variantHealthcheck   = "healthcheck"
	variantNoHealthcheck = "no-healthcheck"
)

// target is one image to measure. Only a target that persists runs the cells
// that set a persistence variable, such as PERSIST_SQS=1.
type target struct {
	name, image string
	persistable bool
	// side is sideBase or sideHead when the run measures two images of one target
	// in alternation (-base-image), and empty otherwise.
	side string
}

const (
	sideBase = "base"
	sideHead = "head"
)

// label names the target in the log and in container names.
func (t target) label() string {
	if t.side == "" {
		return t.name
	}

	return t.name + "-" + t.side
}

// options are the settings of `bench run`.
type options struct {
	profile          string
	services         []string
	notes            []string
	targets          []target
	outDir           string
	compareDir       string
	accepted         string
	loadgenImage     string
	suffix           string
	commit           string
	baseImage        string
	baseCommit       string
	baseOut          string
	startupRuns      int
	idleRuns         int
	reps             int
	settle           time.Duration
	idle             time.Duration
	duration         time.Duration
	cycles           int
	cycleConcurrency int
}

func parseOptions(args []string) (options, error) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	gostack := flags.String("gostack-image", "gostack:dev", "the full gostack image")
	slim := flags.String("slim-image", "gostack:dev-slim", "the slim gostack image")
	localstack := flags.String("localstack-image", "gostack-parity-localstack:8b9a79f",
		"the LocalStack image the parity harness builds")
	targets := flags.String("targets", "", "comma separated targets: gostack, gostack-slim, localstack "+
		"(default: all for the full profile, gostack for the quick one)")

	services := flags.String("services", "", "comma separated services whose cells and agentic cycle run (default: all)")

	var opts options

	opts.bind(flags)

	err := flags.Parse(args)
	if err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}

	err = opts.check()
	if err != nil {
		return options{}, err
	}

	opts.applyProfileDefaults()

	opts.services, err = parseServices(*services)
	if err != nil {
		return options{}, err
	}

	if *targets == "" {
		*targets = defaultTargets(opts.profile)
	}

	opts.targets = selectTargets(*targets, []target{
		{name: targetGostack, image: *gostack, persistable: true},
		{name: targetSlim, image: *slim, persistable: true},
		{name: targetLocalstack, image: *localstack},
	})

	if len(opts.targets) == 0 {
		return options{}, fmt.Errorf("-targets %q names no target", *targets)
	}

	err = opts.checkBase()
	if err != nil {
		return options{}, err
	}

	return opts, nil
}

// checkBase refuses a two-sided run (-base-image) that cannot write two results
// a comparison can tell apart.
func (o *options) checkBase() error {
	if o.baseImage == "" {
		if o.baseCommit != "" || o.baseOut != "" {
			return errors.New("-base-commit and -base-out need -base-image")
		}

		return nil
	}

	switch {
	case len(o.targets) != 1:
		return fmt.Errorf("-base-image measures one target against its base; -targets names %d", len(o.targets))
	case o.baseCommit == "" || o.baseOut == "":
		return errors.New("-base-image needs -base-commit and -base-out")
	case filepath.Clean(o.baseOut) == filepath.Clean(o.outDir):
		return fmt.Errorf("-base-out and -out are both %s: the two results would share a name", o.outDir)
	}

	return nil
}

// sides lists the images the run measures as one target in alternation: the
// base image and the target's own under -base-image, else the target alone.
func (o *options) sides(t target) []target {
	if o.baseImage == "" {
		return []target{t}
	}

	base, head := t, t
	base.image, base.side = o.baseImage, sideBase
	head.side = sideHead

	return []target{base, head}
}

// bind registers the flags that fill o.
func (o *options) bind(flags *flag.FlagSet) {
	flags.Func("note", "a line for the Notes of the report; repeat for each", func(note string) error {
		o.notes = append(o.notes, note)

		return nil
	})
	flags.StringVar(&o.profile, "profile", profileFull,
		"full: the whole matrix. quick: each axis once at concurrency 16, and the base cell at 1 and 64")
	flags.StringVar(&o.loadgenImage, "loadgen-image", "gostack-bench-loadgen:dev",
		"tag for the load generator image the run builds")
	flags.StringVar(&o.outDir, "out", "bench/results", "directory for the result files")
	flags.StringVar(&o.compareDir, "compare-dir", "",
		"after the run, compare it with the newest committed result in this directory that has this host's fingerprint")
	flags.StringVar(&o.accepted, "accepted", "", "the accepted-regressions list -compare-dir reads, such as "+
		"bench/accepted.yaml")
	flags.StringVar(&o.suffix, "name-suffix", "", "appended to container names, to keep parallel runs apart")
	flags.StringVar(&o.commit, "commit", "", "the commit the images were built from (default: HEAD)")
	flags.StringVar(&o.baseImage, "base-image", "", "measure this image too, as the base of the one target, "+
		"alternating the two in every repeated measurement; needs -base-commit and -base-out")
	flags.StringVar(&o.baseCommit, "base-commit", "", "the commit -base-image was built from")
	flags.StringVar(&o.baseOut, "base-out", "", "directory for the result file of -base-image")
	flags.IntVar(&o.startupRuns, "startup-runs", defaultStartups, "containers started to take the median startup time")
	flags.IntVar(&o.idleRuns, "idle-runs", defaultIdleRuns, "containers measured for each idle variant")
	flags.IntVar(&o.reps, "reps", defaultReps, "times each cell runs")
	flags.DurationVar(&o.settle, "settle", 0, "wait after ready before the idle window (default by profile)")
	flags.DurationVar(&o.idle, "idle", 0, "idle window (default by profile)")
	flags.DurationVar(&o.duration, "duration", defaultLoadDuration, "length of each run of a cell")
	flags.IntVar(&o.cycles, "cycles", defaultCycles, "agentic cycles")
	flags.IntVar(&o.cycleConcurrency, "cycle-concurrency", defaultCycleConc, "agentic concurrency")
}

// check refuses a setting no run can use.
func (o *options) check() error {
	if o.profile != profileFull && o.profile != profileQuick {
		return fmt.Errorf("-profile %q: want %s or %s", o.profile, profileFull, profileQuick)
	}

	if o.reps < 1 || o.startupRuns < 1 || o.idleRuns < 1 {
		return errors.New("-reps, -startup-runs and -idle-runs must be at least 1")
	}

	return nil
}

// defaultTargets are the images a profile measures when -targets is not set.
func defaultTargets(profile string) string {
	if profile == profileQuick {
		return targetGostack
	}

	return targetGostack + "," + targetSlim + "," + targetLocalstack
}

// parseServices reads -services: a comma separated list of services some
// workload is registered for, or nothing for all of them.
func parseServices(list string) ([]string, error) {
	if list == "" {
		return nil, nil
	}

	var out []string

	for name := range strings.SplitSeq(list, ",") {
		if !slices.ContainsFunc(workloads, func(w workload) bool { return w.Service() == name }) {
			return nil, fmt.Errorf("-services %q: %w: %s", list, errNoWorkload, name)
		}

		out = append(out, name)
	}

	return out, nil
}

// selectTargets keeps the targets of all that the comma separated list names.
func selectTargets(list string, all []target) []target {
	var out []target

	for _, t := range all {
		if slices.Contains(strings.Split(list, ","), t.name) {
			out = append(out, t)
		}
	}

	return out
}

func (o *options) applyProfileDefaults() {
	settle, idle := fullSettle, fullIdle
	if o.profile == profileQuick {
		settle, idle = quickSettle, quickIdle
	}

	if o.settle == 0 {
		o.settle = settle
	}

	if o.idle == 0 {
		o.idle = idle
	}
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

	// The list is read before the run, so a bad one fails in a second, not
	// after the measurement.
	accepted, err := openAccepted(opts.accepted)
	if err != nil {
		return err
	}

	api, err := newDockerAPI(ctx)
	if err != nil {
		return err
	}

	run := &runner{opts: opts, api: api, log: stderr}

	reps, err := run.measureAll(ctx)
	if err != nil {
		return err
	}

	// The head's result is written last, so path is the file -compare-dir checks.
	var path string

	for _, out := range reps {
		path, err = writeReport(out.dir, out.rep)
		if err != nil {
			return err
		}

		_, err = io.WriteString(stdout, "results: "+path+".json\n")
		if err != nil {
			return fmt.Errorf("write summary: %w", err)
		}
	}

	_, err = io.WriteString(stdout, "\n")
	if err != nil {
		return fmt.Errorf("write summary: %w", err)
	}

	if opts.compareDir == "" {
		return nil
	}

	return compareFiles(ctx, "", opts.compareDir, path+".json", accepted, stdout)
}

// output is one result file of a run and the directory it goes to.
type output struct {
	dir string
	rep report
}

func (r *runner) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.log, format+"\n", args...)
}

// name returns a container name no other run uses.
func (r *runner) name(kind string) string {
	return fmt.Sprintf("gostack-bench-%s-%d-%d%s", kind, os.Getpid(), r.seq.Add(1), r.opts.suffix)
}

func fingerprintOf(info engineInfo) fingerprint {
	lower := strings.ToLower(info.OperatingSystem + " " + info.KernelVersion)

	return fingerprint{
		OS: info.OSType, Arch: info.Architecture, CPUs: info.NCPU, Docker: info.ServerVersion,
		OrbStack: strings.Contains(lower, "orbstack"),
	}
}

// measureAll measures every target and returns the result files to write: one,
// or under -base-image the base's and then the head's.
func (r *runner) measureAll(ctx context.Context) ([]output, error) {
	rep, err := r.header(ctx)
	if err != nil {
		return nil, err
	}

	err = r.buildLoadgen(ctx)
	if err != nil {
		return nil, err
	}

	defer func() { _, _ = dockerOutput(context.WithoutCancel(ctx), "rmi", "-f", r.opts.loadgenImage) }()

	if r.opts.baseImage != "" {
		res := r.measureSides(ctx, r.opts.sides(r.opts.targets[0]))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("interrupted: %w", ctx.Err())
		}

		base := rep
		base.Commit, base.Targets = r.opts.baseCommit, res[:1]
		rep.Targets = res[1:]

		return []output{{r.opts.baseOut, base}, {r.opts.outDir, rep}}, nil
	}

	for _, t := range r.opts.targets {
		err := ctx.Err()
		if err != nil {
			return nil, fmt.Errorf("interrupted: %w", err)
		}

		rep.Targets = append(rep.Targets, r.measureSides(ctx, []target{t})...)
	}

	return []output{{r.opts.outDir, rep}}, nil
}

// header is a report with everything but the targets: the host, the commit and
// the settings of the run.
func (r *runner) header(ctx context.Context) (report, error) {
	info, err := r.api.info(ctx)
	if err != nil {
		return report{}, err
	}

	commit, dirty, err := gitState(ctx)
	if err != nil {
		return report{}, err
	}

	if r.opts.commit != "" {
		commit = r.opts.commit
	}

	now := time.Now().UTC()
	rep := report{
		Schema: reportSchema, Date: now.Format(dateLayout), Time: now.Format(time.RFC3339), Commit: commit,
		Dirty: dirty, Profile: r.opts.profile,
		Host: hostInfo{
			Docker: info.ServerVersion, OS: info.OperatingSystem, Kernel: info.KernelVersion,
			Arch: info.Architecture, CPUs: info.NCPU, MemoryBytes: info.MemTotal, Fingerprint: fingerprintOf(info),
		},
		Settings: settings{
			Profile: r.opts.profile, Services: r.opts.services, Notes: r.opts.notes,
			StartupRuns: r.opts.startupRuns, IdleRuns: r.opts.idleRuns,
			SettleSeconds: r.opts.settle.Seconds(), IdleSeconds: r.opts.idle.Seconds(),
			CellSeconds: r.opts.duration.Seconds(), Reps: r.opts.reps,
			Cycles: r.opts.cycles, CycleConcurrency: r.opts.cycleConcurrency,
		},
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

// roundOrder is the order the sides run in round i of a repeated measurement:
// as listed in an even round, reversed in an odd one. Two sides go base head,
// head base, base head, ..., so a host that slows down or speeds up during the
// run moves both sides alike instead of reading as a difference between them.
func roundOrder(round, sides int) []int {
	order := make([]int, sides)
	for i := range order {
		order[i] = i
	}

	if round%half == 1 {
		slices.Reverse(order)
	}

	return order
}

// alternate runs a measurement rounds times on every side, in roundOrder.
func alternate(rounds, sides int, run func(round, side int)) {
	for round := range rounds {
		for _, side := range roundOrder(round, sides) {
			run(round, side)
		}
	}
}

// measureSides runs every measurement against the sides of one target: one
// image, or a base and a head measured in alternation. A measurement that fails
// is recorded as a problem of its side and the rest still run.
func (r *runner) measureSides(ctx context.Context, sides []target) []targetResult {
	res := make([]targetResult, len(sides))

	for s, t := range sides {
		res[s] = targetResult{Name: t.name, Image: t.image}
		r.logf("== %s (%s)", t.label(), t.image)

		size, err := imageBytes(ctx, t.image)
		res[s].problem("image size", err)

		res[s].ImageBytes = size
	}

	startups, errs := r.startup(ctx, sides)
	for s := range sides {
		res[s].Startup = startups[s]
		res[s].problem("startup", errs[s])
	}

	r.idleVariant(ctx, sides, res, variantHealthcheck)

	if r.opts.profile == profileFull {
		r.idleVariant(ctx, sides, res, variantNoHealthcheck)
	}

	for _, spec := range cellsFor(r.opts.profile, sides[0], r.opts.services...) {
		if ctx.Err() != nil {
			break
		}

		for s, cell := range r.cell(ctx, sides, spec) {
			if cell.Problem != "" {
				res[s].Problems = append(res[s].Problems, "cell "+cell.ID+": "+cell.Problem)
			}

			res[s].Cells = append(res[s].Cells, cell)
		}
	}

	r.agenticAll(ctx, sides, res)

	return res
}

// idleVariant measures one idle variant on every side into res.
func (r *runner) idleVariant(ctx context.Context, sides []target, res []targetResult, variant string) {
	what := "idle with the HEALTHCHECK"
	if variant == variantNoHealthcheck {
		what = "idle without the HEALTHCHECK"
	}

	idles, errs := r.idle(ctx, sides, variant)
	for s := range sides {
		if variant == variantNoHealthcheck {
			res[s].Idle.NoHealthcheck = idles[s]
		} else {
			res[s].Idle.Healthcheck = idles[s]
		}

		res[s].problem(what, errs[s])
	}
}

// agenticAll runs each service's agentic workload once on every side, the
// order of the sides alternating from one workload to the next.
func (r *runner) agenticAll(ctx context.Context, sides []target, res []targetResult) {
	var round int

	for _, w := range agenticWorkloads {
		if len(r.opts.services) > 0 && !slices.Contains(r.opts.services, w.Service()) {
			continue
		}

		for _, s := range roundOrder(round, len(sides)) {
			run, err := r.agentic(ctx, sides[s], w)
			res[s].problem("agentic workload ("+w.Service()+")", err)

			if run.Cycles > 0 {
				if res[s].Agentic == nil {
					res[s].Agentic = map[string]agenticRun{}
				}

				res[s].Agentic[w.Service()] = run
			}
		}

		round++
	}
}

// start launches a container of the target and waits for the ready line.
func (r *runner) start(ctx context.Context, t target, extra ...string) (*instance, time.Duration, error) {
	inst, err := launch(ctx, r.name(t.label()), t.image, extra...)
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

// startup times startupRuns containers on each side, each stopped at once. A
// side stops at its first failure and its result keeps no spread.
func (r *runner) startup(ctx context.Context, sides []target) ([]startupResult, []error) {
	res := make([]startupResult, len(sides))
	errs := make([]error, len(sides))

	alternate(r.opts.startupRuns, len(sides), func(i, s int) {
		if errs[s] != nil {
			return
		}

		t := sides[s]

		inst, took, err := r.start(ctx, t)
		if err != nil {
			errs[s] = fmt.Errorf("run %d: %w", i+1, err)

			return
		}

		inst.stop(ctx)
		r.logf("%s: ready in %s (run %d of %d)", t.label(), took.Round(time.Millisecond), i+1, r.opts.startupRuns)
		res[s].SamplesMS = append(res[s].SamplesMS, float64(took.Microseconds())/microsPerMilli)
	})

	for s := range sides {
		if errs[s] == nil {
			res[s].MS = spreadOf(res[s].SamplesMS)
		}
	}

	return res, errs
}

// idle measures idleRuns containers on each side that nothing calls: memory and
// CPU over the window. With noHealthcheck the container runs without the image
// HEALTHCHECK, which execs a process every ten seconds, so the difference
// between the two is the cost of the check. A side stops at its first failure.
func (r *runner) idle(ctx context.Context, sides []target, variant string) ([]idleVariant, []error) {
	rss := make([][]float64, len(sides))
	cpu := make([][]float64, len(sides))
	errs := make([]error, len(sides))

	alternate(r.opts.idleRuns, len(sides), func(i, s int) {
		if errs[s] != nil {
			return
		}

		used, err := r.idleRun(ctx, sides[s], variant, i)
		if err != nil {
			errs[s] = fmt.Errorf("run %d: %w", i+1, err)

			return
		}

		rss[s] = append(rss[s], float64(used.MedianRSSBytes))
		cpu[s] = append(cpu[s], used.CPUPercent)
	})

	res := make([]idleVariant, len(sides))

	for s := range sides {
		res[s] = idleVariant{SettleSeconds: r.opts.settle.Seconds(), Seconds: r.opts.idle.Seconds()}
		if errs[s] == nil {
			res[s].Runs = len(rss[s])
			res[s].RSSBytes, res[s].CPUPercent = spreadOf(rss[s]), spreadOf(cpu[s])
		}
	}

	return res, errs
}

// idleRun is run i of idle: one container, settled, then sampled over the window.
func (r *runner) idleRun(ctx context.Context, t target, variant string, i int) (usage, error) {
	noHealthcheck := variant == variantNoHealthcheck

	var extra []string
	if noHealthcheck {
		extra = []string{"--no-healthcheck"}
	}

	inst, _, err := r.start(ctx, t, extra...)
	if err != nil {
		return usage{}, err
	}

	defer inst.stop(ctx)

	r.logf("%s: idle run %d of %d (no HEALTHCHECK %t), settling %s then %s", t.label(), i+1, r.opts.idleRuns,
		noHealthcheck, r.opts.settle, r.opts.idle)

	err = sleep(ctx, r.opts.settle)
	if err != nil {
		return usage{}, err
	}

	return r.sampled(ctx, inst.name, func(ctx context.Context) error { return sleep(ctx, r.opts.idle) })
}

// cell measures one cell on each side: reps runs a side, alternating the sides,
// each run on a fresh container, so that the memory of one run, or one cell,
// does not carry into the next.
func (r *runner) cell(ctx context.Context, sides []target, spec cellSpec) []cellResult {
	runs := make([][]runResult, len(sides))
	failed := make([][]string, len(sides))

	alternate(r.opts.reps, len(sides), func(rep, s int) {
		run, err := r.cellRun(ctx, sides[s], spec)
		if err != nil {
			failed[s] = append(failed[s], fmt.Sprintf("run %d: %v", rep+1, err))

			return
		}

		runs[s] = append(runs[s], run)
	})

	res := make([]cellResult, len(sides))

	for s, t := range sides {
		r.logf("%s: %s: %d of %d runs", t.label(), spec.id(), len(runs[s]), r.opts.reps)

		if len(runs[s]) == 0 {
			res[s] = cellResult{ID: spec.id(), Spec: spec, Problem: strings.Join(failed[s], "; ")}

			continue
		}

		res[s] = summarizeCell(spec, runs[s])
	}

	return res
}

// loadgenArgs are the flags of `loadgen cell` that describe the cell.
func (c cellSpec) loadgenArgs() []string {
	args := make([]string, 0, loadgenCellFlags+argsPerOption*len(c.Options))
	args = append(args, "-service", c.Service, "-workload", c.Workload, "-payload", strconv.Itoa(c.Payload),
		"-concurrency", strconv.Itoa(c.Concurrency))

	for _, o := range c.Options {
		args = append(args, "-opt", o.Key+"="+o.Value)
	}

	return args
}

func (r *runner) cellRun(ctx context.Context, t target, spec cellSpec) (runResult, error) {
	var extra []string
	if spec.persists() {
		extra = []string{"-e", spec.PersistEnv + "=1", "--tmpfs", persistDir}
	}

	inst, _, err := r.start(ctx, t, extra...)
	if err != nil {
		return runResult{}, err
	}

	defer inst.stop(ctx)

	var run runResult

	used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error {
		args := append([]string{"cell"}, spec.loadgenArgs()...)

		return r.loadgen(ctx, inst.name, r.opts.duration+loadgenSlack, &run,
			append(args, "-duration", r.opts.duration.String())...)
	})
	run.Usage = used

	return run, err
}

// agentic runs a service's create, use and delete workload on a container of its
// own, so the peak memory is the workload's.
func (r *runner) agentic(ctx context.Context, t target, w agenticWorkload) (agenticRun, error) {
	inst, _, err := r.start(ctx, t)
	if err != nil {
		return agenticRun{}, err
	}

	defer inst.stop(ctx)

	r.logf("%s: %d %s agentic cycles at concurrency %d", inst.name, r.opts.cycles, w.Service(), r.opts.cycleConcurrency)

	var run agenticRun

	used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error {
		return r.loadgen(ctx, inst.name, agenticTimeout, &run, "agentic", "-service", w.Service(),
			"-cycles", strconv.Itoa(r.opts.cycles), "-concurrency", strconv.Itoa(r.opts.cycleConcurrency))
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

	cmd := []string{cmdRun, "--rm", "--name", name, "--network", "container:" + targetName, r.opts.loadgenImage, "loadgen"}

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

	var (
		mu      sync.Mutex
		peak    = first.rss()
		samples []float64
	)

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
					mu.Lock()
					peak = max(peak, now.rss())
					samples = append(samples, float64(now.rss()))
					mu.Unlock()
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

	peak = max(peak, last.rss())
	used := usage{
		BaselineRSSBytes: first.rss(), EndRSSBytes: last.rss(), PeakRSSBytes: peak,
		MedianRSSBytes: uint64(median(samples)),
	}

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
