package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errBrokenBenchmark = errors.New("benchmark failed")

// buildDir writes a bench-build directory: a manifest line and a binary with
// the given bytes per package, and an empty package directory at
// <dir>/src/<package> for each, which writeData fills.
func buildDir(t *testing.T, bins map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	var manifest strings.Builder

	for _, pkg := range slices.Sorted(func(yield func(string) bool) {
		for k := range bins {
			if !yield(k) {
				return
			}
		}
	}) {
		bin := strings.ReplaceAll(pkg, "/", "_") + ".test"
		require.NoError(t, os.WriteFile(filepath.Join(dir, bin), []byte(bins[pkg]), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", pkg), 0o750))
		fmt.Fprintf(&manifest, "%s %s . %s\n", bin, filepath.Join(dir, "src", pkg), pkg)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest.String()), 0o600))

	return dir
}

// writeData writes a file into a package directory of a buildDir.
func writeData(t *testing.T, dir, pkg, name, content string) {
	t.Helper()

	path := filepath.Join(dir, "src", pkg, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func verdicts(pkgs []*selected) map[string]buildVerdict {
	got := map[string]buildVerdict{}
	for _, s := range pkgs {
		got[s.bin.pkg] = s.verdict
	}

	return got
}

const (
	pkgSame     = "p/same"
	pkgEdited   = "p/edited"
	pkgFlaky    = "p/flaky"
	pkgRemoved  = "p/removed"
	pkgUnpaired = "p/unpaired"
	pkgAdded    = "p/added"
	pkgSNS      = "svc/sns"
	pkgRoute    = "svc/route"
)

func Test_classify(t *testing.T) {
	base := buildDir(t, map[string]string{pkgSame: "a", pkgEdited: "b", pkgFlaky: "c", pkgRemoved: "d", pkgUnpaired: "e"})
	head := buildDir(t, map[string]string{pkgSame: "a", pkgEdited: "B", pkgFlaky: "c", pkgAdded: "n", pkgUnpaired: "e"})
	again := buildDir(t, map[string]string{pkgSame: "a", pkgEdited: "B", pkgFlaky: "c2", pkgAdded: "n"})

	pkgs, err := classify(base, head, again)
	require.NoError(t, err)

	assert.Equal(t, map[string]buildVerdict{
		pkgSame:     verdictUnchanged,
		pkgEdited:   verdictChanged,
		pkgFlaky:    verdictUnreproducible,
		pkgUnpaired: verdictUnreproducible,
		pkgAdded:    verdictHeadOnly,
		pkgRemoved:  verdictBaseOnly,
	}, verdicts(pkgs))
}

// A benchmark may read a file of its package directory at run time. The binary
// does not hold it, so selection hashes the files of the directory that are not
// Go source.
func Test_classify_reads_the_files_a_package_holds(t *testing.T) {
	const (
		pkgTestdata = "p/testdata"
		pkgNested   = "p/nested"
		pkgFixture  = "p/fixture"
		pkgReadme   = "p/readme"
		pkgGoData   = "p/godata"
		pkgGoSrc    = "p/gosrc"
		pkgNew      = "p/newfile"
		pkgParent   = "p/parent"
	)

	// Every package builds to the same binary on both sides.
	bins := map[string]string{}
	for _, pkg := range []string{
		pkgTestdata, pkgNested, pkgFixture, pkgReadme, pkgGoData, pkgGoSrc, pkgNew, pkgSame, pkgParent,
	} {
		bins[pkg] = "a"
	}

	base, head := buildDir(t, bins), buildDir(t, bins)

	for _, dir := range []string{base, head} {
		writeData(t, dir, pkgTestdata, "testdata/in.json", `{"n":1}`)
		writeData(t, dir, pkgNested, "testdata/deep/er/in.json", "1")
		writeData(t, dir, pkgFixture, "fixtures/in.yaml", "1")
		writeData(t, dir, pkgReadme, "README.md", "1")
		writeData(t, dir, pkgGoData, "testdata/gen/input.go", "package gen")
		writeData(t, dir, pkgGoSrc, "src.go", "package x // 1")
		writeData(t, dir, pkgSame, "testdata/in.json", "1")
		writeData(t, dir, pkgParent, "child/child.go", "package child")
		writeData(t, dir, pkgParent, "child/data.json", "1")
	}

	// The binary stays the same bytes in every case: only a file the compiler
	// did not read changes. A Go file outside testdata is in the binary, so on
	// its own it proves nothing here.
	writeData(t, head, pkgTestdata, "testdata/in.json", `{"n":2}`)
	writeData(t, head, pkgNested, "testdata/deep/er/in.json", "2")
	writeData(t, head, pkgFixture, "fixtures/in.yaml", "2")
	writeData(t, head, pkgReadme, "README.md", "2")
	writeData(t, head, pkgGoData, "testdata/gen/input.go", "package gen // 2")
	writeData(t, head, pkgGoSrc, "src.go", "package x // 2")
	writeData(t, head, pkgNew, "testdata/added.json", "1")
	// child is a package of its own, with a verdict of its own.
	writeData(t, head, pkgParent, "child/data.json", "2")

	pkgs, err := classify(base, head, head)
	require.NoError(t, err)

	assert.Equal(t, map[string]buildVerdict{
		pkgTestdata: verdictChanged,
		pkgNested:   verdictChanged,
		pkgFixture:  verdictChanged,
		pkgReadme:   verdictChanged,
		pkgGoData:   verdictChanged,
		pkgNew:      verdictChanged,
		pkgGoSrc:    verdictUnchanged,
		pkgSame:     verdictUnchanged,
		pkgParent:   verdictUnchanged,
	}, verdicts(pkgs))
}

func Test_classify_sees_a_removed_file(t *testing.T) {
	bins := map[string]string{pkgSame: "a"}
	base, head := buildDir(t, bins), buildDir(t, bins)
	writeData(t, base, pkgSame, "testdata/gone.json", "1")

	pkgs, err := classify(base, head, head)
	require.NoError(t, err)

	assert.Equal(t, verdictChanged, verdicts(pkgs)[pkgSame])
}

func Test_readManifest_refuses_an_old_manifest(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), []byte("x.test /src/x\n"), 0o600))

	_, err := readManifest(dir)
	require.ErrorIs(t, err, errSelect)
}

// Two import paths that one name stands for would build to one file, and the
// second build would replace the first, so selection would compare a package
// with another's binary.
func Test_readManifest_refuses_two_packages_with_one_binary_name(t *testing.T) {
	dir := t.TempDir()
	manifest := "internal_zz_a.test /src/a . internal/zz/a\ninternal_zz_a.test /src/b . internal/zz_a\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest), 0o600))

	_, err := readManifest(dir)
	require.ErrorIs(t, err, errSelect)
	require.ErrorContains(t, err, "internal/zz/a")
	require.ErrorContains(t, err, "internal/zz_a")

	manifest = "x.test /src/a . p/x\nX.test /src/b . p/X\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest), 0o600))

	_, err = readManifest(dir)
	require.ErrorIs(t, err, errSelect, "a case-insensitive file system writes both names to one file")
}

// fakeRunner stands in for the test binaries: each package lists its
// benchmarks, and each benchmark takes the given time at any benchtime but 1x.
type fakeRunner struct {
	benches map[string]map[string]time.Duration // package, benchmark, cost
	broken  string                              // a package whose benchmarks fail
	// baseHolds is what the base's binary of a package lists, where it is not
	// what the head's lists.
	baseHolds map[string][]string
	// failsLate is a benchmark of a package that fails on its 101st iteration:
	// a run of two iterations passes, one at the gate's benchtime does not.
	failsLate map[string]string
	baseDir   string
	calls     []string
}

func (f *fakeRunner) run(_ context.Context, binDir string, b benchBin, args ...string) ([]byte, time.Duration, error) {
	call := b.pkg + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)

	if b.pkg == f.broken {
		return nil, 0, fmt.Errorf("%s: %w", call, errBrokenBenchmark)
	}

	if name, ok := f.failsLate[b.pkg]; ok && args[0] != "-test.list" &&
		args[slices.Index(args, "-test.benchtime")+1] != "2x" && strings.Contains(call, name) {
		return nil, 0, fmt.Errorf("%s: iteration 101: %w", call, errBrokenBenchmark)
	}

	if args[0] == "-test.list" {
		if held, ok := f.baseHolds[b.pkg]; ok && binDir == f.baseDir {
			return []byte(strings.Join(held, "\n") + "\n"), 0, nil
		}

		return []byte(strings.Join(slices.Sorted(func(yield func(string) bool) {
			for k := range f.benches[b.pkg] {
				if !yield(k) {
					return
				}
			}
		}), "\n") + "\n"), 0, nil
	}

	bench := args[slices.Index(args, "-test.bench")+1]
	for name, cost := range f.benches[b.pkg] {
		if bench == "^("+name+")$" {
			return nil, cost, nil
		}
	}

	return nil, time.Millisecond, nil
}

func runSelect(t *testing.T, f *fakeRunner, base, head map[string]string) (string, matrix, error) {
	t.Helper()

	baseDir, headDir := buildDir(t, base), buildDir(t, head)
	path := filepath.Join(t.TempDir(), "matrix.json")
	f.baseDir = baseDir

	var out bytes.Buffer

	err := selectMain(t.Context(), []string{
		flagBase, baseDir, flagHead, headDir, "-again", headDir, "-matrix", path,
		"-count", "10", "-budget", "40m", "-setup", "5m", "-link", "1s",
	}, &out, f.run)
	if err != nil {
		return out.String(), matrix{}, err
	}

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var m matrix
	require.NoError(t, json.Unmarshal(data, &m))

	return out.String(), m, nil
}

func Test_selectMain(t *testing.T) {
	t.Run("a docs-only change selects nothing and writes an empty matrix", func(t *testing.T) {
		f := &fakeRunner{}
		bins := map[string]string{"x": "1", "y": "2"}

		out, m, err := runSelect(t, f, bins, bins)
		require.NoError(t, err)

		assert.Empty(t, m.Include)
		assert.NotNil(t, m.Include, "the matrix is an empty list, which GitHub reads, not null")
		assert.Empty(t, f.calls, "nothing runs")
		assert.Contains(t, out, "nothing is compared")
	})

	t.Run("a changed package is timed and compared, a new one is timed and not compared", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{
			"x": {benchA: time.Second, benchB: 2 * time.Second},
			"n": {benchA: time.Second},
		}}

		base := map[string]string{"x": "1", "y": "2"}

		out, m, err := runSelect(t, f, base, map[string]string{"x": "1+", "y": "2", "n": "3"})
		require.NoError(t, err)

		require.Equal(t, []matrixEntry{{Index: 0, Shard: "1 of 1", Plan: "x .\n"}}, m.Include)
		assert.Contains(t, f.calls,
			"n -test.run ^$ -test.bench ^(BenchmarkA)$ -test.benchmem -test.benchtime 500ms -test.count 1 -test.timeout 30m",
			"a new package's benchmark runs at the gate's benchtime")
		assert.Contains(t, f.calls,
			"x -test.run ^$ -test.bench ^(BenchmarkA)$ -test.benchmem -test.benchtime 500ms -test.count 1 -test.timeout 30m")
		assert.NotContains(t, strings.Join(f.calls, "\n"), "y -test", "an unchanged package does not run")
		assert.Contains(t, out, "head only")
		assert.Contains(t, out, "only in the head, timed and not compared: BenchmarkA 1s")
	})

	t.Run("a broken benchmark in a new package fails selection", func(t *testing.T) {
		f := &fakeRunner{broken: "n"}

		_, _, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "1", "n": "3"})
		require.ErrorIs(t, err, errBrokenBenchmark)
	})

	t.Run("a benchmark past what a shard job holds fails selection with its name and estimate", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{
			"x": {"BenchmarkSlow": 3 * time.Minute, "BenchmarkQuick": time.Second},
		}}

		_, _, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
		require.ErrorIs(t, err, errSelect)
		// 5m setup + 10 rounds of 2 * 180s and a 1s link is 1h5m10s.
		require.ErrorContains(t, err, "BenchmarkSlow")
		require.ErrorContains(t, err, "1h5m10s")
		assert.NotContains(t, err.Error(), "BenchmarkQuick")
	})

	t.Run("a package of one benchmark past what a shard job holds fails selection too", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{"x": {"BenchmarkOnly": 3 * time.Minute}}}

		_, _, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
		require.ErrorIs(t, err, errSelect)
		require.ErrorContains(t, err, "BenchmarkOnly")
	})

	// 5m of setup and ten rounds of two runs is 40m at 104 s a run. The job
	// holds 60m, so 110 s a run, 41m55s, runs, in a shard of its own.
	t.Run("a benchmark past the budget and within the job's timeout gets a shard of its own", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{
			"x": {"BenchmarkBig": 110 * time.Second, benchA: time.Second},
		}}

		out, m, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
		require.NoError(t, err)

		require.Equal(t, []matrixEntry{
			{Index: 0, Shard: "1 of 2", Plan: "x ^(BenchmarkBig)$\n"},
			{Index: 1, Shard: "2 of 2", Plan: "x ^(" + benchA + ")$\n"},
		}, m.Include)
		assert.Contains(t, out, "about 42m0s")
	})
}

// The Makefile picks the packages to build with a grep for "func Benchmark",
// which a fixture string in a _test.go file satisfies. The test binary's list is
// the authority: a package that lists no benchmark is dropped, printed as
// "no benchmark", and selects nothing, whichever trees hold it. A binary whose
// list fails still fails selection.
func Test_selectMain_drops_a_package_whose_binary_lists_no_benchmark(t *testing.T) {
	// "n" has no entry in the runner's benches: its binary lists nothing.
	t.Run("a package only the head holds", func(t *testing.T) {
		f := &fakeRunner{}

		out, m, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "1", "n": "3"})
		require.NoError(t, err)

		assert.Empty(t, m.Include)
		assert.Contains(t, out, "no benchmark     n\n")
		assert.NotContains(t, out, "head only")
		assert.Equal(t, []string{"n -test.list ^Benchmark"}, f.calls, "only the list runs, no benchmark does")
	})

	t.Run("a changed package", func(t *testing.T) {
		f := &fakeRunner{}

		out, m, err := runSelect(t, f, map[string]string{"n": "1"}, map[string]string{"n": "2"})
		require.NoError(t, err)

		assert.Empty(t, m.Include)
		assert.Contains(t, out, "no benchmark     n\n")
		assert.NotContains(t, out, "changed  ")
		assert.Equal(t, []string{"n -test.list ^Benchmark"}, f.calls)
	})

	t.Run("a package only the base holds", func(t *testing.T) {
		f := &fakeRunner{}

		out, m, err := runSelect(t, f, map[string]string{"x": "1", "n": "3"}, map[string]string{"x": "1"})
		require.NoError(t, err)

		assert.Empty(t, m.Include)
		assert.Contains(t, out, "no benchmark     n\n")
		assert.NotContains(t, out, "base only")
	})

	t.Run("a package that lists benchmarks beside it still runs", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{"x": {benchA: time.Second}}}

		out, m, err := runSelect(t, f, map[string]string{"x": "1", "n": "3"}, map[string]string{"x": "2", "n": "3"})
		require.NoError(t, err)

		require.Len(t, m.Include, 1)
		assert.Equal(t, "x .\n", m.Include[0].Plan)
		assert.NotContains(t, out, "no benchmark", "an unchanged package is not listed")
	})

	t.Run("a binary that cannot list its benchmarks fails selection", func(t *testing.T) {
		for name, trees := range map[string][2]map[string]string{
			"head only": {{"x": "1"}, {"x": "1", "n": "3"}},
			"changed":   {{"n": "1"}, {"n": "2"}},
			"base only": {{"x": "1", "n": "3"}, {"x": "1"}},
		} {
			t.Run(name, func(t *testing.T) {
				_, _, err := runSelect(t, &fakeRunner{broken: "n"}, trees[0], trees[1])
				require.ErrorIs(t, err, errBrokenBenchmark)
			})
		}
	})
}

// A benchmark only the head holds has no base rows, and a shard that held it
// would fail the join, so no shard plans it. It is still run once at the gate's
// benchtime, as main runs it, and held to the shard job's limit: one that breaks
// on its 101st iteration or runs too long would otherwise land green and fail
// the next pull request that selects its package. A package only the head holds
// and a benchmark only the head holds in a package that runs both count.
func Test_selectMain_times_a_benchmark_only_the_head_holds(t *testing.T) {
	const (
		benchLate = "BenchmarkLate"
		benchHuge = "BenchmarkHuge"
	)

	late := func(pkg string) map[string]string { return map[string]string{pkg: benchLate} }
	huge := map[string]time.Duration{benchHuge: 3 * time.Minute}

	t.Run("one that fails on its 101st iteration fails selection", func(t *testing.T) {
		for name, tc := range map[string]struct {
			f          *fakeRunner
			base, head map[string]string
		}{
			"in a new package": {
				f: &fakeRunner{
					benches:   map[string]map[string]time.Duration{"n": {benchLate: time.Second}},
					failsLate: late("n"),
				},
				base: map[string]string{"x": "1"}, head: map[string]string{"x": "1", "n": "3"},
			},
			"in a changed package": {
				f: &fakeRunner{
					benches:   map[string]map[string]time.Duration{"x": {benchA: time.Second, benchLate: time.Second}},
					baseHolds: map[string][]string{"x": {benchA}},
					failsLate: late("x"),
				},
				base: map[string]string{"x": "1"}, head: map[string]string{"x": "2"},
			},
		} {
			t.Run(name, func(t *testing.T) {
				_, _, err := runSelect(t, tc.f, tc.base, tc.head)
				require.ErrorIs(t, err, errBrokenBenchmark)
				require.ErrorContains(t, err, benchLate)
			})
		}
	})

	t.Run("one estimated past what a shard job holds fails selection", func(t *testing.T) {
		for name, tc := range map[string]struct {
			f          *fakeRunner
			base, head map[string]string
		}{
			"in a new package": {
				f:    &fakeRunner{benches: map[string]map[string]time.Duration{"n": huge}},
				base: map[string]string{"x": "1"}, head: map[string]string{"x": "1", "n": "3"},
			},
			"in a changed package": {
				f: &fakeRunner{
					benches:   map[string]map[string]time.Duration{"x": {benchA: time.Second, benchHuge: 3 * time.Minute}},
					baseHolds: map[string][]string{"x": {benchA}},
				},
				base: map[string]string{"x": "1"}, head: map[string]string{"x": "2"},
			},
		} {
			t.Run(name, func(t *testing.T) {
				_, _, err := runSelect(t, tc.f, tc.base, tc.head)
				require.ErrorIs(t, err, errSelect)
				// 5m setup + 10 rounds of 2 * 180s and a 1s link is 1h5m10s.
				require.ErrorContains(t, err, benchHuge)
				require.ErrorContains(t, err, "1h5m10s")
			})
		}
	})

	t.Run("one within the limit is run, reported, and planned in no shard", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{"n": {benchLate: 110 * time.Second}}}

		out, m, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "1", "n": "3"})
		require.NoError(t, err)

		assert.Empty(t, m.Include)
		assert.Contains(t, out, "only in the head, timed and not compared: "+benchLate+" 1m50s")
	})
}

// A pull request that adds a top-level benchmark to a package already too large
// for one shard. The new benchmark has no base rows, so a shard that held it
// alone would fail the join: selection runs it once at the gate's benchtime, as
// it does a package only the head holds, and plans the benchmarks the base has.
func Test_selectMain_plans_no_benchmark_only_the_head_holds(t *testing.T) {
	const benchC = "BenchmarkC"

	f := &fakeRunner{
		benches: map[string]map[string]time.Duration{
			"x": {benchA: 100 * time.Second, benchB: 100 * time.Second, benchC: 100 * time.Second},
		},
		baseHolds: map[string][]string{"x": {benchA, benchB}},
	}

	// Timed, A and B take 5m + 10 rounds of 2 * 200 s: 71 minutes in one shard.
	out, m, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
	require.NoError(t, err)

	assert.Contains(t, f.calls, "x -test.run ^$ -test.bench ^("+benchC+")$ -test.benchmem "+
		"-test.benchtime 500ms -test.count 1 -test.timeout 30m")
	assert.Contains(t, out, "only in the head, timed and not compared: "+benchC+" 1m40s")

	plans := make([]string, 0, len(m.Include))
	for _, e := range m.Include {
		plans = append(plans, e.Plan)
	}

	require.ElementsMatch(t, []string{"x ^(" + benchA + ")$\n", "x ^(" + benchB + ")$\n"}, plans,
		"the shards hold the benchmarks the base has, and none holds the new one alone")

	// The shards' output, as bench-ab writes it for those plans, joins and gates.
	dir := t.TempDir()

	for i, e := range m.Include {
		name := strings.TrimSuffix(strings.TrimPrefix(e.Plan, "x ^("), ")$\n")
		writeShard(t, dir, i, shardOutput("x", name+"-4"), shardOutput("x", name+"-4"))
	}

	matrixPath := writeMatrixFile(t, plans...)
	_, _, err = runJoin(t, matrixPath, dir)
	require.NoError(t, err)

	const benchstat = `goos: linux
pkg: x
cpu: Cpu A
       │  base.txt   │           head.txt            │
       │   sec/op    │   sec/op     vs base          │
BenchmarkA-4   100.1n ± 0%   100.2n ± 0%  ~ (p=0.784 n=10)
BenchmarkB-4   100.1n ± 0%   100.2n ± 0%  ~ (p=0.784 n=10)
`

	_, err = runGateOn(t, benchstat, "[]\n", history(false), commitFlags(baseCommit, headCommit)...)
	require.NoError(t, err)

	t.Run("a whole package that holds one names its timed benchmarks", func(t *testing.T) {
		f := &fakeRunner{
			benches:   map[string]map[string]time.Duration{"x": {benchA: time.Second, benchB: time.Second, benchC: time.Second}},
			baseHolds: map[string][]string{"x": {benchA, benchB}},
		}

		_, m, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
		require.NoError(t, err)

		require.Equal(t, []matrixEntry{{Shard: "1 of 1", Plan: "x ^(" + benchA + "|" + benchB + ")$\n"}}, m.Include)
	})

	t.Run("a package whose benchmarks are all new in the head is timed and not planned", func(t *testing.T) {
		f := &fakeRunner{
			benches:   map[string]map[string]time.Duration{"x": {benchC: time.Second}},
			baseHolds: map[string][]string{"x": {benchA}},
		}

		_, m, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
		require.NoError(t, err)

		assert.Empty(t, m.Include)
		assert.Contains(t, f.calls,
			"x -test.run ^$ -test.bench ^("+benchC+")$ -test.benchmem -test.benchtime 500ms -test.count 1 -test.timeout 30m")
	})
}

// testParams is the shard job of bench.yml: 40 minutes to pack to, 60 to run.
var testParams = shardParams{
	count: 10, budget: 40 * time.Minute, limit: 60 * time.Minute, setup: 5 * time.Minute, link: time.Second,
}

func Test_pack(t *testing.T) {
	p := testParams

	t.Run("packages that fit one shard share it", func(t *testing.T) {
		shards := pack([]unit{{pkg: "a", cost: 30 * time.Second}, {pkg: "b", cost: 20 * time.Second}}, p)

		require.Len(t, shards, 1)
		assert.Equal(t, "a .\nb .\n", shards[0].plan())
		assert.Equal(t, 5*time.Minute+10*(100*time.Second+2*time.Second), shards[0].est)
	})

	t.Run("more than one shard holds is spread over the fewest that fit", func(t *testing.T) {
		// 205 s a round per tree, the base of run 44689662786, is 68 minutes
		// of rounds: two shards of 40 minutes cannot hold it, three can.
		pkgs := []string{"svc/s3", "svc/sqs", "svc/persist", "svc/ssm", "svc/secretsmanager", "svc/api"}
		costs := []int{70, 45, 40, 25, 20, 5}

		us := make([]unit, 0, len(pkgs))
		for i, pkg := range pkgs {
			us = append(us, unit{pkg: pkg, cost: time.Duration(costs[i]) * time.Second})
		}

		shards := pack(us, p)

		require.Len(t, shards, 3)

		var got []string

		for _, s := range shards {
			assert.LessOrEqual(t, s.est, p.budget)

			for line := range strings.Lines(s.plan()) {
				got = append(got, strings.Fields(line)[0])
			}
		}

		assert.ElementsMatch(t, pkgs, got, "every unit lands once")
	})

	t.Run("nothing to run is no shard", func(t *testing.T) {
		assert.Empty(t, pack(nil, p))
	})

	// 110 s a run is 41m55s: past the budget, within the job's timeout. No shard
	// can take it and stay within the budget, so it is not counted against the
	// others, which still share a shard.
	t.Run("a unit past the budget gets a shard of its own and does not split the others", func(t *testing.T) {
		big := unit{pkg: "svc/big", cost: 110 * time.Second}
		shards := pack([]unit{
			{pkg: "a", cost: 20 * time.Second}, big, {pkg: "b", cost: 20 * time.Second},
		}, p)

		require.Len(t, shards, 2)
		assert.Equal(t, []unit{big}, shards[0].units)
		assert.Equal(t, 5*time.Minute+10*(220*time.Second+time.Second), shards[0].est)
		assert.Equal(t, "a .\nb .\n", shards[1].plan())
	})
}

// The limit is what the job's timeout holds: 5m of setup and ten rounds of two
// runs and a link fit 60m up to 164 s a run, and not at 165 s.
func Test_units_fails_a_benchmark_past_the_jobs_timeout(t *testing.T) {
	p := testParams

	pkg := func(cost time.Duration) []*selected {
		return []*selected{{
			bin: benchBin{pkg: pkgRoute}, verdict: verdictChanged,
			benches: []benchCost{{name: benchA, cost: cost}, {name: benchB, cost: time.Second}},
		}}
	}

	us, err := units(pkg(164*time.Second), p)
	require.NoError(t, err)
	assert.Contains(t, us, unit{pkg: pkgRoute, names: []string{benchA}, cost: 164 * time.Second})

	_, err = units(pkg(165*time.Second), p)
	require.ErrorIs(t, err, errSelect)
	require.ErrorContains(t, err, benchA)
	require.ErrorContains(t, err, "1h0m10s")
}

func Test_units_splits_a_package_one_shard_cannot_hold(t *testing.T) {
	p := testParams

	// The SNS package: about 150 s a round per tree on the runner, 50 minutes
	// of rounds on its own.
	sns := &selected{bin: benchBin{pkg: pkgSNS}, verdict: verdictChanged, benches: []benchCost{
		{name: "BenchmarkPublish", cost: 90 * time.Second},
		{name: "BenchmarkSubscribe", cost: 40 * time.Second},
		{name: "BenchmarkFilter", cost: 20 * time.Second},
	}}
	small := &selected{bin: benchBin{pkg: pkgRoute}, verdict: verdictChanged, benches: []benchCost{
		{name: benchA, cost: time.Second}, {name: benchB, cost: time.Second},
	}}
	skipped := &selected{bin: benchBin{pkg: "svc/store"}, verdict: verdictUnchanged}

	us, err := units([]*selected{sns, small, skipped}, p)
	require.NoError(t, err)

	require.Equal(t, []unit{
		{pkg: pkgSNS, names: []string{"BenchmarkPublish"}, cost: 90 * time.Second},
		{pkg: pkgSNS, names: []string{"BenchmarkSubscribe"}, cost: 40 * time.Second},
		{pkg: pkgSNS, names: []string{"BenchmarkFilter"}, cost: 20 * time.Second},
		{pkg: pkgRoute, cost: 2 * time.Second},
	}, us)

	shards := pack(us, p)
	require.Len(t, shards, 2)

	plans := make([]string, 0, len(shards))
	for _, s := range shards {
		assert.LessOrEqual(t, s.est, p.budget)
		plans = append(plans, s.plan())
	}

	assert.ElementsMatch(t, []string{
		pkgSNS + " ^(BenchmarkPublish)$\n",
		pkgRoute + " .\n" + pkgSNS + " ^(BenchmarkFilter|BenchmarkSubscribe)$\n",
	}, plans)
}

// Shards run on runners whose cpu lines differ. benchstat then sets a package's
// rows from each runner in a table of its own, under one pkg line: the replayed
// output joins two shards, the second holding one benchmark of example.com/x on
// another cpu, 20% slower in the head. The gate must still read it as
// example.com/x's.
func Test_gate_reads_shards_from_two_cpus(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "shards-two-cpus.txt"))
	require.NoError(t, err)
	require.Contains(t, string(text), "cpu: Cpu B")

	rows, err := findRegressions(bytes.NewReader(text))
	require.NoError(t, err)

	require.Len(t, rows, 1)
	assert.Equal(t, "example.com/x", rows[0].pkg)
	assert.Equal(t, "Slow-4 sec/op +20.17% (p=0.000)", rows[0].String())

	accepted := benchstatEntry("example.com/x", "Slow", unitSecOp, "+25%")

	out, err := runGateOn(t, string(text), accepted, history(false), commitFlags(baseCommit, headCommit)...)
	require.NoError(t, err, "the entry for example.com/x holds the row from the second cpu")
	assert.Contains(t, out, "Slow-4")

	_, err = runGateOn(t, string(text), "[]\n", history(false), commitFlags(baseCommit, headCommit)...)
	require.ErrorIs(t, err, errRegression)
}
