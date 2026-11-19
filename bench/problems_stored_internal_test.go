package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The comparison of a stored baseline with a head: base and head files can hold
// different cells and targets, and the rules for what the head has alone, what
// it lacks, and what both have but the head has far more of.

const secondID = "sqs/messaging/persist=off/payload=1MiB/queue=fifo/shape=batch/proto=query/c=64"

func second() cellResult { return testCell(secondID, 5000, 9000, 100, 90*bytesPerMiB) }

// deadCell is a cell none of whose three runs completed.
func deadCell(id string) cellResult {
	runs := make([]string, 3)
	for i := range runs {
		runs[i] = fmt.Sprintf("run %d: the target container died during the run", i+1)
	}

	return cellResult{ID: id, Spec: goodCell().Spec, Problem: strings.Join(runs, "; "), FailedRuns: runs}
}

// lostRuns is a cell with `failed` runs that failed and `done` that completed.
func lostRuns(c cellResult, done, failed int) cellResult {
	c.Runs = done
	c.FailedRuns = make([]string, failed)

	for i := range c.FailedRuns {
		c.FailedRuns[i] = fmt.Sprintf("run %d: x", i+1)
	}

	return c
}

func Test_compare_a_cell_only_the_head_has_is_checked(t *testing.T) {
	base := reportOf([]cellResult{goodCell()}, nil)

	for name, c := range map[string]cellResult{
		"no completed run": deadCell(secondID),
		"a failed run":     lostRuns(second(), 2, 1),
		"send errors":      withErrors(second(), map[string]int{opSend: 500}),
	} {
		out, err := compared(t, base, reportOf([]cellResult{goodCell(), c}, nil))

		require.ErrorIs(t, err, errRunProblem, name)
		assert.Contains(t, out, "| gostack | `"+secondID+"` |", name)
		assert.Contains(t, out, "**fails**", name)
	}

	erroring := withErrors(second(), map[string]int{opSend: 500})

	out, err := compared(t, base, reportOf([]cellResult{goodCell(), erroring}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `"+secondID+"` | send errors | 0 | 500 | **fails** |")

	// A clean head-only cell has nothing to fail on, and is listed as not compared.
	out, err = compared(t, base, reportOf([]cellResult{goodCell(), second()}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "### Not compared")
	assert.Contains(t, out, "gostack "+secondID+" (head only)")
	assert.Contains(t, out, "### Errors and failed runs (0, 0 failing)")
}

func Test_compare_a_gated_target_or_cell_the_base_has_and_the_head_lacks_fails(t *testing.T) {
	// A cell of the base, in a head that has the target.
	out, err := compared(t, reportOf([]cellResult{goodCell(), second()}, nil), reportOf([]cellResult{goodCell()}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `"+secondID+"` | missing from the head | 3 runs | absent | **fails** |")

	// A base cell that never completed is not one the head lacks.
	neverRan := reportOf([]cellResult{goodCell(), deadCell(secondID)}, nil)

	_, err = compared(t, neverRan, reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)

	// A gated target of the base that the head has none of.
	base := reportOf([]cellResult{goodCell()}, nil)
	base.Targets = append(base.Targets, targetResult{Name: targetSlim, Cells: []cellResult{goodCell()}})

	out, err = compared(t, base, reportOf([]cellResult{goodCell()}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack-slim | `(target)` | missing from the head | measured | absent | **fails** |")

	// A head with no gostack target at all.
	head := reportOf(nil, nil)
	head.Targets = nil

	out, err = compared(t, reportOf([]cellResult{goodCell()}, nil), head)

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `(target)` | missing from the head |")

	// A target that is not gated does not fail, nor does one missing on both sides.
	base = reportOf([]cellResult{goodCell()}, nil)
	base.Targets = append(base.Targets, targetResult{Name: targetLocalstack, Cells: []cellResult{goodCell()}})

	_, err = compared(t, base, reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)

	_, err = compared(t, reportOf([]cellResult{goodCell()}, nil), reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)
}

// What the head was not asked to measure is not missing from it.
func Test_compare_does_not_miss_what_the_head_did_not_select(t *testing.T) {
	s3 := goodCell()
	s3.ID, s3.Spec.Service = "s3/objects/x", serviceS3

	base := reportOf([]cellResult{goodCell(), s3}, nil)
	base.Targets = append(base.Targets, targetResult{Name: targetSlim, Cells: []cellResult{goodCell()}})

	head := reportOf([]cellResult{goodCell()}, nil)

	_, err := compared(t, base, head)
	require.ErrorIs(t, err, errRunProblem, "a head of every service lacks the S3 cell")

	head.Settings.Services = []string{serviceSQS}
	base.Targets = base.Targets[:1]

	out, err := compared(t, base, head)
	require.NoError(t, err, "a head that selected only sqs was not asked for the S3 cell")
	assert.Contains(t, out, "gostack s3/objects/x", "it is still listed as not compared")

	// A quick run against a full result: the head never asked for most of its cells.
	base.Profile = profileFull
	head.Settings.Services = nil

	_, err = compared(t, base, head)
	require.NoError(t, err, "different profiles")

	base.Targets = append(base.Targets, targetResult{Name: targetSlim, Cells: []cellResult{goodCell()}})
	_, err = compared(t, base, head)
	require.NoError(t, err, "and the slim target the quick run never built")
}

func Test_compare_target_problems_are_compared_by_kind(t *testing.T) {
	problems := func(lines ...string) func(*targetResult) {
		return func(t *targetResult) { t.Problems = lines }
	}

	base := reportOf([]cellResult{goodCell()}, problems("image size: docker inspect failed"))

	// The same kind with another message is a problem both sides have.
	out, err := compared(t, base, reportOf([]cellResult{goodCell()}, problems("image size: no such image")))

	require.NoError(t, err)
	assert.Contains(t, out, "| image size: docker inspect failed | image size: no such image | listed |")

	// Kinds the base does not have fail, one row each, whatever the base's count.
	head := reportOf([]cellResult{goodCell()}, problems(
		"image size: no such image", "startup: never ready",
		"agentic workload (sqs): the target container died during the run", "idle: x"))

	out, err = compared(t, base, head)

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| none | startup: never ready | **fails** |")
	assert.Contains(t, out, "| none | agentic workload (sqs): the target container died during the run | **fails** |")
	assert.Contains(t, out, "| none | idle: x | **fails** |")
	assert.Contains(t, out, "(4, 3 failing)")
}

func Test_compare_failed_runs_fail_when_the_head_has_more_than_the_base(t *testing.T) {
	base := reportOf([]cellResult{lostRuns(goodCell(), 4, 1)}, nil)

	out, err := compared(t, base, reportOf([]cellResult{lostRuns(goodCell(), 1, 4)}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| runs | 4 runs, 1 failed | 1 runs, 4 failed | **fails** |")

	out, err = compared(t, base, reportOf([]cellResult{lostRuns(goodCell(), 4, 1)}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "| runs | 4 runs, 1 failed | 4 runs, 1 failed | listed |")

	// Fewer failed runs in the head than the base is not worse.
	_, err = compared(t, base, reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)
}

func Test_compare_operation_errors_fail_when_far_worse_than_the_base(t *testing.T) {
	for _, tt := range []struct {
		base, head int
		fails      bool
	}{
		{1, 10000, true},
		{1, 10, false},
		{1, 11, true},
		{3, 29, false},
		{3, 31, true},
		{5, 5, false},
		{5, 2, false},
		{0, 1, true},
		{50, 500, false},
		{50, 501, true},
	} {
		base := reportOf([]cellResult{withErrors(goodCell(), map[string]int{opSend: tt.base})}, nil)
		head := reportOf([]cellResult{withErrors(goodCell(), map[string]int{opSend: tt.head})}, nil)

		out, err := compared(t, base, head)

		result := "listed"
		if tt.fails {
			require.ErrorIs(t, err, errRunProblem, "base %d head %d", tt.base, tt.head)

			result = "**fails**"
		} else {
			require.NoError(t, err, "base %d head %d", tt.base, tt.head)
		}

		if tt.base > 0 || tt.head > 0 {
			assert.Contains(t, out, fmt.Sprintf("| send errors | %d | %d | %s |", tt.base, tt.head, result))
		}
	}

	// The same rule for the cycles of an agentic workload.
	agentic := func(n int) func(*targetResult) {
		return func(t *targetResult) { t.Agentic = map[string]agenticRun{serviceSQS: {Cycles: 200, Errors: n}} }
	}

	_, err := compared(t, reportOf([]cellResult{goodCell()}, agentic(1)), reportOf([]cellResult{goodCell()}, agentic(40)))
	require.ErrorIs(t, err, errRunProblem)

	_, err = compared(t, reportOf([]cellResult{goodCell()}, agentic(4)), reportOf([]cellResult{goodCell()}, agentic(40)))
	require.NoError(t, err)
}

func Test_compare_a_base_cell_with_no_completed_run_does_not_hide_the_head_errors(t *testing.T) {
	base := reportOf([]cellResult{deadCell(cellID)}, nil)

	out, err := compared(t, base, reportOf([]cellResult{withErrors(goodCell(), map[string]int{opSend: 500})}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| send errors | 0 | 500 | **fails** |")

	// Failed runs are a kind the base's cell has: fewer than its own pass, more fail.
	out, err = compared(t, base, reportOf([]cellResult{lostRuns(goodCell(), 2, 1)}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "| runs | 0 runs, 3 failed | 2 runs, 1 failed | listed |")

	_, err = compared(t, base, reportOf([]cellResult{deadCell(cellID)}, nil))
	require.NoError(t, err, "a head with no completed run either is a problem both sides have")

	_, err = compared(t, base, reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)

	out, err = compared(t, base, reportOf([]cellResult{goodCell()}, nil))
	require.NoError(t, err)
	assert.Contains(t, out, "### Errors and failed runs (0, 0 failing)")
}

func Test_compare_says_when_a_row_has_fewer_runs_than_the_run_asked_for(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	head := testReport(hostA, testCell(cellID, 1000, 13000, 2700, 50*bytesPerMiB))
	base.Settings.Reps, head.Settings.Reps = 5, 5

	head.Targets[0].Cells[0].Runs = 4

	out, err := compared(t, base, head)

	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "send p50 (base 3 of 5 runs, head 4 of 5 runs)")

	// A failed run counts as asked for even when the result has no reps setting.
	base.Settings.Reps, head.Settings.Reps = 0, 0
	base.Targets[0].Cells[0].Runs = 3
	head.Targets[0].Cells[0].FailedRuns = []string{"run 3: x"}
	head.Targets[0].Cells[0].Runs = 2

	out, err = compared(t, base, head)

	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "send p50 (head 2 of 3 runs)")

	// A complete cell carries no note.
	head.Targets[0].Cells[0].FailedRuns, head.Targets[0].Cells[0].Runs = nil, 3

	out, err = compared(t, base, head)

	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "| send p50 |")
	assert.NotContains(t, out, "runs)")
}

// The scheduled job holds a new result against the stored one of an older
// schema, and a head that has nothing wrong must still pass. These are the
// committed files.
func Test_compare_a_new_head_with_the_committed_results_passes_when_nothing_is_wrong(t *testing.T) {
	const committedV3 = "results/2026-11-18-3971cef.json"

	var out bytes.Buffer

	// An S3 result against the older, SQS-only schema 2 result: the head selected
	// S3 alone, so the base's SQS cells are not missing from it.
	require.NoError(t, compareFiles(t.Context(), committedV2, "", committedV3, acceptedSet{}, &out))
	assert.Contains(t, out.String(), "### Errors and failed runs (0, 0 failing)")

	out.Reset()
	require.NoError(t, compareFiles(t.Context(), committedV3, "", committedV3, acceptedSet{}, &out))
	assert.Contains(t, out.String(), "### Errors and failed runs (0, 0 failing)")

	out.Reset()
	require.NoError(t, compareFiles(t.Context(), committedV2, "", committedV2, acceptedSet{}, &out))
	assert.Contains(t, out.String(), "### Errors and failed runs (0, 0 failing)")

	// The same head with one cell that errored fails, naming the cell.
	head, err := loadReport(committedV3)
	require.NoError(t, err)

	gostack, ok := head.target(targetGostack)
	require.True(t, ok)

	broken := gostack.Cells[0]
	broken.Ops = withErrors(broken, map[string]int{opPutObject: 7}).Ops
	gostack.Cells[0] = broken

	base, err := loadReport(committedV3)
	require.NoError(t, err)

	out.Reset()

	err = compareReports(t.Context(), base, head, acceptedSet{}, &out)

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out.String(), "| gostack | `"+broken.ID+"` | "+opPutObject+" errors | 0 | 7 | **fails** |")
}
