package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_parseOptions_large_profile(t *testing.T) {
	large := "-profile=" + profileLarge

	opts, err := parseOptions([]string{large})
	require.NoError(t, err)
	require.Len(t, opts.targets, 1, "the large profile measures gostack alone")
	assert.Equal(t, targetGostack, opts.targets[0].name)
	assert.Empty(t, opts.memory, "the cell's own limit unless -memory names one")

	opts, err = parseOptions([]string{large, "-memory", "512m"})
	require.NoError(t, err)
	assert.Equal(t, "512m", opts.memory)

	for _, bad := range []string{"0", "1.5g", "1 g", "g", "-1", "1g --privileged"} {
		_, err = parseOptions([]string{large, "-memory", bad})
		require.Error(t, err, "-memory %q", bad)
	}
}

// -memory replaces the limit of a cell that has one and gives none to a cell
// that has none.
func Test_options_limited(t *testing.T) {
	limited := ddbLargeCell("", 100_000, levelMid)
	unlimited := ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid)

	none := options{}
	assert.Equal(t, ddbLargeMemory, none.limited(limited).Memory)
	assert.Empty(t, none.limited(unlimited).Memory)

	two := options{memory: "2g"}
	assert.Equal(t, "2g", two.limited(limited).Memory)
	assert.Equal(t, "dynamodb/large/persist=off/items=100k/mem=2g/c=16", two.limited(limited).id())
	assert.Empty(t, two.limited(unlimited).Memory)
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

func Test_noteOOM(t *testing.T) {
	spec := ddbLargeCell("", 1_000_000, levelMid)
	killed := []runResult{{Usage: usage{BaselineRSSBytes: 8 * bytesPerMiB, PeakRSSBytes: 1000 * bytesPerMiB}}}

	cell := withOOM(settleCell(spec, nil, []string{"run 1: the target container died during the run: OOM-killed"}), killed)

	assert.Equal(t, 1, cell.OOMKilled)
	assert.InDelta(t, 1000*bytesPerMiB, cell.PeakRSSBytes.Median, 1, "how far the target got")
	assert.Equal(t, "**OOM-killed** at the 1g limit in 1 of 1 runs, peak RSS 1000.0 MiB", problemText(cell))

	// A cell some of whose runs completed keeps their figures.
	done := summarizeCell(spec, []runResult{{Seconds: 1, Cycles: 10, Usage: usage{PeakRSSBytes: 500 * bytesPerMiB}}})
	done = withOOM(done, killed)
	assert.Equal(t, 1, done.OOMKilled)
	assert.InDelta(t, 500*bytesPerMiB, done.PeakRSSBytes.Median, 1)

	// A cell no run of which was killed says what it did before.
	plain := settleCell(spec, nil, []string{"run 1: the load generator failed"})
	plain = withOOM(plain, nil)
	assert.Zero(t, plain.OOMKilled)
	assert.Equal(t, plain.Problem, problemText(plain))
}

func Test_large_profile_report(t *testing.T) {
	spec := ddbLargeCell("", 1_000_000, levelMid)
	killed := withOOM(settleCell(spec, nil, []string{"run 1: OOM-killed"}),
		[]runResult{{Usage: usage{PeakRSSBytes: 1000 * bytesPerMiB}}})

	rep := testReport(hostA, killed)
	rep.Profile = profileLarge
	md := rep.markdown()

	assert.Contains(t, md, "### dynamodb/large")
	assert.Contains(t, md, "**OOM-killed** at the 1g limit")
	assert.Contains(t, md, "The large profile fills a table")
	assert.NotContains(t, md, "## Image, startup and idle", "the large profile measures none of that")
	assert.NotContains(t, md, "## Agentic workload")
}
