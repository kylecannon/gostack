package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRuns is how many runs a side of these tests has.
const (
	testRuns    = 30
	serviceFlag = "-service"
)

// ramp is testRuns runs from start, step apart.
func ramp(start, step float64) []float64 {
	runs := make([]float64, testRuns)
	for i := range runs {
		runs[i] = start + float64(i)*step
	}

	return runs
}

// firstUseReport is a report whose one target has these runs as its first use of
// SNS, and no cell.
func firstUseReport(runs []float64) report {
	rep := testReport(hostA)
	rep.Targets[0].FirstUse = map[string]startupResult{serviceSNS: {SamplesMS: runs, MS: spreadOf(runs)}}

	return rep
}

// `loadgen first` runs the workload against SNS and SQS in this process, with
// SNS delivering through the gateway, and prints a time: the whole path the
// orchestrator takes, down to the JSON it decodes.
func Test_loadgen_first_times_a_signed_sns_delivery(t *testing.T) {
	c := snsServer(t)

	var out bytes.Buffer

	err := loadgenMain(t.Context(), []string{"first", serviceFlag, serviceSNS, "-endpoint", c.endpoint}, &out)
	require.NoError(t, err)

	var run firstUseRun

	require.NoError(t, json.Unmarshal(out.Bytes(), &run))
	assert.Positive(t, run.Micros, "the clock ran from CreateTopic to the signed delivery")
}

func Test_loadgen_first_refuses_a_service_with_no_first_use(t *testing.T) {
	var out bytes.Buffer

	err := loadgenMain(t.Context(), []string{"first", serviceFlag, serviceSQS}, &out)

	require.ErrorIs(t, err, errNoWorkload)
	assert.Empty(t, out.String())
}

func Test_rankSumZ_is_zero_for_one_set_of_runs_and_reaches_its_largest_for_two_apart(t *testing.T) {
	runs := ramp(100, 3)
	same := metric{baseRuns: runs, headRuns: runs}
	assert.InDelta(t, 0, same.rankSumZ(), 1e-9, "every pair of the same runs is a tie or one win for each")

	apart := metric{baseRuns: runs, headRuns: ramp(1000, 3)}
	assert.InDelta(t, apart.rankReach(), apart.rankSumZ(), 1e-9)
	assert.Greater(t, apart.rankReach(), rankSumLimit, "30 runs a side can fail a metric")

	few := metric{baseRuns: runs[:4], headRuns: runs[:4]}
	assert.Less(t, few.rankReach(), rankSumLimit, "4 runs a side never reach z 2.6")
}

// A doubled key generation spreads over the base's runs, so the ranges overlap
// and the old rule would call it noise; the rank test fails it.
func Test_compare_a_doubled_first_use_fails_though_the_ranges_overlap(t *testing.T) {
	base := firstUseReport(ramp(100, 3))   // 100 to 187 ms, median 143.5
	head := firstUseReport(ramp(150, 6.5)) // 150 to 338 ms, median 244

	require.Less(t, head.Targets[0].FirstUse[serviceSNS].MS.Min, base.Targets[0].FirstUse[serviceSNS].MS.Max,
		"the ranges overlap")

	out, err := compared(t, base, head)

	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "### Regressions (1)")
	assert.Contains(t, out, "first use")
	assert.Contains(t, out, firstUseCell(serviceSNS))
}

// The accepted list holds a first use regression by cell and metric name, and a
// bound at or under the 25% the gate flags holds none.
func Test_accepted_list_holds_a_first_use_regression(t *testing.T) {
	base := firstUseReport(ramp(100, 3))
	head := firstUseReport(ramp(150, 6.5)) // +70%

	base.Commit, head.Commit = baseCommit, headCommit

	rename := strings.NewReplacer("cell: idle", "cell: "+firstUseCell(serviceSNS),
		"metric: idle RSS", "metric: "+metricFirstUse)
	entry := rename.Replace(idleEntry)

	for _, tt := range []struct {
		max, note string
		held      bool
	}{
		{max: "+100%", held: true},
		{max: "+60%", note: "past the accepted 60%"},
		{max: "+25%", note: "so it holds none"},
	} {
		entries, err := parseAccepted([]byte(strings.Replace(entry, "%s", tt.max, 1)), "test")
		require.NoError(t, err)

		var out bytes.Buffer

		err = compareReports(t.Context(), base, head, acceptedSet{entries: entries, contains: history(false)}, &out)
		if tt.held {
			require.NoError(t, err, tt.max)
			assert.Contains(t, out.String(), "### Accepted (1)")

			continue
		}

		require.ErrorIs(t, err, errRegression, tt.max)
		assert.Contains(t, out.String(), tt.note)
	}
}

func Test_compare_a_first_use_equal_to_the_base_passes(t *testing.T) {
	runs := ramp(100, 3)

	out, err := compared(t, firstUseReport(runs), firstUseReport(slicesReversed(runs)))

	require.NoError(t, err)
	assert.Contains(t, out, "and 3 metrics", "startup, idle RSS and the first use were compared")
	assert.Contains(t, out, "### Regressions (0)")
	assert.Contains(t, out, "### Changes past the limit with overlapping ranges (noise, not failing) (0)")
}

func Test_compare_a_rise_past_the_limit_that_the_rank_test_does_not_back_is_noise(t *testing.T) {
	base := firstUseReport([]float64{100, 120, 130, 500})
	head := firstUseReport([]float64{90, 160, 170, 520}) // the median is +32%

	out, err := compared(t, base, head)

	require.NoError(t, err)
	assert.Contains(t, out, "noise")
	assert.Contains(t, out, "too few for the rank-sum test to fail it", "4 runs a side never reach z 2.6")
}

func Test_compare_a_first_use_the_base_lacks_is_not_compared(t *testing.T) {
	head := firstUseReport(ramp(150, 6.5))

	out, err := compared(t, testReport(hostA), head)

	require.NoError(t, err)
	assert.Contains(t, out, "and 2 metrics", "only startup and idle RSS: no first use in the base, nothing to hold it to")
	assert.Contains(t, out, "### Errors and failed runs (0, 0 failing)", "and it is no problem")
}

func Test_compare_a_first_use_that_got_faster_is_an_improvement(t *testing.T) {
	out, err := compared(t, firstUseReport(ramp(150, 6.5)), firstUseReport(ramp(100, 3)))

	require.NoError(t, err)
	assert.Contains(t, out, "### Improvements")
}

func Test_first_use_is_written_to_the_result_and_the_report(t *testing.T) {
	rep := firstUseReport([]float64{101.5, 99.5})

	written, err := json.Marshal(rep)
	require.NoError(t, err)

	var back report

	require.NoError(t, json.Unmarshal(written, &back))
	assert.Equal(t, []float64{101.5, 99.5}, back.Targets[0].FirstUse[serviceSNS].SamplesMS, "the runs are kept whole")

	md := rep.markdown()
	assert.Contains(t, md, "## First use")
	assert.Contains(t, md, firstUseCell(serviceSNS))
	assert.NotContains(t, testReport(hostA).markdown(), "## First use")
}

func Test_options_refuse_no_first_use_runs(t *testing.T) {
	_, err := parseOptions([]string{"-first-use-runs", "0"})
	require.Error(t, err)

	opts, err := parseOptions(nil)
	require.NoError(t, err)
	assert.Equal(t, defaultFirstUseRuns, opts.firstUseRuns)
}

func slicesReversed(runs []float64) []float64 {
	out := make([]float64, len(runs))
	for i, v := range runs {
		out[len(runs)-1-i] = v
	}

	return out
}
