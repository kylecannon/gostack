package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// committedV2 is a full-profile result of schema 2, kept as the human record of
// the matrix before schema 3.
const committedV2 = "results/2026-11-18-7a928cc.json"

func Test_loadReport_converts_schema_2_to_schema_3(t *testing.T) {
	rep, err := loadReport(committedV2)
	require.NoError(t, err)

	assert.Equal(t, reportSchema, rep.Schema)
	assert.Equal(t, "7a928cc", rep.Commit)
	assert.True(t, rep.Host.Fingerprint.OrbStack)

	cells := map[string]int{}
	for _, tr := range rep.Targets {
		cells[tr.Name] = len(tr.Cells)
	}

	assert.Equal(t, map[string]int{targetGostack: 216, targetSlim: 216, targetLocalstack: 108}, cells)

	gostack, ok := rep.target(targetGostack)
	require.True(t, ok)

	first := gostack.Cells[0]
	assert.Equal(t, "sqs/messaging/persist=off/payload=256B/queue=standard/shape=single/proto=json/c=1", first.ID)
	assert.Equal(t, sqsCellSpec(payloadSmall, queueStandard, shapeSingle, protoJSON, "", levelLow), first.Spec)
	assert.Len(t, first.Ops, 3)
	assert.InDelta(t, 73, first.Ops[opSend].P50us.Median, 0, "the figures carry over")
	assert.Equal(t, 17524, first.Ops[opSend].Count)
	assert.Contains(t, first.Counters, counterEmptyReceives)
	assert.Equal(t, 200, gostack.Agentic[serviceSQS].Cycles, "the one agentic result belongs to sqs")
}

// The ids a schema 2 file converts to are the ids the matrix emits now, so a new
// run compares with an old file cell for cell.
func Test_schema_2_ids_are_the_ids_of_the_matrix(t *testing.T) {
	rep, err := loadReport(committedV2)
	require.NoError(t, err)

	gostack, ok := rep.target(targetGostack)
	require.True(t, ok)

	for _, profile := range []string{profileFull, profileQuick} {
		for _, c := range cellsFor(profile, target{persistable: true}) {
			_, found := gostack.cell(c.id())
			assert.True(t, found, "%s is not a cell of the converted result", c.id())
		}
	}

	localstack, ok := rep.target(targetLocalstack)
	require.True(t, ok)
	assert.Len(t, cellsFor(profileFull, target{}), len(localstack.Cells), "a target that does not persist")
}

func Test_a_converted_result_compares_cleanly_with_itself(t *testing.T) {
	var out bytes.Buffer

	err := compareFiles(t.Context(), committedV2, "", committedV2, &out)
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Compared 432 cells")
	assert.Contains(t, out.String(), "### Regressions (0)")
	assert.Contains(t, out.String(), "### Improvements (0)")
	assert.NotContains(t, out.String(), "### Not compared")
}

// genericTarget reads a target's cells as bare maps, to see which keys they have.
type genericTarget struct {
	Cells []map[string]any `json:"cells"`
}

func Test_loadReport_reads_a_schema_3_file_back_and_refuses_a_newer_one(t *testing.T) {
	dir := t.TempDir()

	written := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	written.Targets[0].Agentic = map[string]agenticRun{serviceSQS: {Cycles: 3}}

	base, err := writeReport(dir, written)
	require.NoError(t, err)

	read, err := loadReport(base + ".json")
	require.NoError(t, err)
	assert.Equal(t, written, read)

	raw, err := os.ReadFile(base + ".json")
	require.NoError(t, err)

	var generic struct {
		Targets []genericTarget `json:"targets"`
	}

	require.NoError(t, json.Unmarshal(raw, &generic))

	cell := generic.Targets[0].Cells[0]
	assert.Contains(t, cell, "ops")

	for _, name := range []string{"send", "receive", "delete"} {
		assert.NotContains(t, cell, name, "an operation is a key of ops, not a field of the cell")
	}

	future := filepath.Join(dir, "future.json")
	require.NoError(t, os.WriteFile(future, []byte(`{"schema": 4}`), fileMode))

	_, err = loadReport(future)
	require.ErrorIs(t, err, errNewerSchema)
}

// fakeCell is a cell of a service the registry does not know: nothing in the
// report or the compare may depend on the operations being send, receive and
// delete.
func fakeCell(put float64) cellResult {
	op := func(p50 float64) opSpread { return opSpread{Count: 1000, P50us: around(p50), P99us: around(p50 * 4)} }

	return cellResult{
		ID: fakeID, Spec: cellSpec{Service: "kv", Workload: "objects", Concurrency: levelMid}, Runs: 3,
		Ops:          map[string]opSpread{"put": op(put), "get": op(300), "list": op(500)},
		CyclesPerSec: around(1000), PeakRSSBytes: around(40 * bytesPerMiB),
	}
}

const fakeID = "kv/objects/persist=off/payload=1KiB/c=16"

func Test_compare_and_report_take_the_operations_from_the_cell(t *testing.T) {
	base := testReport(hostA, fakeCell(800))
	head := testReport(hostA, fakeCell(1200)) // put p50 +50%

	out, err := compared(t, base, head)

	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "put p50")
	assert.Contains(t, out, "### Regressions (2)", "put p50 and put p99")
	assert.NotContains(t, out, "get p50 ")
	assert.NotContains(t, out, "send")

	// An operation the head dropped is not a regression and not an error.
	dropped := fakeCell(800)
	delete(dropped.Ops, "list")

	_, err = compared(t, base, testReport(hostA, dropped))
	require.NoError(t, err)

	md := testReport(hostA, fakeCell(800)).markdown()
	assert.Contains(t, md, "### kv/objects")
	assert.Contains(t, md, "| Cell | get | list | put | Cycles/s | Peak RSS |", "name order, for an unknown workload")
	assert.NotContains(t, md, "Send")

	slow := fakeCell(800)
	slow.Ops["get"] = opSpread{Count: 1, P50us: around(300), P99us: around(60_000)}

	assert.Contains(t, testReport(hostA, slow).markdown(), "get p99 60.0 ms")
}

func Test_report_shows_the_operations_of_a_known_workload_in_cycle_order(t *testing.T) {
	md := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB)).markdown()

	assert.Contains(t, md, "### sqs/messaging")
	assert.Contains(t, md, "| Cell | send | receive | delete | Cycles/s | Peak RSS |")
}

func Test_summarizeCell_folds_the_operations_and_counters_of_its_runs(t *testing.T) {
	run := func(put, empty int) runResult {
		return runResult{
			Cycles: 10, CyclesPerSec: 5,
			Ops:      map[string]opStats{"put": {Count: 10, P50us: int64(put), P99us: int64(put) * 2}, "get": {Count: 10}},
			Counters: map[string]int{"misses": empty},
		}
	}

	cell := summarizeCell(cellSpec{Service: "kv", Workload: "objects"}, []runResult{run(100, 1), run(300, 2), run(200, 3)})

	assert.Equal(t, 3, cell.Runs)
	assert.Equal(t, 30, cell.Ops["put"].Count)
	assert.Equal(t, spread{Min: 100, Median: 200, Max: 300}, cell.Ops["put"].P50us)
	assert.Contains(t, cell.Ops, "get")
	assert.Equal(t, map[string]int{"misses": 6}, cell.Counters)
}

func Test_compareFiles_names_the_fingerprint_it_looked_for(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(t.TempDir())

	head := testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	headPath := filepath.Join(t.TempDir(), "head.json")

	raw, err := jsonMarshal(head)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(headPath, raw, fileMode))

	var out bytes.Buffer

	require.NoError(t, compareFiles(t.Context(), "", dir, headPath, &out))
	assert.Contains(t, out.String(), "has the fingerprint of this host")
	assert.Contains(t, out.String(), hostA.String())
}
