package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stored base can hold cells the head's matrix no longer has. The scheduled
// job keeps a baseline until a run passes, so a removed cell must not fail it.
func Test_compare_a_cell_removed_from_the_matrix_is_not_missing_from_the_head(t *testing.T) {
	removed := testCell(goneID, 5000, 9000, 100, 90*bytesPerMiB)
	base := reportOf([]cellResult{goodCell(), removed}, nil)

	out, err := compared(t, base, reportOf([]cellResult{goodCell()}, nil))

	require.NoError(t, err)
	assert.Contains(t, out, "- gostack "+goneID+" (removed from the matrix)")
	assert.Contains(t, out, "### Errors and failed runs (0, 0 failing)")

	// A cell the matrix still has, and the head lacks, fails.
	still := reportOf([]cellResult{goodCell(), second()}, nil)

	out, err = compared(t, still, reportOf([]cellResult{goodCell()}, nil))

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `"+secondID+"` | missing from the head |")
	assert.NotContains(t, out, "removed from the matrix")

	// A renamed cell id is one removed cell and one head-only cell.
	renamed := reportOf([]cellResult{goodCell(), second()}, nil)

	out, err = compared(t, base, renamed)

	require.NoError(t, err)
	assert.Contains(t, out, "- gostack "+goneID+" (removed from the matrix)")
	assert.Contains(t, out, "- gostack "+secondID+" (head only)")

	// The head-only half is still checked.
	erroring := reportOf([]cellResult{goodCell(), withErrors(second(), map[string]int{opSend: 9})}, nil)

	out, err = compared(t, base, erroring)

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "| gostack | `"+secondID+"` | send errors | 0 | 9 | **fails** |")
}

// A result written before failed_runs existed names no failed run in a cell that
// never completed. That cell failed every run its settings asked for.
func Test_compare_a_dead_cell_without_failed_runs_counts_every_run_as_failed(t *testing.T) {
	dead := deadCell(cellID)
	dead.FailedRuns = nil

	base := reportOf([]cellResult{dead}, nil)
	base.Settings.Reps = 3

	head := reportOf([]cellResult{lostRuns(goodCell(), 1, 2)}, nil)
	head.Settings.Reps = 3

	out, err := compared(t, base, head)

	require.NoError(t, err, "a head that completed a run did better than a base that completed none")
	assert.Contains(t, out, "| runs | 0 runs | 1 runs, 2 failed | listed |")

	// More failed runs than the base asked for is worse.
	head = reportOf([]cellResult{lostRuns(goodCell(), 1, 4)}, nil)
	head.Settings.Reps = 5

	_, err = compared(t, base, head)
	require.ErrorIs(t, err, errRunProblem, "4 failed runs against the base's 3")
}

// A link to a directory that does not exist yet and that directory are one path.
func Test_checkBase_follows_a_link_to_a_directory_not_made_yet(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	require.NoError(t, os.Symlink("later", "link"))
	require.NoError(t, os.Symlink(filepath.Join(dir, "elsewhere", "deep"), "far"))
	require.NoError(t, os.Symlink("far", "chain"))

	opts := func(out, baseOut string) options {
		return options{baseImage: "b", baseCommit: "c", baseOut: baseOut, outDir: out, targets: []target{{}}}
	}

	for name, tt := range map[string]struct{ out, baseOut string }{
		"a dangling link and its target":                 {out: "link", baseOut: "later"},
		"a dangling link, absolute and relative":         {out: filepath.Join(dir, "link"), baseOut: "later/"},
		"a directory made under a dangling link":         {out: "link/new", baseOut: "later/new"},
		"an absolute dangling link and its target":       {out: "far", baseOut: "elsewhere/deep"},
		"a chain of dangling links and the final target": {out: "chain", baseOut: "elsewhere/deep"},
	} {
		o := opts(tt.out, tt.baseOut)
		require.Error(t, o.checkBase(), name)
	}

	o := opts("link", "other")
	require.NoError(t, o.checkBase(), "a dangling link and a different directory")

	// A loop of links is refused, not followed forever.
	require.NoError(t, os.Symlink("loop-b", "loop-a"))
	require.NoError(t, os.Symlink("loop-a", "loop-b"))

	o = opts("loop-a", "other")
	require.Error(t, o.checkBase())
}
