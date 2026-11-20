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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Defaults of `bench select`. The Makefile passes its own BENCH_TIME,
// BENCH_COUNT and BENCH_SHARD_BUDGET.
const (
	defaultSelectBenchtime = 500 * time.Millisecond
	defaultSelectCount     = 10
	defaultShardBudget     = 40 * time.Minute
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
)

// buildVerdict is what selection decided about one benchmark package.
type buildVerdict string

const (
	verdictUnchanged      buildVerdict = "unchanged"
	verdictChanged        buildVerdict = "changed"
	verdictUnreproducible buildVerdict = "not reproducible"
	verdictHeadOnly       buildVerdict = "head only"
	verdictBaseOnly       buildVerdict = "base only"
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
	benches []benchCost
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
// binary differs from its base binary, or from its second build (the build is
// not reproducible, so equal bytes would prove nothing). A package only the
// head holds runs once at -benchtime 1x, so a broken benchmark fails here, and
// is not compared. Each package that runs has each of its top-level benchmarks
// run once at -benchtime, timed, and the shards are packed from those times.
// The report goes to out and the GitHub Actions matrix to -matrix.
func selectMain(ctx context.Context, args []string, out io.Writer, run runBin) error {
	cfg, err := parseSelect(args)
	if err != nil {
		return err
	}

	pkgs, err := classify(cfg.base, cfg.head, cfg.again)
	if err != nil {
		return err
	}

	err = measure(ctx, cfg.head, cfg.benchtime, pkgs, run)
	if err != nil {
		return err
	}

	shards := pack(units(pkgs, cfg.params), cfg.params)

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
		v, err := compareBuilds(filepath.Join(headDir, b.bin),
			builtPath(baseDir, base, b.bin), builtPath(againDir, again, b.bin))
		if err != nil {
			return nil, err
		}

		pkgs = append(pkgs, &selected{bin: b, verdict: v})
	}

	for _, b := range base {
		if !inManifest(head, b.bin) {
			pkgs = append(pkgs, &selected{bin: b, verdict: verdictBaseOnly})
		}
	}

	return pkgs, nil
}

func inManifest(bins []benchBin, bin string) bool {
	return slices.ContainsFunc(bins, func(b benchBin) bool { return b.bin == bin })
}

// builtPath is where a build directory holds bin, or "" when its manifest does
// not list it.
func builtPath(dir string, bins []benchBin, bin string) string {
	if !inManifest(bins, bin) {
		return ""
	}

	return filepath.Join(dir, bin)
}

// compareBuilds compares a head binary with the base's and with its own second
// build; a path of "" is a build that does not hold the package.
func compareBuilds(headPath, basePath, againPath string) (buildVerdict, error) {
	if basePath == "" {
		return verdictHeadOnly, nil
	}

	if againPath == "" {
		return verdictUnreproducible, nil
	}

	head, err := fileSum(headPath)
	if err != nil {
		return "", err
	}

	again, err := fileSum(againPath)
	if err != nil {
		return "", err
	}

	if again != head {
		return verdictUnreproducible, nil
	}

	base, err := fileSum(basePath)
	if err != nil {
		return "", err
	}

	if base != head {
		return verdictChanged, nil
	}

	return verdictUnchanged, nil
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

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}

		if len(fields) != manifestFields {
			return nil, fmt.Errorf("%w: %s: manifest line %q is not <binary> <dir> <bench> <package>", errSelect, dir, sc.Text())
		}

		bins = append(bins, benchBin{bin: fields[0], dir: fields[1], pkg: fields[3]})
	}

	return bins, nil
}

// measure runs what selection picked from the head's build: a head-only
// package once at 1x, and every top-level benchmark of a package that runs once
// at benchtime, timed. A failing run fails selection.
func measure(ctx context.Context, binDir string, benchtime time.Duration, pkgs []*selected, run runBin) error {
	for _, s := range pkgs {
		switch s.verdict {
		case verdictHeadOnly:
			_, _, err := run(ctx, binDir, s.bin, benchArgs(wholePackage, "1x")...)
			if err != nil {
				return err
			}
		case verdictChanged, verdictUnreproducible:
			err := measurePackage(ctx, binDir, benchtime, s, run)
			if err != nil {
				return err
			}
		default: // unchanged, or only in the base: nothing to run
		}
	}

	return nil
}

func measurePackage(ctx context.Context, binDir string, benchtime time.Duration, s *selected, run runBin) error {
	list, _, err := run(ctx, binDir, s.bin, "-test.list", "^Benchmark")
	if err != nil {
		return err
	}

	for name := range strings.FieldsSeq(string(list)) {
		if !strings.HasPrefix(name, "Benchmark") {
			continue
		}

		_, elapsed, err := run(ctx, binDir, s.bin, benchArgs(anchored([]string{name}), benchtime.String())...)
		if err != nil {
			return err
		}

		s.benches = append(s.benches, benchCost{name: name, cost: elapsed})
	}

	if len(s.benches) == 0 {
		return fmt.Errorf("%w: %s lists no benchmark", errSelect, s.bin.pkg)
	}

	return nil
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
// when the package alone would overrun a shard, each of its top-level
// benchmarks.
func units(pkgs []*selected, p shardParams) []unit {
	var us []unit

	for _, s := range pkgs {
		if len(s.benches) == 0 {
			continue
		}

		whole := unit{pkg: s.bin.pkg, cost: s.cost()}
		if len(s.benches) == 1 || p.estimate([]unit{whole}) <= p.budget {
			us = append(us, whole)

			continue
		}

		for _, m := range s.benches {
			us = append(us, unit{pkg: s.bin.pkg, names: []string{m.name}, cost: m.cost})
		}
	}

	return us
}

// pack spreads the units over the fewest shards whose estimates all fit the
// budget, longest unit first onto the shard estimated to finish first. A unit
// that overruns the budget on its own raises the limit to its own estimate.
func pack(us []unit, p shardParams) []shard {
	if len(us) == 0 {
		return nil
	}

	sorted := slices.Clone(us)
	slices.SortStableFunc(sorted, func(a, b unit) int { return cmp.Compare(b.cost, a.cost) })

	limit := max(p.budget, p.estimate(sorted[:1]))

	for n := 1; ; n++ {
		shards := spreadUnits(sorted, n, p)
		if n == len(sorted) || !overruns(shards, limit) {
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

type matrixEntry struct {
	Shard string `json:"shard"`
	Plan  string `json:"plan"`
}

type matrix struct {
	Include []matrixEntry `json:"include"`
}

func writeMatrix(path string, shards []shard) error {
	m := matrix{Include: []matrixEntry{}}
	for i, s := range shards {
		m.Include = append(m.Include, matrixEntry{Shard: fmt.Sprintf("%d of %d", i+1, len(shards)), Plan: s.plan()})
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

func writeSelection(out io.Writer, pkgs []*selected, shards []shard, p shardParams) error {
	var b strings.Builder

	for _, s := range pkgs {
		line := fmt.Sprintf("%-16s %s", s.verdict, s.bin.pkg)
		if len(s.benches) > 0 {
			line += fmt.Sprintf(" (one run %s)", s.cost().Round(time.Second))
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
