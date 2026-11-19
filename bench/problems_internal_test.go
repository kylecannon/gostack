package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withErrors is a copy of c whose operation reported errors in its runs.
func withErrors(c cellResult, errs map[string]int) cellResult {
	ops := map[string]opSpread{}

	for name, op := range c.Ops {
		op.Errors = errs[name]
		ops[name] = op
	}

	c.Ops = ops

	return c
}

func goodCell() cellResult { return testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB) }

// reportOf is a report of the gostack target with the given parts.
func reportOf(cells []cellResult, edit func(*targetResult)) report {
	rep := testReport(hostA, cells...)
	if edit != nil {
		edit(&rep.Targets[0])
	}

	return rep
}

func Test_compare_head_errors_where_the_base_has_none_fail(t *testing.T) {
	base := reportOf([]cellResult{goodCell()}, nil)
	head := reportOf([]cellResult{withErrors(goodCell(), map[string]int{opSend: 51})}, nil)

	out, err := compared(t, base, head)

	require.ErrorIs(t, err, errRunProblem)
	require.NotErrorIs(t, err, errRegression, "the latencies did not move")
	assert.Contains(t, out, "| gostack | `"+cellID+"` | send errors | 0 | 51 | **fails** |",
		"the row names the cell and the operation with both counts")
}

func Test_compare_no_errors_on_either_side_passes(t *testing.T) {
	out, err := compared(t, reportOf([]cellResult{goodCell()}, nil), reportOf([]cellResult{goodCell()}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "### Errors and failed runs (0, 0 failing)")
}

func Test_compare_errors_on_both_sides_are_listed_and_pass(t *testing.T) {
	errs := map[string]int{opSend: 3}
	base := reportOf([]cellResult{withErrors(goodCell(), errs)}, nil)
	head := reportOf([]cellResult{withErrors(goodCell(), errs)}, nil)

	out, err := compared(t, base, head)

	require.NoError(t, err)
	assert.Contains(t, out, "| gostack | `"+cellID+"` | send errors | 3 | 3 | listed |")

	// The rule is per operation: a second operation that errors only in the head fails.
	head = reportOf([]cellResult{withErrors(goodCell(), map[string]int{opSend: 3, opReceive: 1})}, nil)
	out, err = compared(t, base, head)

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| receive errors | 0 | 1 | **fails** |")
	assert.Contains(t, out, "| send errors | 3 | 3 | listed |")
}

func Test_compare_errors_only_in_the_base_are_listed_and_pass(t *testing.T) {
	base := reportOf([]cellResult{withErrors(goodCell(), map[string]int{opSend: 2})}, nil)

	out, err := compared(t, base, reportOf([]cellResult{goodCell()}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "| send errors | 2 | 0 | listed |")
}

func Test_compare_runs_that_failed_fail_and_unequal_counts_are_reported(t *testing.T) {
	base := reportOf([]cellResult{goodCell()}, nil)

	lost := goodCell()
	lost.Runs, lost.FailedRuns = 2, []string{"run 2: the target container died during the run"}

	out, err := compared(t, base, reportOf([]cellResult{lost}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `"+cellID+"` | runs | 3 runs | 2 runs, 1 failed | **fails** |")

	// Fewer runs a side with nothing failed (another -reps) is reported, not failed.
	fewer := goodCell()
	fewer.Runs = 2

	out, err = compared(t, base, reportOf([]cellResult{fewer}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "| runs | 3 runs | 2 runs | listed |")

	// Failed runs on both sides are listed and do not fail.
	out, err = compared(t, reportOf([]cellResult{lost}, nil), reportOf([]cellResult{lost}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "| runs | 2 runs, 1 failed | 2 runs, 1 failed | listed |")
}

func Test_compare_a_head_cell_with_no_completed_run_fails_even_when_it_is_the_only_cell(t *testing.T) {
	dead := cellResult{
		ID: cellID, Spec: goodCell().Spec, Problem: "run 1: x; run 2: x; run 3: x",
		FailedRuns: []string{"run 1: x", "run 2: x", "run 3: x"},
	}

	out, err := compared(t, reportOf([]cellResult{goodCell()}, nil), reportOf([]cellResult{dead}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "no completed run")
	assert.Contains(t, out, cellID)
	assert.NotContains(t, out, "Nothing to compare")

	// The same on both sides is listed; a base that failed is not held against the head.
	_, err = compared(t, reportOf([]cellResult{dead}, nil), reportOf([]cellResult{dead}, nil))
	require.NoError(t, err)

	_, err = compared(t, reportOf([]cellResult{dead}, nil), reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)
}

func Test_compare_agentic_errors_and_target_problems(t *testing.T) {
	agentic := func(errs int) func(*targetResult) {
		return func(t *targetResult) {
			t.Agentic = map[string]agenticRun{"sqs": {Cycles: 200, Errors: errs}}
		}
	}

	out, err := compared(t, reportOf([]cellResult{goodCell()}, agentic(0)), reportOf([]cellResult{goodCell()}, agentic(4)))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `agentic/sqs` | cycle errors | 0 | 4 | **fails** |")

	_, err = compared(t, reportOf([]cellResult{goodCell()}, agentic(4)), reportOf([]cellResult{goodCell()}, agentic(4)))
	require.NoError(t, err)

	problems := func(lines ...string) func(*targetResult) {
		return func(t *targetResult) { t.Problems = lines }
	}

	died := "agentic workload (sqs): the target container died during the run: gostack-x is exited"

	out, err = compared(t, reportOf([]cellResult{goodCell()}, nil), reportOf([]cellResult{goodCell()}, problems(died)))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, died)

	// A line about a cell is read from the cell's own fields, not twice from here.
	cellLine := cellProblemPrefix + cellID + ": send: 51 errors"
	_, err = compared(t, reportOf([]cellResult{goodCell()}, nil), reportOf([]cellResult{goodCell()}, problems(cellLine)))
	require.NoError(t, err)

	_, err = compared(t, reportOf([]cellResult{goodCell()}, problems(died)),
		reportOf([]cellResult{goodCell()}, problems(died)))
	require.NoError(t, err, "a problem on both sides does not fail")
}

func Test_compare_ignores_the_problems_of_ungated_targets(t *testing.T) {
	base := reportOf([]cellResult{goodCell()}, nil)
	head := reportOf([]cellResult{goodCell()}, nil)
	head.Targets = append(head.Targets, targetResult{Name: targetLocalstack, Problems: []string{"startup: x"}})

	_, err := compared(t, base, head)
	require.NoError(t, err)
}

func Test_settleCell_keeps_a_failed_run_in_the_result(t *testing.T) {
	spec := goodCell().Spec
	run := runResult{Seconds: 2, Cycles: 10, Ops: map[string]opStats{opSend: {Count: 10}}}
	lost := []string{"run 2: " + errTargetDied.Error()}

	cell := settleCell(spec, []runResult{run, run}, lost)

	assert.Equal(t, 2, cell.Runs, "the figures are of the runs that completed")
	assert.Equal(t, lost, cell.FailedRuns, "the one that did not is not dropped without a trace")
	assert.Empty(t, cell.Problem, "the cell still has figures")

	var target targetResult

	target.noteCell(cell)

	require.Len(t, target.Problems, 1)
	assert.Contains(t, target.Problems[0], cellProblemPrefix+cell.ID+": 1 of 3 runs failed")

	none := settleCell(spec, nil, []string{"run 1: x", "run 2: y"})
	assert.Equal(t, "run 1: x; run 2: y", none.Problem)
	assert.Zero(t, none.Runs)
}

func Test_noteCell_records_each_operation_that_errored(t *testing.T) {
	var target targetResult

	target.noteCell(withErrors(goodCell(), map[string]int{opSend: 51, opDelete: 2}))
	target.noteAgentic("sqs", agenticRun{Cycles: 200, Errors: 1})

	require.Len(t, target.Problems, 3)
	assert.Contains(t, target.Problems[0], opDelete+": 2 errors")
	assert.Contains(t, target.Problems[1], opSend+": 51 errors")
	assert.Contains(t, target.Problems[2], cellProblemPrefix+"agentic/sqs: 1 errors in 200 cycles")

	var clean targetResult

	clean.noteCell(goodCell())
	clean.noteAgentic("sqs", agenticRun{Cycles: 200})
	assert.Empty(t, clean.Problems)
}

func Test_failed_runs_are_a_compatible_addition_to_schema_3(t *testing.T) {
	raw, err := json.Marshal(goodCell())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "failed_runs", "a cell with none writes no field, so older readers see no change")

	var cell cellResult

	require.NoError(t, json.Unmarshal(raw, &cell), "a file without the field still loads")
	assert.Empty(t, cell.FailedRuns)
	assert.Equal(t, 3, reportSchema)
}

func Test_checkBase_compares_absolute_paths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	opts := func(out, baseOut string) options {
		return options{baseImage: "b", baseCommit: "c", baseOut: baseOut, outDir: out, targets: []target{{}}}
	}

	const rel = "results"

	abs := filepath.Join(dir, rel)

	for name, tt := range map[string]struct{ out, baseOut string }{
		"relative and absolute":       {out: rel, baseOut: abs},
		"absolute and relative":       {out: abs, baseOut: rel},
		"dot and trailing slash":      {out: "./" + rel, baseOut: rel + "/"},
		"up and down":                 {out: rel + "/../" + rel, baseOut: rel},
		"absolute, spelled two ways":  {out: abs, baseOut: abs + "/."},
		"relative, spelled two ways":  {out: rel, baseOut: "sub/../" + rel},
		"relative, the current place": {out: ".", baseOut: dir},
	} {
		o := opts(tt.out, tt.baseOut)
		require.Error(t, o.checkBase(), name)
	}

	for name, tt := range map[string]struct{ out, baseOut string }{
		"two directories":                {out: rel + "/head", baseOut: rel + "/base"},
		"one relative, one absolute one": {out: "head", baseOut: filepath.Join(dir, "base")},
	} {
		o := opts(tt.out, tt.baseOut)
		require.NoError(t, o.checkBase(), name)
	}
}

func Test_removeArgs_removes_the_volume_with_the_container(t *testing.T) {
	args := removeArgs("gostack-bench-x")

	assert.Equal(t, []string{"rm", "-f", "-v", "gostack-bench-x"}, args,
		"-v: the image declares a volume, and docker rm -f leaves it behind without it")
}
