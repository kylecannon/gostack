package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s3Cell is a cell of the objects workload with the figures a flag reads.
func s3Cell(spec cellSpec, putP50, rate, baseline, peak float64) cellResult {
	return cellResult{
		ID: spec.id(), Spec: spec, Runs: 3,
		Ops: map[string]opSpread{
			opPutObject: {Count: 100, P50us: around(putP50), P99us: around(putP50 * 4)},
			opGetObject: {Count: 100, P50us: around(100), P99us: around(400)},
		},
		CyclesPerSec: around(rate), BaselineRSSBytes: around(baseline), PeakRSSBytes: around(peak),
	}
}

func flagged(t *testing.T, targets ...targetResult) string {
	t.Helper()

	var b strings.Builder

	report{Targets: targets}.flags(&b)

	return b.String()
}

func Test_flags_name_a_tenfold_slowdown_and_not_a_smaller_gap(t *testing.T) {
	slow := objectsCell(s3Payload64KiB, "", levelMid)
	near := objectsCell(s3Payload1MiB, "", levelMid)
	persisting := objectsCell(s3Payload64KiB, persistEnvS3, levelMid)

	// LocalStack holds put at 2 ms. gostack is 30 times slower on the first cell,
	// 3 times slower on the second, and the third is a persistence-on cell.
	gostack := targetResult{Name: targetGostack, Cells: []cellResult{
		s3Cell(slow, 60_000, 100, 40*bytesPerMiB, 50*bytesPerMiB),
		s3Cell(near, 6_000, 100, 40*bytesPerMiB, 50*bytesPerMiB),
		s3Cell(persisting, 60_000, 100, 40*bytesPerMiB, 50*bytesPerMiB),
	}}
	localstack := targetResult{Name: targetLocalstack, Cells: []cellResult{
		s3Cell(slow, 2_000, 100, 200*bytesPerMiB, 250*bytesPerMiB),
		s3Cell(near, 2_000, 100, 200*bytesPerMiB, 250*bytesPerMiB),
	}}

	out := flagged(t, gostack, localstack)

	assert.Contains(t, out, "- gostack `"+slow.id()+"`: put p50 60.00 ms against 2.00")
	assert.Contains(t, out, "- gostack `"+persisting.id()+"`: put p50 60.00 ms against 2.00",
		"a persistence-on cell is held against the LocalStack cell without persistence")
	assert.NotContains(t, out, near.id(), "3 times slower is a table row and not a flag")

	// Throughput alone also counts, at a tenth.
	gostack.Cells = []cellResult{s3Cell(slow, 2_000, 5, 40*bytesPerMiB, 50*bytesPerMiB)}
	localstack.Cells = []cellResult{s3Cell(slow, 2_000, 100, 200*bytesPerMiB, 250*bytesPerMiB)}

	assert.Contains(t, flagged(t, gostack, localstack), "5.00 cycles/s against 100.00")

	// A microsecond-scale operation never reaches the 1 ms floor.
	gostack.Cells = []cellResult{s3Cell(slow, 900, 100, 40*bytesPerMiB, 50*bytesPerMiB)}
	localstack.Cells = []cellResult{s3Cell(slow, 50, 100, 200*bytesPerMiB, 250*bytesPerMiB)}

	assert.NotContains(t, flagged(t, gostack, localstack), slow.id())
}

func Test_flags_name_memory_that_grows_with_the_payload(t *testing.T) {
	small, large := objectsCell(s3Payload1KiB, "", levelMid), objectsCell(s3Payload16MiB, "", levelMid)

	streaming := targetResult{Name: targetGostack, Cells: []cellResult{
		s3Cell(small, 500, 100, 5*bytesPerMiB, 40*bytesPerMiB),
		s3Cell(large, 500, 100, 5*bytesPerMiB, 46*bytesPerMiB),
	}}

	assert.NotContains(t, flagged(t, streaming), large.id(), "6 MiB over the small cell is warming up")

	// 16 workers each holding a 16 MiB object: 256 MiB in flight, 300 MiB gained.
	buffering := targetResult{Name: targetGostack, Cells: []cellResult{
		s3Cell(small, 500, 100, 5*bytesPerMiB, 40*bytesPerMiB),
		s3Cell(large, 500, 100, 5*bytesPerMiB, 340*bytesPerMiB),
	}}

	out := flagged(t, buffering)
	assert.Contains(t, out, "- gostack `"+large.id()+"`: peak RSS 340.0 MiB, 300.0 MiB above the 1KiB cell "+
		"(256.0 MiB in flight)")
	assert.NotContains(t, out, small.id())

	// One size, as the multipart cell is: flagged when it rises by the object.
	upload := multipartCell("", levelLow)
	uploaded := func(peak float64) targetResult {
		return targetResult{Name: targetGostack, Cells: []cellResult{s3Cell(upload, 500, 1, 5*bytesPerMiB, peak*bytesPerMiB)}}
	}
	held, streamed := uploaded(75), uploaded(45)

	assert.Contains(t, flagged(t, held), "- gostack `"+upload.id()+"`: peak RSS 75.0 MiB, 70.0 MiB above the start")
	assert.NotContains(t, flagged(t, streamed), upload.id())
}

// A result of one service must not hide the cells another service has in an
// older result: each cell of the base comes from the newest result that has it.
func Test_the_base_takes_each_cell_from_the_newest_result_that_has_it(t *testing.T) {
	dir, head := t.TempDir(), testReport(hostA, testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB))
	t.Chdir(t.TempDir())

	put := func(name, when string, cells ...cellResult) {
		rep := testReport(hostA, cells...)
		rep.Time, rep.Commit = when, name
		raw, err := jsonMarshal(rep)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), raw, fileMode))
	}

	s3 := s3Cell(objectsCell(s3Payload64KiB, "", levelMid), 500, 100, 5*bytesPerMiB, 40*bytesPerMiB)
	staleSQS := testCell(cellID, 900, 10000, 2000, 50*bytesPerMiB)
	newSQS := testCell(cellID, 800, 10000, 2700, 50*bytesPerMiB)

	put("sqs-old", "2026-11-10T10:00:00Z", staleSQS)
	put("sqs-new", "2026-11-12T10:00:00Z", newSQS)
	put("s3-only", "2026-11-18T10:00:00Z", s3)

	base, found, err := newestComparable(t.Context(), dir, filepath.Join(t.TempDir(), "head.json"), head)

	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "s3-only", base.Commit, "the base is the newest result")

	cells := base.Targets[0].Cells
	require.Len(t, cells, 2, "the S3 cells of the newest result and the SQS cell of an older one")

	got, ok := base.Targets[0].cell(cellID)
	require.True(t, ok)
	assert.InDelta(t, 2700, got.CyclesPerSec.Median, 0, "the newer of the two SQS results")

	_, ok = base.Targets[0].cell(s3.ID)
	assert.True(t, ok)

	// A head of SQS alone is held against the SQS cell, not against nothing.
	out, err := compared(t, base, head)
	require.NoError(t, err)
	assert.Contains(t, out, "Compared 1 cells")
}

func Test_notes_of_the_run_are_in_the_report(t *testing.T) {
	opts, err := parseOptions([]string{"-note", "load average 3.1", "-note", "PERSIST_S3 is not wired"})
	require.NoError(t, err)
	assert.Equal(t, []string{"load average 3.1", "PERSIST_S3 is not wired"}, opts.notes)

	rep := testReport(hostA)
	assert.NotContains(t, rep.markdown(), "## Notes")

	rep.Settings.Notes = opts.notes
	assert.Contains(t, rep.markdown(), "## Notes\n\n- load average 3.1\n- PERSIST_S3 is not wired\n")
}

func Test_flags_name_idle_cpu_over_the_limit(t *testing.T) {
	quiet := targetResult{Name: targetGostack, Idle: idleResult{
		Healthcheck: idleVariant{Seconds: 30, CPUPercent: around(0.2)},
	}}
	busy := targetResult{Name: targetSlim, Idle: idleResult{
		Healthcheck:   idleVariant{Seconds: 30, CPUPercent: around(0.2)},
		NoHealthcheck: idleVariant{Seconds: 30, CPUPercent: around(2.5)},
	}}
	reference := targetResult{Name: targetLocalstack, Idle: idleResult{Healthcheck: idleVariant{CPUPercent: around(9)}}}

	out := flagged(t, quiet, busy, reference)

	assert.Contains(t, out, "- gostack-slim without the HEALTHCHECK: 2.50% of one core over 30 s")
	assert.NotContains(t, out, "- gostack with")
	assert.NotContains(t, out, "- localstack", "LocalStack is the reference and is never flagged")
}
