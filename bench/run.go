package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// memoryLimit is what -memory accepts: a number of bytes with a b, k, m or g after
// it in either case, as docker run --memory takes them (1G, 1g, 512m).
var memoryLimit = regexp.MustCompile(`^([1-9][0-9]*)([bBkKmMgG]?)$`)

// memoryUnits is what the unit of a -memory value multiplies it by; none is bytes.
var memoryUnits = map[string]uint64{"": 1, "b": 1, "k": bytesPerKiB, "m": bytesPerMiB, "g": bytesPerGiB}

// memoryFloor is the least docker run --memory accepts, 6 MiB.
const memoryFloor = 6 * bytesPerMiB

// errMemory reports a -memory value docker run would refuse.
var errMemory = errors.New("memory limit")

// parseMemory is the bytes of a -memory value. It refuses what docker run
// --memory would: a value that is not a number with a unit, one too big for a
// count of bytes, and one under memoryFloor (4096, 2b and 100k are).
func parseMemory(s string) (uint64, error) {
	m := memoryLimit.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%w: want a number of bytes with an optional b, k, m or g, such as 1g", errMemory)
	}

	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errMemory, err)
	}

	unit := memoryUnits[strings.ToLower(m[2])]

	if n > math.MaxUint64/unit {
		return 0, fmt.Errorf("%w: more than a count of bytes holds", errMemory)
	}

	if n*unit < memoryFloor {
		return 0, fmt.Errorf("%w: docker takes at least 6m", errMemory)
	}

	return n * unit, nil
}

// profileSpec is what a profile runs, which the run reads from profileSpecs and
// never from the profile's name. What the table leaves out is each workload's own
// choice of cells for a profile (its Cells).
type profileSpec struct {
	name string
	// about is the profile's line in the help of -profile.
	about string
	// targets is the default of -targets.
	targets string
	// settle and idle are the default idle windows.
	settle, idle time.Duration
	// footprint is whether the run times startup, first use and idle memory, and
	// agentic whether it runs each service's create, use and delete cycle.
	footprint, agentic bool
	// noHealthcheck is whether the idle windows run a second time on a target with
	// its HEALTHCHECK off.
	noHealthcheck bool
	// large is whether the profile runs the large workloads and no others, and no
	// other profile runs them. Its cells run under a memory limit, which -memory
	// replaces.
	large bool
}

var profileSpecs = []profileSpec{
	{
		name:          profileFull,
		about:         "the whole matrix",
		targets:       targetGostack + "," + targetSlim + "," + targetLocalstack,
		settle:        fullSettle,
		idle:          fullIdle,
		footprint:     true,
		agentic:       true,
		noHealthcheck: true,
	},
	{
		name:      profileQuick,
		about:     "each axis once at concurrency 16, and the base cell at 1 and 64",
		targets:   targetGostack,
		settle:    quickSettle,
		idle:      quickIdle,
		footprint: true,
		agentic:   true,
	},
	// The large profile measures its cells and nothing else: the startup, idle and
	// agentic runs of the other profiles are not what it is for, and each would add
	// minutes to a run that is already long.
	{
		name:    profileLarge,
		about:   "the large-table workloads alone, each cell a table of its own under a memory limit",
		targets: targetGostack,
		settle:  fullSettle,
		idle:    fullIdle,
		large:   true,
	},
}

// profileOf is the spec of a profile by name.
func profileOf(name string) (profileSpec, bool) {
	i := slices.IndexFunc(profileSpecs, func(p profileSpec) bool { return p.name == name })
	if i < 0 {
		return profileSpec{}, false
	}

	return profileSpecs[i], true
}

// profileNames lists the profiles as an error says them: "full, quick or large".
func profileNames() string {
	names := make([]string, len(profileSpecs))
	for i, p := range profileSpecs {
		names[i] = p.name
	}

	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// profileHelp is the help of -profile: each profile and what it runs.
func profileHelp() string {
	lines := make([]string, len(profileSpecs))
	for i, p := range profileSpecs {
		lines[i] = p.name + ": " + p.about
	}

	return strings.Join(lines, ". ")
}

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
	memory           string
	startupRuns      int
	firstUseRuns     int
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
		"(default: all for the full profile, gostack for the others)")

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
	}

	// Absolute paths with the links resolved, so one directory spelled two ways
	// (out and ./out, a relative and an absolute path, /tmp/x and /private/tmp/x)
	// is still one directory. A run of a commit against itself would otherwise
	// write both results to one file name.
	baseOut, err := realPath(o.baseOut)
	if err != nil {
		return fmt.Errorf("-base-out %s: %w", o.baseOut, err)
	}

	out, err := realPath(o.outDir)
	if err != nil {
		return fmt.Errorf("-out %s: %w", o.outDir, err)
	}

	if baseOut == out {
		return fmt.Errorf("-base-out and -out are both %s: the two results would share a name", out)
	}

	return nil
}

// realPath is the absolute path of p with every symbolic link in it resolved.
// A directory that does not exist yet is resolved through its nearest parent
// that does, as the run creates it there. A link to a directory that does not
// exist yet is followed to where the run would create it.
func realPath(p string) (string, error) {
	return resolvePath(p, maxLinkDepth)
}

// maxLinkDepth is how many dangling links realPath follows before it gives up,
// as the kernel does for a loop.
const maxLinkDepth = 40

func resolvePath(p string, depth int) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}

	dir, rest := abs, ""

	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}

		parent := filepath.Dir(dir)
		if !errors.Is(err, fs.ErrNotExist) || parent == dir {
			return "", fmt.Errorf("resolve links: %w", err)
		}

		// A link whose target is not there yet: the run creates the target.
		if dest, ok := danglingLink(dir); ok {
			if depth == 0 {
				return "", fmt.Errorf("resolve links: too many links at %s", dir)
			}

			resolved, err := resolvePath(dest, depth-1)
			if err != nil {
				return "", err
			}

			return filepath.Join(resolved, rest), nil
		}

		dir, rest = parent, filepath.Join(filepath.Base(dir), rest)
	}
}

// danglingLink returns where the symbolic link at path points, made absolute,
// when path is a link.
func danglingLink(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return "", false
	}

	dest, err := os.Readlink(path)
	if err != nil {
		return "", false
	}

	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(path), dest)
	}

	return dest, true
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
	flags.StringVar(&o.profile, "profile", profileFull, profileHelp())
	flags.StringVar(&o.memory, "memory", "",
		"the memory limit of every cell that has one, as docker run --memory writes it, at least 6m "+
			"(default: the limit of the cell)")
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
	flags.IntVar(&o.firstUseRuns, "first-use-runs", defaultFirstUseRuns,
		"containers started to time a service's first use (SNS: the first signed publish)")
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
	profile, ok := profileOf(o.profile)
	if !ok {
		return fmt.Errorf("-profile %q: want %s", o.profile, profileNames())
	}

	err := o.checkMemory(profile)
	if err != nil {
		return err
	}

	if o.reps < 1 || o.startupRuns < 1 || o.idleRuns < 1 {
		return errors.New("-reps, -startup-runs and -idle-runs must be at least 1")
	}

	if o.firstUseRuns < minFirstUseRuns {
		return fmt.Errorf("-first-use-runs %d: want at least %d, the fewest a side with which the rank-sum test can "+
			"fail a first use", o.firstUseRuns, minFirstUseRuns)
	}

	return nil
}

// checkMemory refuses a -memory that the profile cannot use or docker would refuse.
func (o *options) checkMemory(profile profileSpec) error {
	if o.memory == "" {
		return nil
	}

	if !profile.large {
		return fmt.Errorf("-memory %q: the %s profile has no memory limit to replace; only a profile that runs "+
			"the large workloads has one", o.memory, o.profile)
	}

	_, err := parseMemory(o.memory)
	if err != nil {
		return fmt.Errorf("-memory %q: %w", o.memory, err)
	}

	return nil
}

// defaultTargets are the images a profile measures when -targets is not set.
func defaultTargets(profile string) string {
	spec, _ := profileOf(profile)

	return spec.targets
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
	profile, _ := profileOf(o.profile)

	if o.settle == 0 {
		o.settle = profile.settle
	}

	if o.idle == 0 {
		o.idle = profile.idle
	}
}

// runner holds what one `bench run` shares between its measurements.
type runner struct {
	opts options
	api  *dockerAPI
	log  io.Writer
	seq  atomic.Int64
	// cellRounds counts the runs of every cell so far, so that the sides alternate
	// across cells: with one repetition a cell has one round, and a count that
	// began again in each cell would run the base first in all of them.
	cellRounds int
	// runCell runs a cell once on a target; nil is cellRun. A test puts a fake here.
	runCell func(ctx context.Context, t target, spec cellSpec) (runResult, error)
}

// takeRounds reserves the next n rounds of cell runs and returns the first.
func (r *runner) takeRounds(n int) int {
	first := r.cellRounds
	r.cellRounds += n

	return first
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
			StartupRuns: r.opts.startupRuns, FirstUseRuns: r.opts.firstUseRuns, IdleRuns: r.opts.idleRuns,
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
	alternateFrom(0, rounds, sides, run)
}

// alternateFrom is alternate for rounds first to first+rounds-1: the order of a
// round follows its number, so a caller that counts on across measurements keeps
// the side that runs first changing from one to the next.
func alternateFrom(first, rounds, sides int, run func(round, side int)) {
	for round := first; round < first+rounds; round++ {
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

	profile, _ := profileOf(r.opts.profile)

	if profile.footprint {
		r.footprint(ctx, sides, res)
	}

	for _, spec := range cellsFor(r.opts.profile, sides[0], r.opts.services...) {
		if ctx.Err() != nil {
			break
		}

		for s, cell := range r.cell(ctx, sides, r.opts.withMemory(spec)) {
			res[s].noteCell(cell)

			res[s].Cells = append(res[s].Cells, cell)
		}
	}

	if profile.agentic {
		r.agenticAll(ctx, sides, res)
	}

	return res
}

// withMemory is the cell with the run's memory limit, when it has one and -memory
// names another.
func (o *options) withMemory(spec cellSpec) cellSpec {
	if spec.limited() && o.memory != "" {
		spec.Memory = o.memory
	}

	return spec
}

// footprint measures what the target costs doing nothing: the time to start,
// the first use of a service and the idle memory and CPU.
func (r *runner) footprint(ctx context.Context, sides []target, res []targetResult) {
	startups, errs := r.startup(ctx, sides)
	for s := range sides {
		res[s].Startup = startups[s]
		res[s].problem("startup", errs[s])
	}

	r.firstUseAll(ctx, sides, res)

	r.idleVariant(ctx, sides, res, variantHealthcheck)

	if profile, _ := profileOf(r.opts.profile); profile.noHealthcheck {
		r.idleVariant(ctx, sides, res, variantNoHealthcheck)
	}
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
				res[s].noteAgentic(w.Service(), run)
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

	used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error { return sleep(ctx, r.opts.idle) })
	if err == nil {
		err = inst.alive(ctx)
	}

	return used, err
}

// cell measures one cell on each side: reps runs a side, alternating the sides,
// each run on a fresh container, so that the memory of one run, or one cell,
// does not carry into the next.
func (r *runner) cell(ctx context.Context, sides []target, spec cellSpec) []cellResult {
	runs := make([][]runResult, len(sides))
	failedRuns := make([][]failedRun, len(sides))
	failed := make([][]string, len(sides))
	first := r.takeRounds(r.opts.reps)

	runOnce := r.runCell
	if runOnce == nil {
		runOnce = r.cellRun
	}

	alternateFrom(first, r.opts.reps, len(sides), func(round, s int) {
		rep := round - first

		r.logf("%s: %s: run %d of %d, load average %s", sides[s].label(), spec.id(), rep+1, r.opts.reps,
			loadAverage(ctx))

		run, err := runOnce(ctx, sides[s], spec)
		if err != nil {
			failed[s] = append(failed[s], failedReason(rep+1, err, run))
			failedRuns[s] = append(failedRuns[s], failedRun{run: run, oomKilled: errors.Is(err, errOOMKilled)})

			return
		}

		runs[s] = append(runs[s], run)
	})

	res := make([]cellResult, len(sides))

	for s, t := range sides {
		res[s] = withFailed(settleCell(spec, runs[s], failed[s]), failedRuns[s])

		r.logf("%s: %s: %d of %d runs, %d OOM-killed", t.label(), spec.id(), len(runs[s]), r.opts.reps,
			res[s].OOMKilled)
	}

	return res
}

// loadAverage is the host's load average as `uptime` prints it, for the log of a
// run on a machine that is not quiet. It is "unknown" where there is no uptime.
func loadAverage(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "uptime").Output()
	if err != nil {
		return "unknown"
	}

	_, load, found := strings.Cut(string(out), "load average")
	if !found {
		return "unknown"
	}

	return strings.TrimSpace(strings.TrimLeft(load, "s:"))
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

// containerArgs are the `docker run` flags of a cell's container: its persistence
// variable and the memory limit. A persistence directory in memory (tmpfs) is
// charged to the container's limit, so a cell with a limit keeps it on disk; the
// limit is the swap limit too, so a container that goes over is killed, not
// swapped.
func (c cellSpec) containerArgs() []string {
	var args []string

	if c.persists() {
		args = append(args, "-e", c.PersistEnv+"=1")

		if !c.limited() {
			args = append(args, "--tmpfs", persistDir)
		}
	}

	if c.limited() {
		args = append(args, "--memory", c.Memory, "--memory-swap", c.Memory)
	}

	return args
}

// timeout is how long the load generator may take to run the cell: its clock,
// the slack, and the time the workload needs to fill what it loads.
func (r *runner) timeout(spec cellSpec) time.Duration {
	timeout := r.opts.duration + loadgenSlack

	wl, err := findWorkload(spec.Service, spec.Workload)
	if err != nil {
		return timeout
	}

	if seeded, ok := wl.(seededWorkload); ok {
		timeout += seeded.SeedTime(spec)
	}

	return timeout
}

// cellRun runs the cell once on a container of its own. A container the kernel
// killed for its memory is a failed run that wraps errOOMKilled, and the run
// returned with it holds the memory the container reached.
func (r *runner) cellRun(ctx context.Context, t target, spec cellSpec) (runResult, error) {
	inst, _, err := r.start(ctx, t, spec.containerArgs()...)
	if err != nil {
		return runResult{}, err
	}

	defer inst.stop(ctx)

	var run runResult

	used, err := r.sampled(ctx, inst.name, func(ctx context.Context) error {
		args := append([]string{"cell"}, spec.loadgenArgs()...)

		return r.loadgen(ctx, inst.name, r.timeout(spec), &run,
			append(args, "-duration", r.opts.duration.String())...)
	})
	run.Usage = used

	return endRun(ctx, inst, spec, run, err)
}

// endRun is the run and its error once the load generator has returned: the load
// generator's own error, and how the target container stands, read after
// settleWait.
func endRun(ctx context.Context, inst *instance, spec cellSpec, run runResult, loadgenErr error) (runResult, error) {
	return run, runError(loadgenErr, inst.aliveWithin(ctx, spec.settleWait(loadgenErr, run)))
}

// settleWait is how long a run waits for its container to stop before it reads
// the container's state. A load generator that failed may have done so the moment
// the connection dropped, before the daemon recorded why the container stopped.
// So may the operations of a cell with a memory limit that failed as its container
// was dying. In any other run a container that is still running costs the whole
// wait for nothing: a cell without a limit is not OOM-killed.
func (c cellSpec) settleWait(loadgenErr error, run runResult) time.Duration {
	if loadgenErr != nil || (c.limited() && run.opErrors() > 0) {
		return oomSettle
	}

	return 0
}

// runError is the error of a run from what the load generator returned and what
// the target container's state says. A load generator that failed on a container
// the kernel killed says why the run failed; one that failed on a container that
// is running, or died of something else, says it itself.
func runError(loadgenErr, died error) error {
	switch {
	case loadgenErr == nil:
		return died
	case errors.Is(died, errOOMKilled):
		return fmt.Errorf("%w; the load generator said: %w", died, loadgenErr)
	default:
		return loadgenErr
	}
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

	if err == nil {
		err = inst.alive(ctx)
	}

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
		// A load generator that failed may have printed what it had measured, such as
		// how far a fill got; what it printed is kept, and the failure stays the error.
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, out)
		}

		return err
	}

	err = json.Unmarshal(raw, out)
	if err != nil {
		return fmt.Errorf("decode the load generator output %q: %w", raw, err)
	}

	return nil
}

// memoryPeaks is the highest memory readings of a window, and the resident set of
// each sample in it. A reading of nothing at all is what a container that has
// stopped returns, and is no reading.
type memoryPeaks struct {
	rss, usage uint64
	samples    []float64
}

// peak notes a reading in the peaks.
func (m *memoryPeaks) peak(s containerStats) {
	if s.zero() {
		return
	}

	m.rss = max(m.rss, s.rss())
	m.usage = max(m.usage, s.usage())
}

// sample notes a reading in the peaks and in the samples whose median is the
// resident set of the window.
func (m *memoryPeaks) sample(s containerStats) {
	if s.zero() {
		return
	}

	m.peak(s)
	m.samples = append(m.samples, float64(s.rss()))
}

// sampled runs work while it reads the container's counters: once before, every
// sampleEvery during, once after. It returns the memory and CPU of the window.
func (r *runner) sampled(ctx context.Context, container string, work func(context.Context) error) (usage, error) {
	first, err := r.api.stats(ctx, container)
	if err != nil {
		return usage{}, fmt.Errorf("read stats: %w", err)
	}

	var (
		mu    sync.Mutex
		peaks memoryPeaks
	)

	peaks.peak(first)

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
					peaks.sample(now)
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
		// The memory reached before the counters went is what a container the
		// kernel killed has to show.
		return windowUsage(first, containerStats{}, peaks), errors.Join(workErr, fmt.Errorf("read stats: %w", err))
	}

	return windowUsage(first, last, peaks), workErr
}

// windowUsage is the memory and CPU of a window from the counters before and
// after it and the peaks between. A container that has stopped returns zero
// counters at the end: the window has no end to read, and its CPU time would run
// backwards, so what it reached is all it has to show.
func windowUsage(first, last containerStats, peaks memoryPeaks) usage {
	peaks.peak(last)

	used := usage{BaselineRSSBytes: first.rss(), PeakRSSBytes: peaks.rss, PeakUsageBytes: peaks.usage}
	if last.zero() {
		return used
	}

	used.EndRSSBytes, used.MedianRSSBytes = last.rss(), uint64(median(peaks.samples))

	if wall := last.Read.Sub(first.Read); wall > 0 {
		cpuNanos := last.CPUStats.CPUUsage.TotalUsage - first.CPUStats.CPUUsage.TotalUsage
		used.CPUPercent = float64(cpuNanos) / float64(wall.Nanoseconds()) * percentScale
	}

	return used
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
