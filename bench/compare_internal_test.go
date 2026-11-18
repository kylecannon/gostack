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
		ID: id, Runs: 3, Send: op, Receive: op, Delete: op,
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

const cellID = "persist=off/payload=256B/queue=standard/shape=single/proto=json/c=16"

func compared(t *testing.T, base, head report) (string, error) {
	t.Helper()

	var out bytes.Buffer

	err := compareReports(base, head, &out)

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
	noisy.Send.P50us = spread{Min: 830, Median: 1040, Max: 1300}
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

func Test_compare_a_missing_cell_is_listed_and_does_not_fail(t *testing.T) {
	other := "persist=off/payload=1MiB/queue=fifo/shape=batch/proto=query/c=64"
	base := testReport(hostA,
		testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB), testCell(other, 5000, 9000, 100, 90*bytesPerMiB))
	head := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))

	out, err := compared(t, base, head)

	require.NoError(t, err)
	assert.Contains(t, out, "Compared 1 cells")
	assert.Contains(t, out, "### Not compared")
	assert.Contains(t, out, other)
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

	err := compareFiles(t.Context(), "", dir, head, &out)
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
	require.NoError(t, compareFiles(t.Context(), "", dir, lonelyPath, &out))
	assert.Contains(t, out.String(), "has the fingerprint of this host")
}

func jsonMarshal(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	return raw, nil
}

func Test_report_markdown_lists_the_slow_cells_and_needs_no_reference(t *testing.T) {
	slow := testCell(cellID, 800, 60000, 2700, 50*bytesPerMiB)
	slow.Spec.Concurrency = levelMid

	md := testReport(hostA, slow).markdown()

	assert.Contains(t, md, "## Cells with p99 above 50 ms at concurrency 16")
	assert.Contains(t, md, "send p99 60.0 ms")
	assert.Contains(t, md, "none (or LocalStack was not measured)")
}
