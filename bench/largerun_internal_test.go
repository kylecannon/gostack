package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryFlag is the flag that sets the memory limit of the cells of a run.
const (
	memoryFlag = "-memory"
	statAnon   = "anon"
	statMapped = "file_mapped"

	limit512m = "512m"
)

func Test_parseOptions_large_profile(t *testing.T) {
	large := "-profile=" + profileLarge

	opts, err := parseOptions([]string{large})
	require.NoError(t, err)
	require.Len(t, opts.targets, 1, "the large profile measures gostack alone")
	assert.Equal(t, targetGostack, opts.targets[0].name)
	assert.Empty(t, opts.memory, "the cell's own limit unless -memory names one")

	opts, err = parseOptions([]string{large, memoryFlag, limit512m})
	require.NoError(t, err)
	assert.Equal(t, limit512m, opts.memory)

	for _, bad := range []string{"0", "1.5g", "1 g", "g", "-1", "1g --privileged"} {
		_, err = parseOptions([]string{large, memoryFlag, bad})
		require.Error(t, err, "-memory %q", bad)
	}
}

// -memory replaces the limit of a cell that has one and gives none to a cell
// that has none.
func Test_options_withMemory(t *testing.T) {
	limited := ddbLargeCell("", 100_000, levelMid)
	unlimited := ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid)

	none := options{}
	assert.Equal(t, ddbLargeMemory, none.withMemory(limited).Memory)
	assert.Empty(t, none.withMemory(unlimited).Memory)

	two := options{memory: "2g"}
	assert.Equal(t, "2g", two.withMemory(limited).Memory)
	assert.Equal(t, "dynamodb/large/persist=off/items=100k/mem=2g/c=16", two.withMemory(limited).id())
	assert.Empty(t, two.withMemory(unlimited).Memory)
	assert.Equal(t, ddbLargeMemory, limited.Memory, "the cell of the matrix is not changed")
}

func Test_cell_container_flags(t *testing.T) {
	tests := []struct {
		name string
		give cellSpec
		want []string
	}{
		{"no persistence, no limit", ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid), nil},
		{
			"persistence in memory, no limit",
			ddbCell(workloadDDBItem, ddbPayloadSmall, persistEnvDynamoDB, levelMid),
			[]string{"-e", "PERSIST_DYNAMODB=1", "--tmpfs", persistDir},
		},
		{
			"a limit, no swap",
			ddbLargeCell("", 100_000, levelMid),
			[]string{"--memory", "1g", "--memory-swap", "1g"},
		},
		{
			"persistence on disk under a limit",
			ddbLargeCell(persistEnvDynamoDB, 100_000, levelMid),
			[]string{"-e", "PERSIST_DYNAMODB=1", "--memory", "1g", "--memory-swap", "1g"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.give.containerArgs())
		})
	}
}

// A seeded workload gets the time to fill its table on top of the clock and the
// slack of any other; one that is not seeded gets what it always did.
func Test_runner_timeout_adds_the_fill(t *testing.T) {
	r := &runner{opts: options{duration: 30e9}}

	plain := r.timeout(ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid))
	assert.Equal(t, r.opts.duration+loadgenSlack, plain)

	big := r.timeout(ddbLargeCell("", 1_000_000, levelMid))
	assert.Equal(t, plain+largeTableWorkload{}.SeedTime(ddbLargeCell("", 1_000_000, levelMid)), big)
	assert.Greater(t, big, plain)
}

func Test_withFailed(t *testing.T) {
	spec := ddbLargeCell("", 1_000_000, levelMid)
	killed := failedRun{
		run: runResult{
			ItemsFilled: 86_100,
			Usage: usage{
				BaselineRSSBytes: 8 * bytesPerMiB, PeakRSSBytes: 1000 * bytesPerMiB, PeakUsageBytes: 1024 * bytesPerMiB,
			},
		},
		oomKilled: true,
	}
	reasons := []string{"run 1: the target container died during the run: OOM-killed"}

	cell := withFailed(settleCell(spec, nil, reasons), []failedRun{killed})

	assert.Equal(t, 1, cell.OOMKilled)
	assert.Equal(t, 86_100, cell.ItemsFilled)
	assert.InDelta(t, 1000*bytesPerMiB, cell.PeakRSSBytes.Median, 1, "how far the target got")
	assert.InDelta(t, 1024*bytesPerMiB, cell.PeakUsageBytes.Median, 1)
	assert.Equal(t, "**OOM-killed** at the 1g limit in 1 of 1 runs, 86100 items in, peak RSS 1000.0 MiB, "+
		"peak usage 1024.0 MiB", problemText(cell))

	// A cell some of whose runs completed keeps their figures.
	done := summarizeCell(spec, []runResult{{
		Seconds: 1, Cycles: 10, ItemsFilled: 1_000_000, Usage: usage{PeakRSSBytes: 500 * bytesPerMiB},
	}})
	done = withFailed(done, []failedRun{killed})
	assert.Equal(t, 1, done.OOMKilled)
	assert.Equal(t, 1_000_000, done.ItemsFilled)
	assert.InDelta(t, 500*bytesPerMiB, done.PeakRSSBytes.Median, 1)
}

// A run that failed without being killed still reports the memory it reached and
// how far its fill got, as one that thrashes until the deadline would.
func Test_withFailed_keeps_the_peak_of_a_run_that_was_not_killed(t *testing.T) {
	spec := ddbLargeCell("", 300_000, levelMid)
	thrashed := failedRun{run: runResult{
		ItemsFilled: 120_000,
		Usage:       usage{BaselineRSSBytes: 8 * bytesPerMiB, PeakRSSBytes: 900 * bytesPerMiB},
	}}
	noContainer := failedRun{} // failed before it had a container to read

	reasons := []string{
		failedReason(1, errors.New("deadline"), thrashed.run),
		failedReason(2, errors.New("no container"), noContainer.run),
	}
	cell := withFailed(settleCell(spec, nil, reasons), []failedRun{thrashed, noContainer})

	assert.Zero(t, cell.OOMKilled)
	assert.Equal(t, 120_000, cell.ItemsFilled)
	assert.InDelta(t, 900*bytesPerMiB, cell.PeakRSSBytes.Median, 1)
	assert.Equal(t, "run 1: deadline (120000 items in, peak RSS 900.0 MiB); run 2: no container", problemText(cell),
		"each failed run's entry ends with what it reached")

	// A cell nothing was read of says what it did before.
	empty := withFailed(settleCell(spec, nil, []string{"run 1: no container"}), []failedRun{noContainer})
	assert.Equal(t, empty.Problem, problemText(empty))
}

func Test_large_profile_report(t *testing.T) {
	spec := ddbLargeCell("", 1_000_000, levelMid)
	killed := withFailed(settleCell(spec, nil, []string{"run 1: OOM-killed"}), []failedRun{{
		run: runResult{
			ItemsFilled: 85_000,
			Usage:       usage{PeakRSSBytes: 1000 * bytesPerMiB, PeakUsageBytes: 1024 * bytesPerMiB},
		},
		oomKilled: true,
	}})
	ok := summarizeCell(ddbLargeCell("", 25_000, levelMid), []runResult{{
		Seconds: 1, Cycles: 10, ItemsFilled: 25_000, Ops: map[string]opStats{opDDBGet: {Count: 10}},
		Usage: usage{PeakRSSBytes: 155 * bytesPerMiB, PeakUsageBytes: 300 * bytesPerMiB},
	}})

	rep := testReport(hostA, ok, killed)
	rep.Profile = profileLarge
	rep.Targets[0].Startup, rep.Targets[0].Idle = startupResult{}, idleResult{}
	md := rep.markdown()

	assert.Contains(t, md, "### dynamodb/large")
	assert.Contains(t, md, "**OOM-killed** at the 1g limit")
	assert.Contains(t, md, "| Peak RSS | Peak usage | Items in |", "the columns of cells with a limit")
	assert.Contains(t, md, "| 155.0 MiB | 300.0 MiB | 25000 |")
	assert.Contains(t, md, "A cell whose id has mem=")
	assert.Contains(t, md, "highest of readings taken every 100ms", "the peaks are sampled")
	assert.NotContains(t, md, "## Image, startup and idle", "the report holds no startup or idle run")
	assert.NotContains(t, md, "## Agentic workload")
}

// The report words its memory paragraph and shows its columns by the cells that
// have a limit, not by the profile that ran them.
func Test_report_memory_columns_follow_the_cells(t *testing.T) {
	plain := testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	md := plain.markdown()

	assert.NotContains(t, md, "A cell whose id has mem=")
	assert.NotContains(t, md, "Items in")
	assert.Contains(t, md, "## Image, startup and idle", "a run that timed startup and idle writes the table")

	limited := testReport(hostA, summarizeCell(ddbLargeCell("", 25_000, levelMid), []runResult{{Seconds: 1, Cycles: 1}}))
	limited.Profile = profileQuick

	assert.Contains(t, limited.markdown(), "A cell whose id has mem=")
}

func Test_parseOptions_refuses_memory_with_a_profile_that_has_no_limit(t *testing.T) {
	for _, profile := range []string{profileFull, profileQuick} {
		_, err := parseOptions([]string{"-profile=" + profile, memoryFlag, "1g"})
		require.Error(t, err, profile)
		assert.Contains(t, err.Error(), memoryFlag, profile)
	}

	_, err := parseOptions([]string{"-profile=" + profileLarge, memoryFlag, "1g"})
	require.NoError(t, err)

	for _, good := range []string{"1g", "1G", "512m", "512M", "6m", "6144k"} {
		_, err = parseOptions([]string{"-profile=" + profileLarge, memoryFlag, good})
		require.NoError(t, err, "-memory %q", good)
	}

	// What docker run --memory refuses never reaches it.
	for _, bad := range []string{"4096", "2b", "100k", "5m", "6291455"} {
		_, err = parseOptions([]string{"-profile=" + profileLarge, memoryFlag, bad})
		require.ErrorIs(t, err, errMemory, "-memory %q", bad)
	}
}

func Test_parseMemory(t *testing.T) {
	tests := []struct {
		give string
		want uint64
		bad  bool
	}{
		{give: "1g", want: bytesPerGiB},
		{give: "1G", want: bytesPerGiB},
		{give: limit512m, want: 512 * bytesPerMiB},
		{give: "6m", want: memoryFloor},
		{give: "6144k", want: memoryFloor},
		{give: "6291456", want: memoryFloor},
		{give: "6291456b", want: memoryFloor},
		{give: "6291455", bad: true},
		{give: "5m", bad: true},
		{give: "4096", bad: true},
		{give: "2b", bad: true},
		{give: "100k", bad: true},
		{give: "0g", bad: true},
		{give: "1.5g", bad: true},
		{give: "g", bad: true},
		{give: "99999999999g", bad: true},
		{give: "99999999999999999999999", bad: true},
	}

	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			got, err := parseMemory(tt.give)
			if tt.bad {
				require.ErrorIs(t, err, errMemory)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The profiles' settings come from one table: the names an error lists, the help
// of -profile, the default idle windows and what each profile runs.
func Test_profileSpecs(t *testing.T) {
	assert.Equal(t, "full, quick or large", profileNames())
	assert.Contains(t, profileHelp(), "full: the whole matrix. quick: ")
	assert.Contains(t, profileHelp(), ". large: ")

	_, err := parseOptions([]string{"-profile=nope"})
	require.ErrorContains(t, err, "want full, quick or large")

	full, err := parseOptions([]string{"-profile=" + profileFull})
	require.NoError(t, err)
	assert.Equal(t, fullIdle, full.idle)

	quick, err := parseOptions([]string{"-profile=" + profileQuick})
	require.NoError(t, err)
	assert.Equal(t, quickSettle, quick.settle)

	for _, name := range []string{profileFull, profileQuick} {
		p, ok := profileOf(name)
		require.True(t, ok)
		assert.False(t, p.large, name)
		assert.True(t, p.footprint, name)
	}

	p, _ := profileOf(profileFull)
	assert.True(t, p.noHealthcheck)

	p, _ = profileOf(profileQuick)
	assert.False(t, p.noHealthcheck)

	p, _ = profileOf(profileLarge)
	assert.True(t, p.large)
	assert.False(t, p.footprint)
	assert.False(t, p.agentic)

	// Only the large profile lists the large workload's cells, and it lists nothing else.
	for _, c := range cellsFor(profileLarge, target{persistable: true}) {
		assert.True(t, c.limited(), c.id())
	}

	for _, name := range []string{profileFull, profileQuick} {
		for _, c := range cellsFor(name, target{persistable: true}) {
			assert.False(t, c.limited(), "%s: %s", name, c.id())
		}
	}
}

// The base and the head change places from one cell to the next, so with one
// repetition a cell runs the base first as often as the head.
func Test_cell_rounds_alternate_across_cells(t *testing.T) {
	for _, reps := range []int{1, 3} {
		r := &runner{opts: options{reps: reps}}

		var firsts []int

		for range 6 {
			first := r.takeRounds(reps)

			started := false

			alternateFrom(first, reps, 2, func(_, side int) {
				if !started {
					firsts = append(firsts, side)
					started = true
				}
			})
		}

		assert.Equal(t, []int{0, 1, 0, 1, 0, 1}, firsts, "reps=%d: the side that runs first in a cell", reps)
	}

	// One repetition: the sides take the first turn in turn.
	r := &runner{opts: options{reps: 1}}

	var order []int

	for range 4 {
		alternateFrom(r.takeRounds(1), 1, 2, func(_, side int) { order = append(order, side) })
	}

	assert.Equal(t, []int{0, 1, 1, 0, 0, 1, 1, 0}, order)
}

func Test_targetState(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  error
	}{
		{"running", "running 0 false", nil},
		{"OOM-killed", "exited 137 true", errOOMKilled},
		{"killed by something else", "exited 137 false", errTargetDied},
		{"a child was OOM-killed, the container exited later on its own", "exited 1 true", errTargetDied},
		{"exited cleanly", "exited 0 false", errTargetDied},
		{"killed while being removed", "removing 137 true", errOOMKilled},
		{"unreadable", "", errTargetDied},
		{"not a state line", "exited", errTargetDied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := targetState("c", tt.state)
			if tt.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tt.want)
			// An OOM kill is a death too; the reverse is not true.
			assert.Equal(t, errors.Is(tt.want, errOOMKilled), errors.Is(err, errOOMKilled))
			assert.ErrorIs(t, err, errTargetDied)
		})
	}
}

func Test_runError(t *testing.T) {
	killed := targetState("c", "exited 137 true")
	gone := targetState("c", "exited 1 false")
	failure := errors.New("the load generator failed")

	require.NoError(t, runError(nil, nil))
	require.ErrorIs(t, runError(nil, killed), errOOMKilled, "a run that counted on, against a container that died")
	require.ErrorIs(t, runError(nil, gone), errTargetDied)

	// A load generator that failed on a container the kernel killed says both.
	err := runError(failure, killed)
	require.ErrorIs(t, err, errOOMKilled)
	require.ErrorIs(t, err, failure)

	// On any other container it says it itself.
	require.Equal(t, failure, runError(failure, nil))
	assert.Equal(t, failure, runError(failure, gone))
}

func Test_memoryPeaks_leaves_out_the_zero_readings_of_a_stopped_container(t *testing.T) {
	reading := func(anon, mapped, usage uint64) containerStats {
		return containerStats{MemoryStats: memoryStats{
			Stats: map[string]uint64{statAnon: anon, statMapped: mapped}, Usage: usage,
		}}
	}

	var m memoryPeaks

	m.peak(reading(100, 10, 400))
	m.sample(reading(200, 20, 900))
	m.sample(containerStats{}) // a stopped container
	m.sample(reading(150, 0, 600))
	m.peak(containerStats{})

	assert.EqualValues(t, 220, m.rss)
	assert.EqualValues(t, 900, m.usage, "what the cgroup counts, page cache included")
	assert.Equal(t, []float64{220, 150}, m.samples, "no zero sample drags the median down")
	assert.InDelta(t, 185, median(m.samples), 0)

	// A kernel that keeps a high-water mark lends it to the peak.
	marked := reading(1, 1, 300)
	marked.MemoryStats.MaxUsage = 700

	m.peak(marked)
	assert.EqualValues(t, 900, m.usage)

	m.peak(containerStats{MemoryStats: memoryStats{Usage: 2000, MaxUsage: 2500}})
	assert.EqualValues(t, 2500, m.usage)
}

// A window whose container stopped before the last reading keeps the peaks it
// reached and reads no CPU time off the zero counters.
func Test_windowUsage(t *testing.T) {
	first := containerStats{
		Read:        time.Unix(100, 0),
		CPUStats:    cpuStats{CPUUsage: cpuUsage{TotalUsage: 5_000_000_000}},
		MemoryStats: memoryStats{Stats: map[string]uint64{statAnon: 10, statMapped: 0}, Usage: 20},
	}
	last := containerStats{
		Read:        time.Unix(102, 0),
		CPUStats:    cpuStats{CPUUsage: cpuUsage{TotalUsage: 6_000_000_000}},
		MemoryStats: memoryStats{Stats: map[string]uint64{statAnon: 50, statMapped: 0}, Usage: 80},
	}
	peaks := memoryPeaks{rss: 60, usage: 90, samples: []float64{30, 60}}

	got := windowUsage(first, last, peaks)
	assert.EqualValues(t, 10, got.BaselineRSSBytes)
	assert.EqualValues(t, 50, got.EndRSSBytes)
	assert.EqualValues(t, 60, got.PeakRSSBytes)
	assert.EqualValues(t, 90, got.PeakUsageBytes)
	assert.EqualValues(t, 45, got.MedianRSSBytes)
	assert.InDelta(t, 50, got.CPUPercent, 0.001, "a second of CPU in two")

	stopped := windowUsage(first, containerStats{Read: time.Unix(102, 0)}, peaks)
	assert.EqualValues(t, 60, stopped.PeakRSSBytes)
	assert.EqualValues(t, 90, stopped.PeakUsageBytes)
	assert.Zero(t, stopped.CPUPercent, "the zero counters of a stopped container are no end of the window")
	assert.Zero(t, stopped.EndRSSBytes)
}

func Test_runResult_opErrors(t *testing.T) {
	run := runResult{Ops: map[string]opStats{opDDBGet: {Count: 9, Errors: 2}, opDDBPut: {Errors: 1}}}

	assert.Equal(t, 3, run.opErrors())
	assert.Zero(t, runResult{}.opErrors())
}

// fakeDocker puts a docker on the PATH whose `run` prints out and exits with
// status, and whose every other command succeeds.
func fakeDocker(t *testing.T, out string, status int) {
	t.Helper()

	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = run ]; then\n  printf '%%s' '%s'\n  exit %d\nfi\nexit 0\n",
		out, status)

	installDocker(t, script)
}

// fakeInspect puts a docker on the PATH whose `inspect` prints the states in turn
// and then the last one for ever, as a container does that stops while it is read.
func fakeInspect(t *testing.T, states ...string) {
	t.Helper()

	var cases strings.Builder

	for i, state := range states[:len(states)-1] {
		fmt.Fprintf(&cases, "  %d) echo '%s';;\n", i+1, state)
	}

	fmt.Fprintf(&cases, "  *) echo '%s';;\n", states[len(states)-1])

	count := filepath.Join(t.TempDir(), "count")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = inspect ]; then\n  n=$(cat %s 2>/dev/null || echo 0)\n"+
		"  n=$((n + 1))\n  echo $n > %s\n  case $n in\n%s  esac\nfi\nexit 0\n", count, count, cases.String())

	installDocker(t, script)
}

func installDocker(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A container the daemon has not yet recorded the death of reads as running to a
// single look, and as OOM-killed to a run that waits for it to stop.
func Test_instance_aliveWithin_waits_for_the_container_to_stop(t *testing.T) {
	inst := &instance{name: "target"}

	fakeInspect(t, "running 0 false", "exited 137 true")
	require.NoError(t, inst.alive(t.Context()), "one look, as a run that completed takes")

	fakeInspect(t, "running 0 false", "running 0 false", "exited 137 true")
	require.ErrorIs(t, inst.aliveWithin(t.Context(), oomSettle), errOOMKilled)

	// A container that is still running when the wait ends is alive.
	fakeInspect(t, "running 0 false")
	require.NoError(t, inst.aliveWithin(t.Context(), 300*time.Millisecond))

	// One that died of something else is a death and no OOM kill.
	fakeInspect(t, "running 0 false", "exited 1 false")

	err := inst.aliveWithin(t.Context(), oomSettle)
	require.ErrorIs(t, err, errTargetDied)
	require.NotErrorIs(t, err, errOOMKilled)
}

// A load generator that fails after it printed how far its fill got has that kept,
// with the failure as the error.
func Test_runner_loadgen_keeps_what_a_failed_load_generator_printed(t *testing.T) {
	fakeDocker(t, `{"items_filled":86100}`, 1)

	r := &runner{opts: options{loadgenImage: "loadgen"}}

	var run runResult

	err := r.loadgen(t.Context(), "target", time.Minute, &run, "cell")
	require.Error(t, err)
	assert.Equal(t, 86_100, run.ItemsFilled)

	// A load generator that printed nothing leaves the result as it was.
	fakeDocker(t, "", 1)

	var none runResult

	require.Error(t, r.loadgen(t.Context(), "target", time.Minute, &none, "cell"))
	assert.Zero(t, none.ItemsFilled)

	// One that completed decodes as it always did.
	fakeDocker(t, `{"cycles":7,"items_filled":300}`, 0)

	var done runResult

	require.NoError(t, r.loadgen(t.Context(), "target", time.Minute, &done, "cell"))
	assert.Equal(t, 7, done.Cycles)
	assert.Equal(t, 300, done.ItemsFilled)
}

// failingFillItems is a table of one batch more than the fill has workers.
const failingFillItems = (ddbSeedWorkers + 1) * ddbBatchSize

// failingDynamoDB answers CreateTable and the first BatchWriteItem, holds the
// next ones and refuses them, as a target does that has just been killed, when
// the batch past one a worker each arrives. Only a worker whose batch was
// answered and counted asks for another, so the refusals, which cancel the fill,
// cannot come before the count.
func failingDynamoDB(t *testing.T) client {
	t.Helper()

	var calls atomic.Int64

	refuse := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n := calls.Add(1); {
		case n <= 2: // CreateTable, and the first batch
			_, _ = w.Write([]byte(`{}`))
		case n == ddbSeedWorkers+2: // the batch past a worker each
			close(refuse)
			http.Error(w, "gone", http.StatusServiceUnavailable)
		default:
			<-refuse
			http.Error(w, "gone", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)

	return client{endpoint: srv.URL}
}

// A fill a failure ended says how many items the server had acknowledged, in the
// run's own field and not only in its message.
func Test_large_table_run_reports_how_far_its_fill_got(t *testing.T) {
	cell := ddbLargeCell("", failingFillItems, levelMid)

	run, err := largeTableWorkload{}.Run(t.Context(), failingDynamoDB(t), cell, time.Millisecond)

	require.Error(t, err)
	assert.Equal(t, ddbBatchSize, run.ItemsFilled, "one batch of 25 was acknowledged")
	assert.Contains(t, err.Error(), fmt.Sprintf("seeded %d of %d items", ddbBatchSize, failingFillItems))

	// loadgen cell prints the run when the fill failed, so the orchestrator can read it.
	var out bytes.Buffer

	err = loadgenMain(t.Context(), []string{
		"cell", "-endpoint", failingDynamoDB(t).endpoint, "-service", serviceDynamoDB,
		"-workload", workloadDDBLarge, "-opt", optItems + "=" + strconv.Itoa(failingFillItems),
		"-concurrency", "16", "-duration", "1ms",
	}, &out)
	require.Error(t, err)

	var printed runResult

	require.NoError(t, json.Unmarshal(out.Bytes(), &printed), "stdout: %q", out.String())
	assert.Equal(t, ddbBatchSize, printed.ItemsFilled)
}

// A run that completed counts every item the fill put.
func Test_large_table_run_counts_a_completed_fill(t *testing.T) {
	cell := ddbLargeCell("", 300, levelMid)

	run, err := largeTableWorkload{}.Run(t.Context(), ddbServer(t), cell, 50*time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, 300, run.ItemsFilled)

	raw, err := json.Marshal(run)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"items_filled":300`)
}

// A run waits for its container to stop only when something says it may have
// died: a load generator that failed, or operations that failed under a limit.
func Test_cellSpec_settleWait(t *testing.T) {
	limited := ddbLargeCell("", 25_000, levelMid)
	plain := ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid)
	failure := errors.New("the load generator failed")
	clean := runResult{Ops: map[string]opStats{opDDBGet: {Count: 10}}}
	erroring := runResult{Ops: map[string]opStats{opDDBGet: {Count: 9, Errors: 1}}}

	tests := []struct {
		name string
		spec cellSpec
		err  error
		run  runResult
		want time.Duration
	}{
		{"no limit, nothing failed", plain, nil, clean, 0},
		{"no limit, operations failed", plain, nil, erroring, 0},
		{"no limit, the load generator failed", plain, failure, clean, oomSettle},
		{"a limit, nothing failed", limited, nil, clean, 0},
		{"a limit, operations failed", limited, nil, erroring, oomSettle},
		{"a limit, the load generator failed", limited, failure, clean, oomSettle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.spec.settleWait(tt.err, tt.run))
		})
	}
}

// endRun reads the container after the wait settleWait names, so a container the
// daemon has not yet recorded the death of is an OOM kill and not a survivor.
func Test_endRun_waits_for_the_container_of_a_run_that_failed(t *testing.T) {
	inst := &instance{name: "target"}
	limited := ddbLargeCell("", 25_000, levelMid)
	plain := ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid)
	failure := errors.New("the load generator failed")
	erroring := runResult{ItemsFilled: 5, Ops: map[string]opStats{opDDBGet: {Errors: 1}}}

	// The load generator failed before the daemon recorded the kill.
	fakeInspect(t, "running 0 false", "running 0 false", "exited 137 true")

	run, err := endRun(t.Context(), inst, limited, runResult{ItemsFilled: 5}, failure)
	require.ErrorIs(t, err, errOOMKilled)
	require.ErrorIs(t, err, failure)
	assert.Equal(t, 5, run.ItemsFilled, "the run is returned with the error")

	// Operations failed under a limit as the container was dying.
	fakeInspect(t, "running 0 false", "exited 137 true")
	_, err = endRun(t.Context(), inst, limited, erroring, nil)
	require.ErrorIs(t, err, errOOMKilled)

	// Without a limit there is no kill to wait for: the run reads the state once.
	fakeInspect(t, "running 0 false", "exited 137 true")

	_, err = endRun(t.Context(), inst, plain, erroring, nil)
	require.NoError(t, err)

	// Nothing failed: once.
	fakeInspect(t, "running 0 false", "exited 137 true")

	_, err = endRun(t.Context(), inst, limited, runResult{}, nil)
	require.NoError(t, err)
}

func Test_failedReason(t *testing.T) {
	failure := errors.New("deadline")

	assert.Equal(t, "run 3: deadline", failedReason(3, failure, runResult{}), "nothing was read of it")
	assert.Equal(t, "run 2: deadline (86100 items in, peak RSS 1000.0 MiB, peak usage 1024.0 MiB)",
		failedReason(2, failure, runResult{
			ItemsFilled: 86_100,
			Usage:       usage{PeakRSSBytes: 1000 * bytesPerMiB, PeakUsageBytes: 1024 * bytesPerMiB},
		}))
	assert.Equal(t, "run 1: deadline (peak RSS 1.0 MiB)",
		failedReason(1, failure, runResult{Usage: usage{PeakRSSBytes: bytesPerMiB}}))
}

// fakeRuns is a runner whose runs of a cell are the result function's, and the
// calls it logs: the side and the cell of each, in order.
func fakeRuns(reps int, result func(call int) (runResult, error)) (*runner, *[]string) {
	var calls []string

	r := &runner{opts: options{reps: reps}, log: io.Discard}
	r.runCell = func(_ context.Context, t target, spec cellSpec) (runResult, error) {
		calls = append(calls, t.side+" "+spec.id())

		return result(len(calls) - 1)
	}

	return r, &calls
}

// Across cells, the side that runs first changes: with one repetition a cell has
// one round, and the head would otherwise come second in every cell.
func Test_runner_cell_runs_the_sides_in_turn_across_cells(t *testing.T) {
	r, calls := fakeRuns(1, func(int) (runResult, error) { return runResult{Seconds: 1, Cycles: 1}, nil })
	sides := []target{{name: targetGostack, side: sideBase}, {name: targetGostack, side: sideHead}}
	a, b, c := ddbLargeCell("", 25_000, levelMid), ddbLargeCell("", 50_000, levelMid), ddbLargeCell("", 100_000, levelMid)

	for _, spec := range []cellSpec{a, b, c} {
		cells := r.cell(t.Context(), sides, spec)
		require.Len(t, cells, len(sides))
	}

	assert.Equal(t, []string{
		"base " + a.id(), "head " + a.id(),
		"head " + b.id(), "base " + b.id(),
		"base " + c.id(), "head " + c.id(),
	}, *calls)
}

// A failed run keeps what it reached when other runs of its cell completed: the
// cell counts the kill and names the run's memory and fill in its entry.
func Test_runner_cell_keeps_a_failed_run_beside_the_completed_ones(t *testing.T) {
	spec := ddbLargeCell("", 1_000_000, levelMid)
	killedErr := fmt.Errorf("%w: exited 137 true", errOOMKilled)
	killed := runResult{
		ItemsFilled: 86_000,
		Usage:       usage{PeakRSSBytes: 1000 * bytesPerMiB, PeakUsageBytes: 1024 * bytesPerMiB},
	}
	done := runResult{Seconds: 1, Cycles: 10, ItemsFilled: 1_000_000, Usage: usage{PeakRSSBytes: 500 * bytesPerMiB}}

	r, calls := fakeRuns(3, func(call int) (runResult, error) {
		if call == 1 {
			return killed, killedErr
		}

		return done, nil
	})

	cells := r.cell(t.Context(), []target{{name: targetGostack}}, spec)
	require.Len(t, cells, 1)
	require.Len(t, *calls, 3)

	cell := cells[0]
	assert.Equal(t, 2, cell.Runs)
	assert.Equal(t, 1, cell.OOMKilled)
	assert.Equal(t, []string{
		"run 2: " + killedErr.Error() + " (86000 items in, peak RSS 1000.0 MiB, peak usage 1024.0 MiB)",
	}, cell.FailedRuns)
	assert.InDelta(t, 500*bytesPerMiB, cell.PeakRSSBytes.Median, 1, "the completed runs' figures")
	assert.Empty(t, cell.Problem)
}
