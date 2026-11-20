package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// shardArtifactPrefix is the name of a shard's output artifact up to its
	// index: the shard whose matrix entry has index i uploads bench-shard-i.
	shardArtifactPrefix = "bench-shard-"
	baseFile            = "base.txt"
	headFile            = "head.txt"
	planFields          = 2 // <import path> <bench regex>
)

// errJoin is returned when the shards' output cannot be trusted to hold what the
// run planned.
var errJoin = errors.New("join")

// planEntry is one line of a shard's plan: a package and the benchmarks of it
// the shard runs.
type planEntry struct {
	pkg   string
	bench *regexp.Regexp
	raw   string
}

// parsePlan reads a shard's plan, "<import path> <bench regex>" per line, the
// format shard.plan writes and the Makefile's bench-build reads.
func parsePlan(text string) ([]planEntry, error) {
	var entries []planEntry

	for line := range strings.Lines(text) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		if len(fields) != planFields {
			return nil, fmt.Errorf("%w: plan line %q is not <import path> <bench regex>", errJoin, strings.TrimSpace(line))
		}

		re, err := regexp.Compile(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%w: plan line %q: %w", errJoin, strings.TrimSpace(line), err)
		}

		entries = append(entries, planEntry{pkg: fields[0], bench: re, raw: fields[1]})
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: the plan names no package", errJoin)
	}

	return entries, nil
}

// benchmarkNames reads go test -bench output: the top-level benchmarks of the
// result rows, by the package of the pkg: line above them. Concatenated outputs
// are read as one; goos: starts a new block, so rows after it and before its
// pkg: line belong to no package.
func benchmarkNames(output []byte) map[string]map[string]bool {
	names := map[string]map[string]bool{}

	var pkg string

	for line := range strings.Lines(string(output)) {
		switch {
		case strings.HasPrefix(line, "goos:"):
			pkg = ""
		case strings.HasPrefix(line, "pkg:"):
			pkg = strings.TrimSpace(strings.TrimPrefix(line, "pkg:"))
		case strings.HasPrefix(line, "Benchmark"):
			fields := strings.Fields(line)
			if len(fields) < planFields || pkg == "" || !isCount(fields[1]) {
				continue
			}

			if names[pkg] == nil {
				names[pkg] = map[string]bool{}
			}

			names[pkg][topLevel(fields[0])] = true
		default: // a log line, or what go test prints around the rows
		}
	}

	return names
}

func isCount(s string) bool {
	_, err := strconv.Atoi(s)

	return err == nil
}

// topLevel is the top-level benchmark of a result row's name:
// "BenchmarkA/sub-4" is BenchmarkA, and "BenchmarkA-4" is BenchmarkA.
func topLevel(row string) string {
	if top, _, ok := strings.Cut(row, "/"); ok {
		return top
	}

	if i := strings.LastIndexByte(row, '-'); i > 0 && isCount(row[i+1:]) {
		return row[:i]
	}

	return row
}

// missing names each planned package whose regex matches no benchmark that has
// rows in the output.
func missing(plan []planEntry, output []byte) []string {
	names := benchmarkNames(output)

	var gone []string

	for _, e := range plan {
		found := false

		for name := range names[e.pkg] {
			if e.bench.MatchString(name) {
				found = true

				break
			}
		}

		if !found {
			gone = append(gone, e.pkg+" "+e.raw)
		}
	}

	return gone
}

// joinMain is `bench join`: it reads the matrix `bench select` wrote and joins
// the output of exactly the shards it planned, bench-shard-0 up to
// bench-shard-<n-1> under -shards, into -base and -head for benchstat. A run
// that is re-run keeps the artifacts of the attempt before, so a directory
// holding more shards than the matrix plans is not read past the plan. It fails
// when a planned shard's output is missing, or holds no rows, in the base or in
// the head, for a package and regex of that shard's plan.
func joinMain(args []string, out io.Writer) error {
	var matrixPath, shardsDir, baseOut, headOut string

	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	flags.StringVar(&matrixPath, "matrix", "", "the matrix file bench select wrote")
	flags.StringVar(&shardsDir, "shards", "", "the directory holding each shard's bench-shard-<i> artifact")
	flags.StringVar(&baseOut, "base", "", "where to write the joined base output")
	flags.StringVar(&headOut, "head", "", "where to write the joined head output")

	err := flags.Parse(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if matrixPath == "" || shardsDir == "" || baseOut == "" || headOut == "" {
		return fmt.Errorf("%w: -matrix, -shards, -base and -head are required", errJoin)
	}

	m, err := readMatrix(matrixPath)
	if err != nil {
		return err
	}

	base, head, err := joinShards(m, shardsDir)
	if err != nil {
		return err
	}

	err = os.WriteFile(baseOut, base, fileMode)
	if err != nil {
		return fmt.Errorf("write joined base: %w", err)
	}

	err = os.WriteFile(headOut, head, fileMode)
	if err != nil {
		return fmt.Errorf("write joined head: %w", err)
	}

	_, err = fmt.Fprintf(out, "joined %d planned shard(s) from %s\n", len(m.Include), shardsDir)
	if err != nil {
		return fmt.Errorf("write join report: %w", err)
	}

	return nil
}

func readMatrix(path string) (matrix, error) {
	var m matrix

	data, err := os.ReadFile(path) //nolint:gosec // the file bench select wrote
	if err != nil {
		return m, fmt.Errorf("read matrix: %w", err)
	}

	err = json.Unmarshal(data, &m)
	if err != nil {
		return m, fmt.Errorf("%w: decode matrix: %w", errJoin, err)
	}

	if len(m.Include) == 0 {
		return m, fmt.Errorf("%w: the matrix plans no shard", errJoin)
	}

	return m, nil
}

// joinShards joins bench-shard-0 up to bench-shard-<n-1> of the matrix, and
// names every problem of every shard at once.
func joinShards(m matrix, shardsDir string) (base, head []byte, err error) {
	var baseJoin, headJoin bytes.Buffer

	var problems []error

	for i, e := range m.Include {
		name := shardArtifactPrefix + strconv.Itoa(e.Index)

		var shardErr error

		if e.Index == i {
			shardErr = joinShard(e, filepath.Join(shardsDir, name), &baseJoin, &headJoin)
		} else {
			shardErr = fmt.Errorf("%w: its matrix entry has index %d, not %d", errJoin, e.Index, i)
		}

		if shardErr != nil {
			problems = append(problems, fmt.Errorf("shard %d of %d (%s): %w", i+1, len(m.Include), name, shardErr))
		}
	}

	if len(problems) > 0 {
		return nil, nil, errors.Join(problems...)
	}

	return baseJoin.Bytes(), headJoin.Bytes(), nil
}

// joinShard appends a shard's base and head output to the joins after checking
// that each holds rows for every package and regex of the shard's plan.
func joinShard(e matrixEntry, dir string, base, head *bytes.Buffer) error {
	plan, err := parsePlan(e.Plan)
	if err != nil {
		return err
	}

	var problems []error

	for _, side := range []struct {
		file string
		join *bytes.Buffer
	}{{baseFile, base}, {headFile, head}} {
		output, readErr := os.ReadFile(filepath.Join(dir, side.file)) //nolint:gosec // a downloaded artifact of this run
		if readErr != nil {
			problems = append(problems, fmt.Errorf("%s: %w", side.file, readErr))

			continue
		}

		for _, gone := range missing(plan, output) {
			problems = append(problems, fmt.Errorf("%w: %s has no benchmark rows for %s", errJoin, side.file, gone))
		}

		side.join.Write(output)

		if !bytes.HasSuffix(output, []byte("\n")) {
			side.join.WriteByte('\n')
		}
	}

	return errors.Join(problems...)
}
