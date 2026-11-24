package persist

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here pin what the abandoned-save marker costs a flush that is in
// time (SQ-461): PR #56's benchmark gate read two more allocations per flush
// for the allowance, seven for the deadline's AfterFunc and the run's map, and
// ten for a lstat of the marker, on a save of 157 allocations
// (BenchmarkSavePendingAssets100, which timed an engine's first save on a
// runner; the 174 it read is 10.83% more). Its successor,
// BenchmarkSavePending100, times later saves: 156 allocations in a 4 CPU
// linux/arm64 container, where a first save takes 160.

// markerPath returns where the marker of the service "svc" is in dir.
func markerPath(dir string) string {
	return filepath.Join(dir, "svc", abandonedFile)
}

// Test_sweepTemps checks that the listing a sweep reads says whether an entry
// called seek is in it, and that the sweep removes the temp files of name as it
// did, whatever seek is.
func Test_sweepTemps(t *testing.T) {
	const (
		mark   = abandonedFile
		folded = ".GOSTACK-SAVE-ABANDONED"
		state  = "state.json"
		temp   = state + tempSuffix + "-1"
		other  = "other.json" + tempSuffix + "-1"
	)

	tests := []struct {
		name     string
		seek     string
		entries  []string
		found    bool
		leftOver []string
	}{
		{"marker among temp files", mark, []string{state, temp, mark}, true, []string{state, mark}},
		{"no marker", mark, []string{state, temp}, false, []string{state}},
		{"no seek", "", []string{state, mark}, false, []string{state, mark}},
		{"a temp file of another name stays", mark, []string{other}, false, []string{other}},
		{"marker in another case", mark, []string{state, folded}, true, []string{state, folded}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(dir, "svc"), 0o750))

			for _, entry := range tt.entries {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "svc", entry), nil, 0o600))
			}

			root, err := os.OpenRoot(dir)
			require.NoError(t, err)

			defer func() { _ = root.Close() }()

			found, err := sweepTemps(root, "svc", state, tt.seek)
			require.NoError(t, err)
			assert.Equal(t, tt.found, found)

			left, err := os.ReadDir(filepath.Join(dir, "svc"))
			require.NoError(t, err)

			names := make([]string, 0, len(left))
			for _, entry := range left {
				names = append(names, entry.Name())
			}

			assert.ElementsMatch(t, tt.leftOver, names)
		})
	}
}

// Test_Engine_SaveClearsAMarkerWhereverTheStateFileIs checks that a completed
// save removes the marker of its service whether it finds it in the listing of
// the service's directory, as it does when the state file is there, or by a
// look of its own, as it does when a link leads the state file to another
// directory, where the listing is not the service's.
func Test_Engine_SaveClearsAMarkerWhereverTheStateFileIs(t *testing.T) {
	tests := []struct {
		name string
		link bool
	}{
		{"state file in the service dir", false},
		{"state file linked to another dir", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			e := New(quietConfig(dir, time.Hour))
			e.Register("svc", &stateSnap{state: "x"})
			require.NoError(t, os.Mkdir(filepath.Join(dir, "svc"), 0o750))

			if tt.link {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "elsewhere"), 0o750))

				err := os.Symlink("../elsewhere/"+FormatJSON.fileName(), filepath.Join(dir, "svc", FormatJSON.fileName()))
				if err != nil {
					t.Skipf("no links here: %v", err)
				}
			}

			require.NoError(t, os.WriteFile(markerPath(dir), nil, 0o600))

			e.MarkDirty("svc")
			require.NoError(t, e.Flush(t.Context()))

			assert.NoFileExists(t, markerPath(dir), "the save that completed removed the marker")
			assert.FileExists(t, filepath.Join(dir, "svc", FormatJSON.fileName()))
		})
	}
}

// Test_Engine_SaveKeepsTheMarkerOfAServiceChangedSinceItsSnapshot checks that
// the listing path still leaves the marker when the service changed during the
// save: the change is not saved, and the marker says so.
func Test_Engine_SaveKeepsTheMarkerOfAServiceChangedSinceItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	e := New(quietConfig(dir, time.Hour))
	snap := &stateSnap{state: "x"}
	e.Register("svc", snap)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "svc"), 0o750))
	require.NoError(t, os.WriteFile(markerPath(dir), nil, 0o600))

	snap.during = func() error {
		e.MarkDirty("svc")

		return nil
	}

	e.MarkDirty("svc")
	require.NoError(t, e.Flush(t.Context()))

	assert.FileExists(t, markerPath(dir), "a change since the snapshot keeps the marker")
}

// Test_Engine_FlushInTimeArmsNothingForAContextWithNoDeadline checks that a
// flush whose ctx has no deadline allocates nothing to mark one, and that one
// with a deadline arms its marking.
func Test_Engine_FlushInTimeArmsNothingForAContextWithNoDeadline(t *testing.T) {
	e := New(quietConfig(t.TempDir(), time.Hour))
	run := newFlushRun(e, byHand)

	assert.Zero(t, testing.AllocsPerRun(100, func() {
		e.markAtDeadline(t.Context(), run)
		run.endMark()
		run.endMark()
	}), "no deadline, nothing to arm")

	assert.False(t, hasDeadline(t.Context()))
	assert.Empty(t, e.slow.runs, "a flush that ended is not watched")

	timed, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()

	assert.NotZero(t, testing.AllocsPerRun(10, func() {
		e.markAtDeadline(timed, run)
		run.endMark()
	}), "a deadline arms it")
	assert.True(t, hasDeadline(timed))
}
