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
	)

	// Every package builds to the same binary on both sides.
	bins := map[string]string{}
	for _, pkg := range []string{pkgTestdata, pkgNested, pkgFixture, pkgReadme, pkgGoData, pkgGoSrc, pkgNew, pkgSame} {
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
	calls   []string
}

func (f *fakeRunner) run(_ context.Context, _ string, b benchBin, args ...string) ([]byte, time.Duration, error) {
	call := b.pkg + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)

	if b.pkg == f.broken {
		return nil, 0, fmt.Errorf("%s: %w", call, errBrokenBenchmark)
	}

	if args[0] == "-test.list" {
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

	t.Run("a changed package is timed and compared, a new one runs twice at 2x", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{
			"x": {benchA: time.Second, benchB: 2 * time.Second},
		}}

		base := map[string]string{"x": "1", "y": "2"}

		_, m, err := runSelect(t, f, base, map[string]string{"x": "1+", "y": "2", "n": "3"})
		require.NoError(t, err)

		require.Equal(t, []matrixEntry{{Shard: "1 of 1", Plan: "x .\n"}}, m.Include)
		assert.Contains(t, f.calls,
			"n -test.run ^$ -test.bench . -test.benchmem -test.benchtime 2x -test.count 1 -test.timeout 30m")
		assert.Contains(t, f.calls,
			"x -test.run ^$ -test.bench ^(BenchmarkA)$ -test.benchmem -test.benchtime 500ms -test.count 1 -test.timeout 30m")
		assert.NotContains(t, strings.Join(f.calls, "\n"), "y -test", "an unchanged package does not run")
	})

	t.Run("a broken benchmark in a new package fails selection", func(t *testing.T) {
		f := &fakeRunner{broken: "n"}

		_, _, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "1", "n": "3"})
		require.ErrorIs(t, err, errBrokenBenchmark)
	})

	t.Run("a benchmark estimated past the shard budget fails selection with its name and estimate", func(t *testing.T) {
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

	t.Run("a package of one benchmark past the shard budget fails selection too", func(t *testing.T) {
		f := &fakeRunner{benches: map[string]map[string]time.Duration{"x": {"BenchmarkOnly": 3 * time.Minute}}}

		_, _, err := runSelect(t, f, map[string]string{"x": "1"}, map[string]string{"x": "2"})
		require.ErrorIs(t, err, errSelect)
		require.ErrorContains(t, err, "BenchmarkOnly")
	})
}

func Test_pack(t *testing.T) {
	p := shardParams{count: 10, budget: 40 * time.Minute, setup: 5 * time.Minute, link: time.Second}

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
}

func Test_units_splits_a_package_one_shard_cannot_hold(t *testing.T) {
	p := shardParams{count: 10, budget: 40 * time.Minute, setup: 5 * time.Minute, link: time.Second}

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
