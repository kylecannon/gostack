package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Defaults of `bench select`. The Makefile passes its own BENCH_TIME,
// BENCH_COUNT, BENCH_SHARD_BUDGET and BENCH_SHARD_LIMIT.
const (
	defaultSelectBenchtime = 500 * time.Millisecond
	defaultSelectCount     = 10
	defaultShardBudget     = 40 * time.Minute
	// defaultShardLimit is what a shard job holds: its timeout-minutes in
	// .github/workflows/bench.yml. The budget is a packing target; a unit past
	// the budget gets a shard of its own, and one past the limit cannot run.
	defaultShardLimit = 60 * time.Minute
	// defaultShardSetup is a shard job's time outside its rounds: two
	// checkouts, setup-go, the first round's compile and the upload.
	defaultShardSetup = 5 * time.Minute
	// defaultShardLink is the relink of one package for both trees, once per
	// round: run 44689662786 linked 19 packages for both trees in 23 s.
	defaultShardLink = 1500 * time.Millisecond
	manifestName     = "manifest"
	manifestFields   = 4 // <binary> <dir> <bench regex> <import path>
	wholePackage     = "."
	runTimeout       = "30m"
	testdataDir      = "testdata"
	goSuffix         = ".go"
)

// buildVerdict is what selection decided about one benchmark package.
type buildVerdict string

const (
	verdictUnchanged      buildVerdict = "unchanged"
	verdictChanged        buildVerdict = "changed"
	verdictUnreproducible buildVerdict = "not reproducible"
	verdictHeadOnly       buildVerdict = "head only"
	verdictBaseOnly       buildVerdict = "base only"
	// verdictNoBenchmark is a package whose test binary lists no benchmark,
	// whatever a source file says: only the binary's list decides what a
	// package holds. It is dropped from selection, as a package only the base
	// holds is.
	verdictNoBenchmark buildVerdict = "no benchmark"
)

// errSelect is returned when selection cannot decide what to run.
var errSelect = errors.New("select")

// benchBin is one line of a bench-build manifest: a package's test binary.
type benchBin struct {
	bin string // file name in the build directory
	dir string // the package directory, which the binary runs in
	pkg string // import path
}

// selected is one package's verdict and, for a package that runs, the measured
// cost of one run of each of its top-level benchmarks at the gate's benchtime.
type selected struct {
	bin     benchBin
	verdict buildVerdict
	base    *benchBin // the base's binary of the package, nil when the base holds none
	benches []benchCost
	// headOnly is the top-level benchmarks only the head holds, each timed once
	// at the gate's benchtime. They have no base rows, so no shard plans them.
	headOnly []benchCost
}

type benchCost struct {
	name string
	cost time.Duration
}

func (s *selected) cost() time.Duration {
	var total time.Duration
	for _, m := range s.benches {
		total += m.cost
	}

	return total
}

// unit is what a shard is packed from: a whole package, or the named top-level
// benchmarks of a package too large for one shard.
type unit struct {
	pkg   string
	names []string // nil runs the whole package
	cost  time.Duration
}

// shard is one job of the matrix: what it runs, and its estimated wall time.
type shard struct {
	units []unit
	est   time.Duration
}

// shardParams is what a shard's wall time is estimated from.
type shardParams struct {
	count  int
	budget time.Duration
	limit  time.Duration
	setup  time.Duration
	link   time.Duration
}

// estimate is a shard's wall time: its setup, then count rounds, each relinking
// every package it holds and running its benchmarks once per tree.
func (p shardParams) estimate(units []unit) time.Duration {
	pkgs := map[string]bool{}

	var run time.Duration

	for _, u := range units {
		pkgs[u.pkg] = true
		run += u.cost
	}

	return p.setup + time.Duration(p.count)*(2*run+time.Duration(len(pkgs))*p.link)
}

// runBin runs a test binary of dir in its package directory and returns its
// standard output and how long it ran.
type runBin func(ctx context.Context, binDir string, b benchBin, args ...string) ([]byte, time.Duration, error)

func execBin(ctx context.Context, binDir string, b benchBin, args ...string) ([]byte, time.Duration, error) {
	var stdout, stderr bytes.Buffer

	//nolint:gosec // the binary is one bench-build wrote
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, b.bin), args...)
	cmd.Dir = b.dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		return nil, elapsed, fmt.Errorf("%s %s: %w\n%s%s",
			b.pkg, strings.Join(args, " "), err, stdout.Bytes(), stderr.Bytes())
	}

	return stdout.Bytes(), elapsed, nil
}

type selectConfig struct {
	base, head, again string
	benchtime         time.Duration
	params            shardParams
	matrix            string
}

// selectMain is `bench select`: it decides which benchmark packages the gate
// runs and packs them into shards. -base and -head are bench-build directories
// of the two trees, built at one fixed layout with -trimpath; -again is the head
// built a second time with an empty build cache. A package runs when its head
// binary differs from its base binary, when the files of its directory that are
// not Go source differ (a benchmark may read them at run time, and the binary
// does not hold them), or when its head binary differs from its second build
// (the build is not reproducible, so equal bytes would prove nothing). Every
// top-level benchmark of a package that runs, and of a package only the head
// holds, is run once at -benchtime and timed, so a broken benchmark fails here.
// A benchmark only the head holds, in a package only the head holds or in one
// that runs, has no base rows: it is checked against the limit but planned in
// no shard. The shards are packed from the times of the others. A unit past the
// budget gets a shard of its own, and one past what a shard job holds fails
// selection. The report goes to out and the GitHub Actions matrix to -matrix.
func selectMain(ctx context.Context, args []string, out io.Writer, run runBin) error {
	cfg, err := parseSelect(args)
	if err != nil {
		return err
	}

	pkgs, err := classify(cfg.base, cfg.head, cfg.again)
	if err != nil {
		return err
	}

	err = measure(ctx, cfg.head, cfg.base, cfg.benchtime, pkgs, run)
	if err != nil {
		return err
	}

	us, err := units(pkgs, cfg.params)
	if err != nil {
		return err
	}

	shards := pack(us, cfg.params)

	err = writeMatrix(cfg.matrix, shards)
	if err != nil {
		return err
	}

	return writeSelection(out, pkgs, shards, cfg.params)
}

func parseSelect(args []string) (selectConfig, error) {
	var cfg selectConfig

	flags := flag.NewFlagSet("select", flag.ContinueOnError)
	flags.StringVar(&cfg.base, "base", "", "bench-build directory of the base")
	flags.StringVar(&cfg.head, "head", "", "bench-build directory of the head")
	flags.StringVar(&cfg.again, "again", "", "bench-build directory of the head, built again with an empty build cache")
	flags.DurationVar(&cfg.benchtime, "benchtime", defaultSelectBenchtime, "the gate's -benchtime")
	flags.IntVar(&cfg.params.count, "count", defaultSelectCount, "the gate's rounds")
	flags.DurationVar(&cfg.params.budget, "budget", defaultShardBudget, "the most one shard should take")
	flags.DurationVar(&cfg.params.limit, "limit", defaultShardLimit, "the most a shard job holds, its timeout")
	flags.DurationVar(&cfg.params.setup, "setup", defaultShardSetup, "a shard's time outside its rounds")
	flags.DurationVar(&cfg.params.link, "link", defaultShardLink, "one package's relink for both trees")
	flags.StringVar(&cfg.matrix, "matrix", "", "where to write the matrix as JSON")

	err := flags.Parse(args)
	if err != nil {
		return cfg, fmt.Errorf("parse flags: %w", err)
	}

	if cfg.base == "" || cfg.head == "" || cfg.again == "" || cfg.matrix == "" {
		return cfg, fmt.Errorf("%w: -base, -head, -again and -matrix are required", errSelect)
	}

	if cfg.params.count < 1 || cfg.benchtime <= 0 || cfg.params.budget <= 0 {
		return cfg, fmt.Errorf("%w: -count, -benchtime and -budget must be above zero", errSelect)
	}

	if cfg.params.limit < cfg.params.budget {
		return cfg, fmt.Errorf("%w: -limit %s is below -budget %s", errSelect, cfg.params.limit, cfg.params.budget)
	}

	return cfg, nil
}

// classify gives every package of either tree its verdict, head packages first
// in manifest order, then the packages only the base holds.
func classify(baseDir, headDir, againDir string) ([]*selected, error) {
	base, err := readManifest(baseDir)
	if err != nil {
		return nil, err
	}

	head, err := readManifest(headDir)
	if err != nil {
		return nil, err
	}

	again, err := readManifest(againDir)
	if err != nil {
		return nil, err
	}

	var pkgs []*selected

	for _, b := range head {
		v, err := compareBuilds(heldBuild(headDir, head, b.bin),
			heldBuild(baseDir, base, b.bin), heldBuild(againDir, again, b.bin))
		if err != nil {
			return nil, err
		}

		pkgs = append(pkgs, &selected{bin: b, verdict: v, base: findBin(base, b.bin)})
	}

	for _, b := range base {
		if !inManifest(head, b.bin) {
			pkgs = append(pkgs, &selected{bin: b, verdict: verdictBaseOnly})
		}
	}

	return pkgs, nil
}

// findBin is the manifest entry of bin, or nil.
func findBin(bins []benchBin, bin string) *benchBin {
	i := slices.IndexFunc(bins, func(b benchBin) bool { return b.bin == bin })
	if i < 0 {
		return nil
	}

	return &bins[i]
}

func inManifest(bins []benchBin, bin string) bool {
	return slices.ContainsFunc(bins, func(b benchBin) bool { return b.bin == bin })
}

// build is a package's test binary in one build directory, and the package
// directory it was built from. The zero build is one that does not hold the
// package.
type build struct {
	path string
	dir  string
}

// heldBuild is the build of bin in a build directory, or the zero build when
// its manifest does not list bin.
func heldBuild(dir string, bins []benchBin, bin string) build {
	i := slices.IndexFunc(bins, func(b benchBin) bool { return b.bin == bin })
	if i < 0 {
		return build{}
	}

	return build{path: filepath.Join(dir, bin), dir: bins[i].dir}
}

// compareBuilds compares a head binary with the base's and with its own second
// build, then the files beside the base's and the head's package directory that
// the binary does not hold.
func compareBuilds(head, base, again build) (buildVerdict, error) {
	if base.path == "" {
		return verdictHeadOnly, nil
	}

	if again.path == "" {
		return verdictUnreproducible, nil
	}

	headSum, err := fileSum(head.path)
	if err != nil {
		return "", err
	}

	againSum, err := fileSum(again.path)
	if err != nil {
		return "", err
	}

	if againSum != headSum {
		return verdictUnreproducible, nil
	}

	baseSum, err := fileSum(base.path)
	if err != nil {
		return "", err
	}

	if baseSum != headSum {
		return verdictChanged, nil
	}

	headData, err := dataSum(head.dir)
	if err != nil {
		return "", err
	}

	baseData, err := dataSum(base.dir)
	if err != nil {
		return "", err
	}

	if baseData != headData {
		return verdictChanged, nil
	}

	return verdictUnchanged, nil
}

// dataSum hashes what a test binary can read at run time from its package
// directory without the compiler having seen it: every file under dir, outside
// the directories of other packages, whose name does not end in .go, and every
// file under a testdata directory, which go tooling ignores and a test reads as
// data. Each is hashed by its path below dir
// and its content, so a changed, added, removed or renamed file changes the sum.
func dataSum(dir string) ([sha256.Size]byte, error) {
	h := sha256.New()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}

		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return skipSubpackage(path, rel)
		}

		if strings.HasSuffix(rel, goSuffix) && !isTestdata(rel) {
			return nil
		}

		sum := [sha256.Size]byte{}
		if d.Type().IsRegular() {
			sum, err = fileSum(path)
			if err != nil {
				return err
			}
		}

		_, _ = fmt.Fprintf(h, "%s\x00%x\n", rel, sum) // a hash's Write never fails

		return nil
	})

	var sum [sha256.Size]byte

	if err != nil {
		return sum, fmt.Errorf("hash the files of %s: %w", dir, err)
	}

	h.Sum(sum[:0])

	return sum, nil
}

// isTestdata reports whether a slash path below a package directory has a
// testdata directory in it.
func isTestdata(rel string) bool {
	return strings.Contains("/"+rel+"/", "/"+testdataDir+"/")
}

// skipSubpackage skips a directory below a package directory that holds Go
// files of its own, which is another package with a verdict of its own. Its
// files are not the parent's data, and hashing them would mark the parent
// changed whenever a descendant's data did. A testdata directory is data, Go
// files and all.
func skipSubpackage(path, rel string) error {
	if rel == "." || isTestdata(rel) {
		return nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), goSuffix) {
			return fs.SkipDir
		}
	}

	return nil
}

func fileSum(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte

	f, err := os.Open(path) //nolint:gosec // a path bench-build wrote
	if err != nil {
		return sum, fmt.Errorf("open test binary: %w", err)
	}

	h := sha256.New()
	_, err = io.Copy(h, f)

	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}

	if err != nil {
		return sum, fmt.Errorf("read %s: %w", path, err)
	}

	h.Sum(sum[:0])

	return sum, nil
}

// readManifest reads a bench-build manifest: "<binary> <dir> <bench regex>
// <import path>" per line.
func readManifest(dir string) ([]benchBin, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName)) //nolint:gosec // the directory bench-build wrote
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var bins []benchBin

	// owners maps a binary name, folded to lower case because a case-insensitive
	// file system would write two names that differ in case to one file, to the
	// package that holds it.
	owners := map[string]string{}

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}

		if len(fields) != manifestFields {
			return nil, fmt.Errorf("%w: %s: manifest line %q is not <binary> <dir> <bench> <package>", errSelect, dir, sc.Text())
		}

		key := strings.ToLower(fields[0])
		if owner, ok := owners[key]; ok {
			return nil, fmt.Errorf("%w: %s: packages %s and %s share the test binary name %s, so one would overwrite the other",
				errSelect, dir, owner, fields[3], fields[0])
		}

		owners[key] = fields[3]
		bins = append(bins, benchBin{bin: fields[0], dir: fields[1], pkg: fields[3]})
	}

	return bins, nil
}

// measure times what selection picked from the head's build: every top-level
// benchmark of a package only the head holds or of a package that runs, once at
// benchtime. A failing run, or a failing list of a binary's benchmarks, fails
// selection. A package whose binary lists no benchmark gets the verdict
// verdictNoBenchmark and is neither timed nor compared.
func measure(
	ctx context.Context, headDir, baseDir string, benchtime time.Duration, pkgs []*selected, run runBin,
) error {
	for _, s := range pkgs {
		var err error

		switch s.verdict {
		case verdictHeadOnly, verdictChanged, verdictUnreproducible:
			err = measurePackage(ctx, headDir, baseDir, benchtime, s, run)
		case verdictBaseOnly:
			err = dropEmpty(ctx, baseDir, s, run)
		default: // unchanged: nothing to run
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// dropEmpty gives a package only the base holds the verdict verdictNoBenchmark
// when its binary lists no benchmark. The package is dropped either way.
func dropEmpty(ctx context.Context, binDir string, s *selected, run runBin) error {
	names, err := listBenchmarks(ctx, binDir, s.bin, run)
	if err != nil {
		return err
	}

	if len(names) == 0 {
		s.verdict = verdictNoBenchmark
	}

	return nil
}

// measurePackage times each top-level benchmark of a package at benchtime, the
// way the gate runs it, so one that breaks late or runs long fails here and not
// on the next pull request that selects the package. A benchmark that only the
// head holds has nothing to compare against: it is timed into headOnly and is
// no unit of a shard.
func measurePackage(
	ctx context.Context, headDir, baseDir string, benchtime time.Duration, s *selected, run runBin,
) error {
	head, err := listBenchmarks(ctx, headDir, s.bin, run)
	if err != nil {
		return err
	}

	if len(head) == 0 {
		// The Makefile finds benchmark packages with a grep that a fixture string
		// satisfies too, so the binary's list has the last word.
		s.verdict = verdictNoBenchmark

		return nil
	}

	var inBase []string

	if s.base != nil {
		inBase, err = listBenchmarks(ctx, baseDir, *s.base, run)
		if err != nil {
			return err
		}
	}

	for _, name := range head {
		_, elapsed, runErr := run(ctx, headDir, s.bin, benchArgs(anchored([]string{name}), benchtime.String())...)
		if runErr != nil {
			return runErr
		}

		cost := benchCost{name: name, cost: elapsed}

		if s.base == nil || !slices.Contains(inBase, name) {
			s.headOnly = append(s.headOnly, cost)
		} else {
			s.benches = append(s.benches, cost)
		}
	}

	return nil
}

// listBenchmarks is the top-level benchmarks a test binary holds.
func listBenchmarks(ctx context.Context, binDir string, b benchBin, run runBin) ([]string, error) {
	list, _, err := run(ctx, binDir, b, "-test.list", "^Benchmark")
	if err != nil {
		return nil, err
	}

	var names []string

	for name := range strings.FieldsSeq(string(list)) {
		if strings.HasPrefix(name, "Benchmark") {
			names = append(names, name)
		}
	}

	return names, nil
}

func benchArgs(bench, benchtime string) []string {
	return []string{
		"-test.run", "^$", "-test.bench", bench, "-test.benchmem",
		"-test.benchtime", benchtime, "-test.count", "1", "-test.timeout", runTimeout,
	}
}

// anchored is the -test.bench pattern that matches exactly the named top-level
// benchmarks and every sub-benchmark under them.
func anchored(names []string) string {
	return "^(" + strings.Join(names, "|") + ")$"
}

// units turns the packages that run into packing units: a whole package, or,
// when the package alone would overrun the budget, each of its top-level
// benchmarks. A top-level benchmark is the smallest unit there is, so one whose
// own estimate is past what a shard job holds fails selection: its author splits
// it. A benchmark only the head holds is no unit, as no shard plans it, but it
// is held to the same limit, so the pull request that adds it is the one that
// fails and not the next that selects its package.
func units(pkgs []*selected, p shardParams) ([]unit, error) {
	var us []unit

	for _, s := range pkgs {
		for _, m := range s.headOnly {
			err := fitsLimit(unit{pkg: s.bin.pkg, names: []string{m.name}, cost: m.cost}, p)
			if err != nil {
				return nil, err
			}
		}

		if len(s.benches) == 0 {
			continue
		}

		whole := unit{pkg: s.bin.pkg, cost: s.cost()}
		if len(s.headOnly) > 0 {
			// The plan names the benchmarks with base rows: the others have none.
			for _, m := range s.benches {
				whole.names = append(whole.names, m.name)
			}
		}

		if p.estimate([]unit{whole}) <= p.budget {
			us = append(us, whole)

			continue
		}

		for _, m := range s.benches {
			u := unit{pkg: s.bin.pkg, names: []string{m.name}, cost: m.cost}

			err := fitsLimit(u, p)
			if err != nil {
				return nil, err
			}

			us = append(us, u)
		}
	}

	return us, nil
}

// fitsLimit fails when a shard job holding only u would run past its timeout.
func fitsLimit(u unit, p shardParams) error {
	est := p.estimate([]unit{u})
	if est <= p.limit {
		return nil
	}

	return fmt.Errorf("%w: %s %s alone is estimated at %s, over the %s a shard job holds: split the benchmark",
		errSelect, u.pkg, strings.Join(u.names, " "), est.Round(time.Second), p.limit)
}

// pack spreads the units over the fewest shards whose estimates all fit the
// budget, longest unit first onto the shard estimated to finish first. A unit
// that is past the budget alone, and within the limit (units refuses one
// that is not), gets a shard of its own: no shard could take it and stay
// within the budget.
func pack(us []unit, p shardParams) []shard {
	var alone []shard

	var rest []unit

	for _, u := range us {
		if est := p.estimate([]unit{u}); est > p.budget {
			alone = append(alone, shard{units: []unit{u}, est: est})
		} else {
			rest = append(rest, u)
		}
	}

	return append(alone, packWithin(rest, p)...)
}

// packWithin packs units that each fit the budget alone into the fewest shards
// that all fit it.
func packWithin(us []unit, p shardParams) []shard {
	if len(us) == 0 {
		return nil
	}

	sorted := slices.Clone(us)
	slices.SortStableFunc(sorted, func(a, b unit) int { return cmp.Compare(b.cost, a.cost) })

	for n := 1; ; n++ {
		shards := spreadUnits(sorted, n, p)
		if n == len(sorted) || !overruns(shards, p.budget) {
			return shards
		}
	}
}

func spreadUnits(sorted []unit, n int, p shardParams) []shard {
	shards := make([]shard, n)

	for _, u := range sorted {
		i := 0

		for j := range shards {
			if p.estimate(append(slices.Clip(shards[j].units), u)) < p.estimate(append(slices.Clip(shards[i].units), u)) {
				i = j
			}
		}

		shards[i].units = append(shards[i].units, u)
		shards[i].est = p.estimate(shards[i].units)
	}

	return shards
}

func overruns(shards []shard, budget time.Duration) bool {
	return slices.ContainsFunc(shards, func(s shard) bool { return s.est > budget })
}

// plan is a shard's BENCH_PLAN: "<import path> <bench regex>" per package, the
// regex "." for a whole package.
func (s shard) plan() string {
	names := map[string][]string{}

	var pkgs []string

	for _, u := range s.units {
		if _, ok := names[u.pkg]; !ok {
			pkgs = append(pkgs, u.pkg)
		}

		names[u.pkg] = append(names[u.pkg], u.names...)
	}

	slices.Sort(pkgs)

	var b strings.Builder

	for _, pkg := range pkgs {
		bench := wholePackage
		if len(names[pkg]) > 0 {
			bench = anchored(slices.Sorted(slices.Values(names[pkg])))
		}

		fmt.Fprintf(&b, "%s %s\n", pkg, bench)
	}

	return b.String()
}

// matrixEntry is one shard job. Index is its place in the matrix, from zero:
// the job names its artifact bench-shard-<index> from it, and join reads that
// name, so neither rests on the order GitHub numbers the jobs in.
type matrixEntry struct {
	Index int    `json:"index"`
	Shard string `json:"shard"`
	Plan  string `json:"plan"`
}

type matrix struct {
	Include []matrixEntry `json:"include"`
}

func writeMatrix(path string, shards []shard) error {
	m := matrix{Include: []matrixEntry{}}
	for i, s := range shards {
		m.Include = append(m.Include, matrixEntry{Index: i, Shard: fmt.Sprintf("%d of %d", i+1, len(shards)), Plan: s.plan()})
	}

	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode matrix: %w", err)
	}

	err = os.WriteFile(path, append(data, '\n'), fileMode)
	if err != nil {
		return fmt.Errorf("write matrix: %w", err)
	}

	return nil
}

// describeHeadOnly lists benchmarks with the time of one run of each.
func describeHeadOnly(benches []benchCost) string {
	parts := make([]string, 0, len(benches))
	for _, m := range benches {
		parts = append(parts, fmt.Sprintf("%s %s", m.name, m.cost.Round(time.Second)))
	}

	return strings.Join(parts, ", ")
}

func writeSelection(out io.Writer, pkgs []*selected, shards []shard, p shardParams) error {
	var b strings.Builder

	for _, s := range pkgs {
		line := fmt.Sprintf("%-16s %s", s.verdict, s.bin.pkg)
		if len(s.benches) > 0 {
			line += fmt.Sprintf(" (one run %s)", s.cost().Round(time.Second))
		}

		if len(s.headOnly) > 0 {
			line += fmt.Sprintf(" (only in the head, timed and not compared: %s)", describeHeadOnly(s.headOnly))
		}

		b.WriteString(line + "\n")
	}

	switch len(shards) {
	case 0:
		b.WriteString("\nNo benchmark package's test binary changed, so nothing is compared.\n")
	default:
		fmt.Fprintf(&b, "\n%d shard(s), each %d rounds of base and head, budget %s:\n", len(shards), p.count, p.budget)

		for i, s := range shards {
			fmt.Fprintf(&b, "shard %d, about %s:\n", i+1, s.est.Round(time.Minute))

			for line := range strings.Lines(s.plan()) {
				b.WriteString("  " + line)
			}
		}
	}

	_, err := io.WriteString(out, b.String())
	if err != nil {
		return fmt.Errorf("write selection: %w", err)
	}

	return nil
}
