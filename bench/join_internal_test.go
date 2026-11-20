package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	flagBase = "-base"
	flagHead = "-head"
	benchA   = "BenchmarkA"
	benchB   = "BenchmarkB"
	// The names of result rows, with their GOMAXPROCS suffix.
	rowA     = benchA + "-4"
	rowB     = benchB + "-4"
	rowASub  = benchA + "/size=1k-4"
	rowADeep = benchA + "/sub-case/deep-16"
	rowAx    = benchA + "/x"
)

// shardOutput is what go test -bench prints for one run of a package: rows is
// the result rows' names, each with its GOMAXPROCS suffix.
func shardOutput(pkg string, rows ...string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "goos: linux\ngoarch: amd64\npkg: %s\ncpu: Cpu A\n", pkg)

	for _, row := range rows {
		fmt.Fprintf(&b, "%s \t 100\t 10.0 ns/op\n", row)
	}

	fmt.Fprintf(&b, "PASS\nok  \t%s\t1.0s\n", pkg)

	return b.String()
}

// writeShard writes a shard artifact: bench-shard-<i>/base.txt and head.txt.
func writeShard(t *testing.T, dir string, i int, base, head string) {
	t.Helper()

	shard := filepath.Join(dir, shardArtifactPrefix+strconv.Itoa(i))
	require.NoError(t, os.MkdirAll(shard, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(shard, baseFile), []byte(base), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(shard, headFile), []byte(head), 0o600))
}

func writeMatrixFile(t *testing.T, plans ...string) string {
	t.Helper()

	var m matrix

	for i, plan := range plans {
		m.Include = append(m.Include, matrixEntry{Shard: fmt.Sprintf("%d of %d", i+1, len(plans)), Plan: plan})
	}

	data, err := json.Marshal(m)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "matrix.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

func runJoin(t *testing.T, matrixPath, shards string) (base, head string, err error) {
	t.Helper()

	out := t.TempDir()
	basePath, headPath := filepath.Join(out, "base.txt"), filepath.Join(out, "head.txt")

	args := []string{"-matrix", matrixPath, "-shards", shards, flagBase, basePath, flagHead, headPath}

	err = joinMain(args, &bytes.Buffer{})
	if err != nil {
		return "", "", err
	}

	b, err := os.ReadFile(basePath)
	require.NoError(t, err)

	h, err := os.ReadFile(headPath)
	require.NoError(t, err)

	return string(b), string(h), nil
}

// A re-run keeps the artifacts of the attempt before. When the second attempt
// plans fewer shards, the higher index is stale: the run replayed a +15%
// regression passing because the stale shard's rows diluted it.
func Test_joinMain_joins_the_planned_shards_only(t *testing.T) {
	dir := t.TempDir()
	matrixPath := writeMatrixFile(t, "x .\n", "y .\n")

	writeShard(t, dir, 0, shardOutput("x", rowA), shardOutput("x", rowA))
	writeShard(t, dir, 1, shardOutput("y", rowB), shardOutput("y", rowB))
	writeShard(t, dir, 2, shardOutput("stale", "BenchmarkStale-4"), shardOutput("stale", "BenchmarkStale-4"))

	base, head, err := runJoin(t, matrixPath, dir)
	require.NoError(t, err)

	assert.Contains(t, base, rowA)
	assert.Contains(t, base, rowB)
	assert.NotContains(t, base, "Stale", "bench-shard-2 is not in the matrix")
	assert.NotContains(t, head, "Stale")
	assert.True(t, strings.HasPrefix(base, "goos:"), "shards join in index order, from bench-shard-0")
	assert.Less(t, strings.Index(base, benchA), strings.Index(base, rowB))
}

func Test_joinMain_fails_on_a_missing_shard(t *testing.T) {
	dir := t.TempDir()
	matrixPath := writeMatrixFile(t, "x .\n", "y .\n")

	writeShard(t, dir, 0, shardOutput("x", rowA), shardOutput("x", rowA))
	// bench-shard-1 never arrived; a stale bench-shard-2 is no replacement.
	writeShard(t, dir, 2, shardOutput("y", rowB), shardOutput("y", rowB))

	_, _, err := runJoin(t, matrixPath, dir)
	require.ErrorContains(t, err, "shard 2 of 2 (bench-shard-1)")
	require.ErrorIs(t, err, os.ErrNotExist)
}

// A package a shard planned that has no rows in one side's output means that
// side's measurement was lost: the run replayed a 2x regression passing when
// the shard that held it produced no head output.
func Test_joinMain_fails_when_a_planned_package_has_no_rows(t *testing.T) {
	const plan = "x .\ny ^(BenchmarkSlow|BenchmarkFast)$\n"

	rows := shardOutput

	tests := []struct {
		name       string
		base, head string
		want       string // the problem, or "" when the shard covers its plan
	}{
		{
			name: "every package has rows on both sides",
			base: rows("x", rowA) + rows("y", "BenchmarkSlow/size=1k-4"),
			head: rows("x", rowA) + rows("y", "BenchmarkSlow-4"),
		},
		{
			name: "an empty head output",
			base: rows("x", rowA) + rows("y", "BenchmarkSlow-4"),
			head: "",
			want: "head.txt has no benchmark rows for x .",
		},
		{
			name: "a package missing from the base",
			base: rows("x", rowA),
			head: rows("x", rowA) + rows("y", "BenchmarkFast-4"),
			want: "base.txt has no benchmark rows for y ^(BenchmarkSlow|BenchmarkFast)$",
		},
		{
			name: "a package block without rows",
			base: rows("x", rowA) + rows("y", "BenchmarkFast-4"),
			head: rows("x", rowA) + rows("y"),
			want: "head.txt has no benchmark rows for y ^(BenchmarkSlow|BenchmarkFast)$",
		},
		{
			name: "rows of the package that the split regex does not name",
			base: rows("x", rowA) + rows("y", "BenchmarkOther-4"),
			head: rows("x", rowA) + rows("y", "BenchmarkFast-4"),
			want: "base.txt has no benchmark rows for y ^(BenchmarkSlow|BenchmarkFast)$",
		},
		{
			name: "rows under another package's name",
			base: rows("x", rowA) + rows("y", "BenchmarkFast-4"),
			head: rows("x", rowA) + rows("z", "BenchmarkFast-4"),
			want: "head.txt has no benchmark rows for y ^(BenchmarkSlow|BenchmarkFast)$",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeShard(t, dir, 0, tt.base, tt.head)

			_, _, err := runJoin(t, writeMatrixFile(t, plan), dir)
			if tt.want == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, errJoin)
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, "shard 1 of 1 (bench-shard-0)")
		})
	}
}

func Test_joinMain_refuses_a_plan_it_cannot_read(t *testing.T) {
	for _, plan := range []string{"", "\n  \n", "x\n", "x . extra\n", "x ^(\n"} {
		dir := t.TempDir()
		writeShard(t, dir, 0, shardOutput("x", rowA), shardOutput("x", rowA))

		_, _, err := runJoin(t, writeMatrixFile(t, plan), dir)
		require.ErrorIs(t, err, errJoin, "plan %q", plan)
	}

	_, _, err := runJoin(t, writeMatrixFile(t), t.TempDir())
	require.ErrorIs(t, err, errJoin, "a matrix without a shard")
}

func Test_topLevel(t *testing.T) {
	tests := map[string]string{
		rowA:     benchA,
		benchA:   benchA,
		rowASub:  benchA,
		rowADeep: benchA,
		rowAx:    benchA,
	}

	for row, want := range tests {
		assert.Equal(t, want, topLevel(row), row)
	}
}
