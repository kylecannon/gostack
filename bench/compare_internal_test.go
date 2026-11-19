package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var hostA = fingerprint{OS: "linux", Arch: "aarch64", CPUs: 10, Docker: "29.8.2", OrbStack: true}

func around(median float64) spread {
	return spread{Min: median * 0.95, Median: median, Max: median * 1.05}
}

// testCell is a cell whose every figure is the given median, with a 5% range.
func testCell(id string, p50, p99, rate, rss float64) cellResult {
	op := opSpread{Count: 1000, P50us: around(p50), P99us: around(p99)}

	return cellResult{
		ID: id, Spec: cellSpec{Service: serviceSQS, Workload: workloadSQS}, Runs: 3,
		Ops:          map[string]opSpread{opSend: op, opReceive: op, opDelete: op},
		CyclesPerSec: around(rate), PeakRSSBytes: around(rss),
	}
}

func testReport(host fingerprint, cells ...cellResult) report {
	return report{
		Schema: reportSchema, Date: "2026-11-18", Time: "2026-11-18T10:00:00Z", Commit: "abc1234", Profile: profileQuick,
		Host: hostInfo{Fingerprint: host},
		Targets: []targetResult{{
			Name: targetGostack, Cells: cells,
			Startup: startupResult{MS: around(200)},
			Idle:    idleResult{Healthcheck: idleVariant{RSSBytes: around(5 * bytesPerMiB)}},
		}},
	}
}

const cellID = "sqs/messaging/persist=off/payload=256B/queue=standard/shape=single/proto=json/c=16"

func compared(t *testing.T, base, head report) (string, error) {
	t.Helper()

	var out bytes.Buffer

	err := compareReports(t.Context(), base, head, acceptedSet{}, &out)

	return out.String(), err
}

func Test_compare_a_regression_fails(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	// p50 +25%, past the 15% limit, and the runs do not overlap.
	head := testReport(hostA, testCell(cellID, 1000, 13000, 2700, 50*bytesPerMiB))

	out, err := compared(t, base, head)

	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "### Regressions (3)", "send, receive and delete p50")
	assert.Contains(t, out, "send p50")
	assert.Contains(t, out, cellID)
}

func Test_compare_each_metric_has_its_own_limit(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))

	tests := []struct {
		name string
		head cellResult
		fail bool
	}{
		{name: "p50 +14% passes", head: testCell(cellID, 800*1.14, 10000, 2700, 50*bytesPerMiB)},
		{name: "p99 +29% passes", head: testCell(cellID, 800, 10000*1.29, 2700, 50*bytesPerMiB)},
		{name: "p99 +40% fails", head: testCell(cellID, 800, 10000*1.4, 2700, 50*bytesPerMiB), fail: true},
		{name: "throughput -10% passes", head: testCell(cellID, 800, 10000, 2700*0.9, 50*bytesPerMiB)},
		{name: "throughput -20% fails", head: testCell(cellID, 800, 10000, 2700*0.8, 50*bytesPerMiB), fail: true},
		{name: "peak RSS +25% fails", head: testCell(cellID, 800, 10000, 2700, 62.5*bytesPerMiB), fail: true},
		{name: "peak RSS +15% passes", head: testCell(cellID, 800, 10000, 2700, 57.5*bytesPerMiB)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compared(t, base, testReport(hostA, tt.head))
			if tt.fail {
				require.ErrorIs(t, err, errRegression)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func Test_compare_latency_needs_an_absolute_rise_as_well(t *testing.T) {
	// Latency is held in microseconds in the results.
	tests := []struct {
		name     string
		base     float64
		head     float64
		p99, bad bool
	}{
		{name: "p50 0.07 to 0.08 ms is a tick", base: 70, head: 80},
		{name: "p50 1.0 to 1.5 ms regresses", base: 1000, head: 1500, bad: true},
		{name: "p99 1.17 to 1.79 ms passes both rules", base: 1170, head: 1790, p99: true, bad: true},
		{name: "p99 1.17 to 1.40 ms is under the floor", base: 1170, head: 1400, p99: true},
		{name: "p50 0.06 to 0.08 ms is +33% but under the floor", base: 60, head: 80},
		{name: "p99 1.0 to 1.4 ms is +40% but under the floor", base: 1000, head: 1400, p99: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cell := func(v float64) cellResult {
				if tt.p99 {
					return testCell(cellID, 100, v, 2700, 50*bytesPerMiB)
				}

				return testCell(cellID, v, 100000, 2700, 50*bytesPerMiB)
			}

			_, err := compared(t, testReport(hostA, cell(tt.base)), testReport(hostA, cell(tt.head)))
			if tt.bad {
				require.ErrorIs(t, err, errRegression)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func Test_compare_startup_and_idle_rss_gate(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))

	slowStart := base
	slowStart.Targets = []targetResult{base.Targets[0]}
	slowStart.Targets[0].Startup = startupResult{MS: around(260)} // +30%

	out, err := compared(t, base, slowStart)
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "startup")

	fatIdle := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	fatIdle.Targets[0].Idle = idleResult{Healthcheck: idleVariant{RSSBytes: around(7 * bytesPerMiB)}} // +40%

	out, err = compared(t, base, fatIdle)
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "idle RSS")
}

func Test_compare_a_noisy_change_with_overlapping_runs_passes(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	// The median is 30% up, but one head run is as fast as the base's slowest.
	noisy := testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB)
	send := noisy.Ops[opSend]
	send.P50us = spread{Min: 830, Median: 1040, Max: 1300}
	noisy.Ops[opSend] = send
	head := testReport(hostA, noisy)

	out, err := compared(t, base, head)

	require.NoError(t, err)
	assert.Contains(t, out, "noise, not failing) (1)")
	assert.Contains(t, out, "### Regressions (0)")
}

func Test_compare_an_improvement_passes(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	head := testReport(hostA, testCell(cellID, 500, 6000, 4000, 40*bytesPerMiB))

	out, err := compared(t, base, head)

	require.NoError(t, err)
	assert.Contains(t, out, "### Regressions (0)")
	assert.NotContains(t, out, "### Improvements (0)")
}

func Test_compare_a_cell_missing_from_the_head_is_listed_and_fails(t *testing.T) {
	other := "sqs/messaging/persist=off/payload=1MiB/queue=fifo/shape=batch/proto=query/c=64"
	base := testReport(hostA,
		testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB), testCell(other, 5000, 9000, 100, 90*bytesPerMiB))
	head := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))

	out, err := compared(t, base, head)

	require.ErrorIs(t, err, errRunProblem)
	assert.Contains(t, out, "Compared 1 cells")
	assert.Contains(t, out, "### Not compared")
	assert.Contains(t, out, "| gostack | `"+other+"` | missing from the head | 3 runs | absent | **fails** |")
}

func Test_compare_refuses_a_different_host(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))

	otherHost := hostA
	otherHost.CPUs = 4

	// The head is far slower. Different hosts must still not be called a regression.
	out, err := compared(t, base, testReport(otherHost, testCell(cellID, 8000, 100000, 270, 500*bytesPerMiB)))

	require.NoError(t, err)
	assert.Contains(t, out, "Not compared")
	assert.Contains(t, out, "10 CPUs")
	assert.Contains(t, out, "4 CPUs")
	assert.NotContains(t, out, "### Regressions")

	for _, change := range []func(*fingerprint){
		func(f *fingerprint) { f.OS = "darwin" }, func(f *fingerprint) { f.Arch = "x86_64" },
		func(f *fingerprint) { f.Docker = "28.0.0" }, func(f *fingerprint) { f.OrbStack = false },
	} {
		host := hostA
		change(&host)

		out, err = compared(t, base, testReport(host, testCell(cellID, 8000, 100000, 270, 500*bytesPerMiB)))
		require.NoError(t, err)
		assert.Contains(t, out, "Not compared")
	}
}

func Test_compare_ignores_localstack_and_old_files(t *testing.T) {
	base := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	head := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))

	slow := head.Targets[0]
	slow.Name = targetLocalstack
	slow.Cells = []cellResult{testCell(cellID, 80000, 100000, 27, 500*bytesPerMiB)}
	base.Targets = append(base.Targets, targetResult{Name: targetLocalstack, Cells: base.Targets[0].Cells})
	head.Targets = append(head.Targets, slow)

	_, err := compared(t, base, head)
	require.NoError(t, err, "LocalStack numbers never gate")

	// A file of the first schema has no cells and no fingerprint.
	old := report{Date: "2026-11-18", Commit: "bc1968c"}

	out, err := compared(t, old, old)
	require.NoError(t, err)
	assert.NotContains(t, out, "### Regressions")
}

func Test_compareFiles_picks_the_newest_result_of_the_same_host(t *testing.T) {
	dir := t.TempDir()

	write := func(name, when string, host fingerprint, cells ...cellResult) string {
		rep := testReport(host, cells...)
		rep.Time = when
		path := filepath.Join(dir, name)

		raw, err := jsonMarshal(rep)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, fileMode))

		return path
	}

	good := testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB)
	write("a-old.json", "2026-11-01T00:00:00Z", hostA, testCell(cellID, 100, 100, 100, bytesPerMiB))
	write("b-newest.json", "2026-11-10T00:00:00Z", hostA, good)
	otherHost := hostA
	otherHost.CPUs = 2
	write("c-other-host.json", "2026-11-17T00:00:00Z", otherHost, testCell(cellID, 1, 1, 1, 1))
	head := write("d-head.json", "2026-11-18T00:00:00Z", hostA, testCell(cellID, 810, 10000, 2700, 50*bytesPerMiB))

	var out bytes.Buffer

	// Outside a git checkout every json file counts, and the head is skipped.
	t.Chdir(t.TempDir())

	err := compareFiles(t.Context(), "", dir, head, acceptedSet{}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Compared 1 cells")
	assert.NotContains(t, out.String(), "Not compared")

	// No file has the head's fingerprint.
	lonely := testReport(fingerprint{OS: "linux", CPUs: 1})
	raw, err := jsonMarshal(lonely)
	require.NoError(t, err)

	lonelyPath := filepath.Join(t.TempDir(), "lonely.json")
	require.NoError(t, os.WriteFile(lonelyPath, raw, fileMode))

	out.Reset()
	require.NoError(t, compareFiles(t.Context(), "", dir, lonelyPath, acceptedSet{}, &out))
	assert.Contains(t, out.String(), "has the fingerprint of this host")
}

func jsonMarshal(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	return raw, nil
}

func Test_report_markdown_needs_no_reference(t *testing.T) {
	md := testReport(hostA, testCell(cellID, 800, 60000, 2700, 50*bytesPerMiB)).markdown()

	assert.Contains(t, md, "### A gostack cell ten times slower than LocalStack")
	assert.Contains(t, md, "none (or LocalStack was not measured)")
}
