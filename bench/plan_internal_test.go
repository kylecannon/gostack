package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The plan crosses from Go to the Makefile: `bench select` writes it
// (shard.plan), `make bench-build` reads it with awk and writes the manifest
// `bench select` reads back, and `bench join` parses it again. These tests run
// the real Makefile on a small module, so a change to either side's format
// fails here.

const (
	pkgFixA     = "fixture/a"
	pkgFixB     = "fixture/b"
	pkgFixZZ    = "fixture/zz/a"
	pkgFixZZ2   = "fixture/zz_a"
	pkgFixNoOne = "fixture/nobench"
)

// fixtureModule writes a module of packages that hold benchmarks, in a
// temporary directory, with the Go pin of this repository.
func fixtureModule(t *testing.T) string {
	t.Helper()

	_, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}

	root, err := os.ReadFile(filepath.Join("..", "go.mod"))
	require.NoError(t, err)

	pins := regexp.MustCompile(`(?m)^(go|toolchain) .*$`).FindAllString(string(root), -1)
	mod := "module fixture\n\n" + strings.Join(pins, "\n") + "\n"

	files := map[string]string{
		"go.mod": mod,
		"a/a.go": "package a\n",
		"a/a_test.go": `package a

import "testing"

func BenchmarkA(b *testing.B) { for range b.N {} }
func BenchmarkB(b *testing.B) { for range b.N {} }
`,
		"b/b.go": "package b\n",
		"b/b_test.go": `package b

import "testing"

func BenchmarkC(b *testing.B) {
	b.Run("size=1k", func(b *testing.B) { for range b.N {} })
}
`,
		"zz/a/a.go": "package a\n",
		"zz/a/a_test.go": `package a

import "testing"

func BenchmarkZ(b *testing.B) { for range b.N {} }
`,
		"zz_a/a.go": "package zz_a\n",
		"zz_a/a_test.go": `package zz_a

import "testing"

func BenchmarkZ(b *testing.B) { for range b.N {} }
`,
		"nobench/n.go": "package nobench\n",
		"nobench/n_test.go": `package nobench

import "testing"

func TestN(t *testing.T) {}
`,
	}

	dir := t.TempDir()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	return dir
}

// runMake runs one target of this repository's Makefile in a tree.
func runMake(t *testing.T, tree, target string, vars ...string) (string, error) {
	t.Helper()

	makefile, err := filepath.Abs(filepath.Join("..", "Makefile"))
	require.NoError(t, err)

	args := append([]string{"-s", "--no-print-directory", "-C", tree, "-f", makefile, target}, vars...)

	var out bytes.Buffer

	cmd := exec.CommandContext(t.Context(), "make", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()

	return out.String(), err
}

func writePlan(t *testing.T, text string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "plan")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))

	return path
}

// plannedShard is a shard that holds a split package and whole ones.
func plannedShard() shard {
	return shard{units: []unit{
		{pkg: pkgFixA, names: []string{benchB}},
		{pkg: pkgFixB},
		{pkg: pkgFixZZ},
		{pkg: pkgFixZZ2},
	}}
}

func Test_plan_builds_what_it_names(t *testing.T) {
	tree := fixtureModule(t)
	plan := plannedShard().plan()
	bin := filepath.Join(t.TempDir(), "bin")

	out, err := runMake(t, tree, "bench-build", "BENCH_BIN="+bin, "BENCH_PLAN="+writePlan(t, plan))
	require.NoError(t, err, out)

	pkgs, err := readManifest(bin)
	require.NoError(t, err)

	regexes := map[string]string{}

	raw, err := os.ReadFile(filepath.Join(bin, manifestName))
	require.NoError(t, err)

	for line := range strings.Lines(string(raw)) {
		fields := strings.Fields(line)
		regexes[fields[3]] = fields[2]
	}

	assert.Equal(t, map[string]string{
		pkgFixA:   "^(BenchmarkB)$",
		pkgFixB:   ".",
		pkgFixZZ:  ".",
		pkgFixZZ2: ".",
	}, regexes, "the plan's regex is the manifest's third field, and nothing the plan omits is built")
	require.Len(t, pkgs, 4)

	// The plan parses on the join side to the same packages and regexes.
	entries, err := parsePlan(plan)
	require.NoError(t, err)

	got := map[string]string{}
	for _, e := range entries {
		got[e.pkg] = e.raw
	}

	assert.Equal(t, regexes, got)
}

// internal/zz/a and internal/zz_a once built to one binary, because the name
// replaced every / by _.
func Test_bench_build_names_no_two_packages_alike(t *testing.T) {
	tree := fixtureModule(t)
	bin := filepath.Join(t.TempDir(), "bin")

	out, err := runMake(t, tree, "bench-build", "BENCH_BIN="+bin)
	require.NoError(t, err, out)

	pkgs, err := readManifest(bin)
	require.NoError(t, err, "no two manifest lines share a binary")

	names := map[string]string{}
	for _, p := range pkgs {
		names[p.pkg] = p.bin
	}

	assert.Contains(t, names, pkgFixZZ)
	assert.Contains(t, names, pkgFixZZ2)
	assert.NotEqual(t, names[pkgFixZZ], names[pkgFixZZ2])

	for _, p := range pkgs {
		assert.FileExists(t, filepath.Join(bin, p.bin))
	}
}

// A plan that builds nothing measured nothing, and bench-ab then exited 0 with
// empty output, which the run replayed as a 2x regression passing.
func Test_bench_build_fails_on_a_plan_that_resolves_to_nothing(t *testing.T) {
	tree := fixtureModule(t)

	missing := filepath.Join(t.TempDir(), "no-such-plan")

	tests := map[string]string{
		"an empty plan":                     writePlan(t, ""),
		"a plan of blank lines":             writePlan(t, "\n \n"),
		"a missing plan":                    missing,
		"a line without a regex":            writePlan(t, pkgFixA+"\n"),
		"a line with a third field":         writePlan(t, pkgFixA+" . extra\n"),
		"a package twice":                   writePlan(t, pkgFixA+" .\n"+pkgFixA+" ^(BenchmarkB)$\n"),
		"a package this tree does not hold": writePlan(t, pkgFixA+" .\nfixture/gone .\n"),
		"a package without a benchmark":     writePlan(t, pkgFixNoOne+" .\n"),
	}

	for name, plan := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := runMake(t, tree, "bench-build", "BENCH_BIN="+filepath.Join(t.TempDir(), "bin"), "BENCH_PLAN="+plan)
			require.Error(t, err, out)
			assert.Contains(t, out, "bench-build: BENCH_PLAN")
		})
	}
}

// The whole path: select's plan, bench-ab's two trees, the shard's artifact,
// and the join's coverage check.
func Test_plan_runs_through_bench_ab_and_join(t *testing.T) {
	tree := fixtureModule(t)
	plan := shard{units: []unit{{pkg: pkgFixA, names: []string{benchB}}, {pkg: pkgFixB}}}.plan()
	out := t.TempDir()
	baseOut, headOut := filepath.Join(out, baseFile), filepath.Join(out, headFile)

	abOut, err := runMake(t, tree, "bench-ab", "BASE=.", "HEAD=.", "BENCH_COUNT=1", "BENCH_TIME=1x",
		"BENCH_AB_DIR="+filepath.Join(t.TempDir(), "ab"), "BENCH_BASE_OUT="+baseOut, "BENCH_HEAD_OUT="+headOut,
		"BENCH_PLAN="+writePlan(t, plan))
	require.NoError(t, err, abOut)

	shards := t.TempDir()
	artifact := filepath.Join(shards, shardArtifactPrefix+"0")
	require.NoError(t, os.MkdirAll(artifact, 0o750))

	for _, name := range []string{baseFile, headFile} {
		raw, readErr := os.ReadFile(filepath.Join(out, name))
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(filepath.Join(artifact, name), raw, 0o600))

		names := benchmarkNames(raw)
		assert.Equal(t, map[string]bool{benchB: true}, names[pkgFixA], "the split regex ran BenchmarkB only")
		assert.Equal(t, map[string]bool{"BenchmarkC": true}, names[pkgFixB], "a sub-benchmark's row is its top-level's")
		assert.NotContains(t, names, pkgFixZZ, "a package the plan omits does not run")
	}

	_, _, err = runJoin(t, writeMatrixFile(t, plan), shards)
	require.NoError(t, err, "what bench-ab wrote covers the plan that drove it")

	// The same output does not cover a plan that names the package it skipped.
	_, _, err = runJoin(t, writeMatrixFile(t, plan+pkgFixZZ+" .\n"), shards)
	require.ErrorContains(t, err, "has no benchmark rows for "+pkgFixZZ+" .")
}
