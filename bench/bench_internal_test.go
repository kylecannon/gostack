package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// benchstatSlower is benchstat's output for a benchmark that got 20% slower and
// whose throughput dropped with it, next to one that did not change.
const benchstatSlower = `goos: linux
goarch: amd64
pkg: example.com/x
cpu: Fake CPU
        │   old.txt   │               new.txt               │
        │   sec/op    │   sec/op     vs base                │
Fast-8    101.0n ± 1%   101.0n ± 1%        ~ (p=1.000 n=10)
Slow-8    505.0n ± 1%   606.0n ± 1%  +20.00% (p=0.000 n=10)
geomean   225.8n        247.4n        +9.54%

        │  old.txt   │               new.txt               │
        │    B/op    │    B/op     vs base                 │
Fast-8    64.00 ± 0%   64.00 ± 0%       ~ (p=1.000 n=10) ¹
geomean   64.00        64.00       +0.00%
¹ all samples are equal

       │   old.txt    │               new.txt                │
       │     B/s      │     B/s       vs base                │
Slow-8   94.42Mi ± 1%   78.69Mi ± 1%  -16.66% (p=0.000 n=10)
`

const benchstatBelowLimit = `        │   old.txt   │              new2.txt              │
        │   sec/op    │   sec/op     vs base               │
Slow-8    505.0n ± 1%   530.3n ± 1%  +5.01% (p=0.000 n=10)
Noisy-8   505.0n ± 9%   590.3n ± 9%  +16.9% (p=0.200 n=10)
Faster-8  505.0n ± 1%   400.0n ± 1%  -20.79% (p=0.000 n=10)
geomean   225.8n        231.4n       +2.47%
`

func Test_gate_regressions(t *testing.T) {
	tests := []struct {
		name      string
		give      string
		want      []string
		wantError bool
	}{
		{
			name: "a cost that rose over 10% and a throughput that fell over 10% fail",
			give: benchstatSlower,
			want: []string{"Slow-8 sec/op +20.00% (p=0.000)", "Slow-8 B/s -16.66% (p=0.000)"},
		},
		{
			name: "a rise under 10%, an insignificant rise and an improvement pass",
			give: benchstatBelowLimit,
			want: nil,
		},
		{
			name: "no comparison rows pass",
			give: "goos: linux\n",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := findRegressions(strings.NewReader(tt.give))
			require.NoError(t, err)

			var got []string
			for _, r := range rows {
				got = append(got, r.String())
			}

			assert.Equal(t, tt.want, got)

			var out bytes.Buffer

			err = gateMain(t.Context(), nil, strings.NewReader(tt.give), &out, nil)
			if len(tt.want) == 0 {
				require.NoError(t, err)
				assert.Contains(t, out.String(), "no benchmark regressed")

				return
			}

			require.ErrorIs(t, err, errRegression)
			assert.Contains(t, out.String(), tt.want[0])
		})
	}
}

// The gate flags a row only when benchstat prints p=0.000: p=0.023 over 10% is
// the shape of the false flags on PRs #6 and #7, and p=0.001 sits on the
// boundary, which significance does not reach.
func Test_gate_requires_p_of_zero(t *testing.T) {
	const (
		header = "pkg: example.com/x\n        │   old.txt   │      new.txt       │\n" +
			"        │   sec/op    │   sec/op   vs base │\n"
		costRow = "Slow-8    505.0n ± 1%   563.0n ± 1%  +11.28% (p=PVAL n=10)\n"
	)

	costAt := func(p string) string { return strings.Replace(costRow, "PVAL", p, 1) }

	tests := []struct {
		name string
		give string
		want []string
	}{
		{name: "over 10% at p=0.023 passes", give: costAt("0.023")},
		{name: "over 10% at p=0.049 passes", give: costAt("0.049")},
		{name: "over 10% at p=0.001 passes", give: costAt("0.001")},
		{
			name: "over 10% at p=0.000 fails", give: costAt("0.000"),
			want: []string{"Slow-8 sec/op +11.28% (p=0.000)"},
		},
		{
			name: "a throughput drop over 10% at p=0.023 passes",
			give: "        │   old.txt   │      new.txt       │\n        │     B/s     │    B/s     vs base │\n" +
				"Slow-8   506.0Mi ± 14%   455.0Mi ± 17%  -10.09% (p=0.023 n=10)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := findRegressions(strings.NewReader(header + tt.give))
			require.NoError(t, err)

			var got []string
			for _, r := range rows {
				got = append(got, r.String())
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

// Replays of CI's benchstat output, cut to the rows over 10% either way
// (testdata/). The two false flags pass, and the real regression PR #4 landed
// still fails.
func Test_gate_replays_ci_runs(t *testing.T) {
	const none = "# nothing accepted\n"

	tests := []struct {
		name     string
		file     string
		wantText string
		wantRow  string
	}{
		{
			name: "PR #7, identical code on both sides, ReceiveMessage 1MiB at p=0.023", file: "pr7-run-44524892109.txt",
			wantText: "p=0.023",
		},
		{
			name: "PR #6, Snapshot lock-ms at p=0.018", file: "pr6-run-44517842228.txt",
			wantText: "p=0.018",
		},
		{
			name: "PR #4, RouteDetect/host at p=0.000", file: "pr4-run-44470444180.txt",
			wantText: "p=0.000", wantRow: "RouteDetect/host-2 sec/op +26.83% (p=0.000)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join("testdata", tt.file))
			require.NoError(t, err)
			require.Contains(t, string(text), tt.wantText, "the replay carries the row it is about")

			out, err := runGateOn(t, string(text), none, nil, commitFlags(baseCommit, headCommit)...)
			if tt.wantRow == "" {
				require.NoError(t, err)
				assert.Contains(t, out, "no benchmark regressed")

				return
			}

			require.ErrorIs(t, err, errRegression)
			assert.Contains(t, out, tt.wantRow)
			assert.Contains(t, err.Error(), "1 over the limit", "the improvements on the same run do not count")
		})
	}
}

func Test_gate_refuses_empty_input(t *testing.T) {
	for name, give := range map[string]string{"empty": "", "blank": " \n\t\n"} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer

			err := gateMain(t.Context(), nil, strings.NewReader(give), &out, nil)
			require.ErrorIs(t, err, errEmptyInput)
			assert.Empty(t, out.String())
		})
	}
}

func Test_percentile(t *testing.T) {
	sorted := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		sorted = append(sorted, time.Duration(i)*time.Millisecond)
	}

	tests := []struct {
		name string
		give []time.Duration
		pct  int
		want time.Duration
	}{
		{name: "p50 of 100 is the 50th", give: sorted, pct: 50, want: 50 * time.Millisecond},
		{name: "p99 of 100 is the 99th", give: sorted, pct: 99, want: 99 * time.Millisecond},
		{name: "one sample is every percentile", give: sorted[:1], pct: 99, want: time.Millisecond},
		{name: "p99 of 10 is the largest", give: sorted[:10], pct: 99, want: 10 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, percentile(tt.give, tt.pct))
		})
	}
}

func Test_median(t *testing.T) {
	assert.InDelta(t, 3.0, median([]float64{5, 1, 3, 2, 4}), 0)
	assert.InDelta(t, 2.5, median([]float64{4, 1, 3, 2}), 0)
	assert.InDelta(t, 0.0, median(nil), 0)
}

func Test_containerStats_rss(t *testing.T) {
	v2 := containerStats{MemoryStats: memoryStats{
		Usage: 900, Stats: map[string]uint64{"anon": 100, "file_mapped": 20, "file": 700},
	}}
	v1 := containerStats{MemoryStats: memoryStats{Usage: 900, Stats: map[string]uint64{"rss": 50, "mapped_file": 5}}}
	bare := containerStats{MemoryStats: memoryStats{Usage: 900}}

	assert.Equal(t, uint64(120), v2.rss())
	assert.Equal(t, uint64(55), v1.rss())
	assert.Equal(t, uint64(900), bare.rss())
}

func Test_parseOptions(t *testing.T) {
	opts, err := parseOptions([]string{"-targets", "gostack,localstack"})
	require.NoError(t, err)
	require.Len(t, opts.targets, 2)
	assert.Equal(t, "localstack", opts.targets[1].name)
	assert.False(t, opts.targets[1].persistable)
	assert.Equal(t, profileFull, opts.profile)
	assert.Equal(t, fullIdle, opts.idle)

	quick, err := parseOptions([]string{"-profile", "quick"})
	require.NoError(t, err)
	require.Len(t, quick.targets, 1, "the quick profile measures gostack alone")
	assert.Equal(t, "gostack", quick.targets[0].name)
	assert.Equal(t, quickIdle, quick.idle)

	all, err := parseOptions(nil)
	require.NoError(t, err)
	assert.Len(t, all.targets, 3)

	_, err = parseOptions([]string{"-targets", "nothing"})
	require.Error(t, err)

	_, err = parseOptions([]string{"-profile", "huge"})
	require.Error(t, err)

	_, err = parseOptions([]string{"-reps", "0"})
	require.Error(t, err)
}

func Test_parseOptions_base(t *testing.T) {
	const (
		baseImage  = "base-image"
		baseCommit = "base-commit"
		baseOut    = "base-out"
	)

	// args turns the settings of a two-sided quick run, changed by edit, into flags.
	args := func(edit func(map[string]string)) []string {
		set := map[string]string{
			"profile": profileQuick, "gostack-image": "gostack:head", "out": "out/head",
			baseImage: "gostack:base", baseCommit: "1234abc", baseOut: "out/base",
		}
		edit(set)

		out := make([]string, 0, 2*len(set))
		for _, name := range slices.Sorted(maps.Keys(set)) {
			out = append(out, "-"+name, set[name])
		}

		return out
	}

	opts, err := parseOptions(args(func(map[string]string) {}))
	require.NoError(t, err)

	sides := opts.sides(opts.targets[0])
	require.Len(t, sides, 2)
	assert.Equal(t, target{name: targetGostack, image: "gostack:base", persistable: true, side: sideBase}, sides[0])
	assert.Equal(t, target{name: targetGostack, image: "gostack:head", persistable: true, side: sideHead}, sides[1])
	assert.Equal(t, "gostack-base", sides[0].label(), "the two sides' containers are told apart")

	plain, err := parseOptions(args(func(set map[string]string) {
		delete(set, baseImage)
		delete(set, baseCommit)
		delete(set, baseOut)
	}))
	require.NoError(t, err)
	assert.Equal(t, []target{plain.targets[0]}, plain.sides(plain.targets[0]), "a run without -base-image has one side")
	assert.Equal(t, targetGostack, plain.targets[0].label())

	for name, edit := range map[string]func(map[string]string){
		"two targets":      func(set map[string]string) { set["targets"] = "gostack,gostack-slim" },
		"no base commit":   func(set map[string]string) { delete(set, baseCommit) },
		"no base out":      func(set map[string]string) { delete(set, baseOut) },
		"same out":         func(set map[string]string) { set[baseOut] = "out/head/" },
		"commit, no image": func(set map[string]string) { delete(set, baseImage); delete(set, baseOut) },
		"out, no image":    func(set map[string]string) { delete(set, baseImage); delete(set, baseCommit) },
	} {
		_, err := parseOptions(args(edit))
		assert.Error(t, err, name)
	}
}

func Test_roundOrder(t *testing.T) {
	var got [][]int

	alternate(4, 2, func(round, side int) { got = append(got, []int{round, side}) })
	assert.Equal(t, [][]int{{0, 0}, {0, 1}, {1, 1}, {1, 0}, {2, 0}, {2, 1}, {3, 1}, {3, 0}}, got,
		"two sides go base head, head base, ...")

	got = nil

	alternate(3, 1, func(round, side int) { got = append(got, []int{round, side}) })
	assert.Equal(t, [][]int{{0, 0}, {1, 0}, {2, 0}}, got, "one side runs as before")
	assert.Equal(t, []int{2, 1, 0}, roundOrder(1, 3))
}

func Test_cells_profiles(t *testing.T) {
	full := cellsFor(profileFull, target{persistable: true}, serviceSQS)
	assert.Len(t, full, 216, "3 payloads x 2 queues x 3 shapes x 2 protocols x 3 concurrencies x 2 persistence")

	ids := map[string]bool{}
	for _, c := range full {
		assert.False(t, ids[c.id()], c.id())
		ids[c.id()] = true
	}

	quick := cellsFor(profileQuick, target{persistable: true}, serviceSQS)
	assert.Len(t, quick, 10)

	for _, c := range quick {
		assert.True(t, ids[c.id()], "%s is not a cell of the full profile", c.id())
	}

	assert.Len(t, cellsFor(profileFull, target{}, serviceSQS), 108,
		"a target that does not persist skips the persistence cells")
	assert.Len(t, cellsFor(profileQuick, target{}, serviceSQS), 9)
	assert.Equal(t, "sqs/messaging/persist=off/payload=64KiB/queue=fifo/shape=batch/proto=query/c=64",
		sqsCellSpec(payloadMedium, queueFIFO, shapeBatch, protoQuery, "", levelHigh).id())
	assert.Equal(t, "sqs/messaging/persist=on/payload=256B/queue=standard/shape=single/proto=json/c=1",
		sqsCellSpec(payloadSmall, queueStandard, shapeSingle, protoJSON, persistEnvSQS, levelLow).id())
	assert.Equal(t, 104857, sqsCell{Payload: payloadMax, Shape: shapeBatch}.entryBytes(),
		"ten entries fit the batch limit")
	assert.Equal(t, payloadMax, sqsCell{Payload: payloadMax, Shape: shapeSingle}.entryBytes())
}

func Test_cells_a_persistence_cell_names_its_variable(t *testing.T) {
	for _, c := range cellsFor(profileFull, target{persistable: true}, serviceSQS) {
		if c.persists() {
			assert.Equal(t, "PERSIST_SQS", c.PersistEnv, c.id())
			assert.Contains(t, c.id(), "persist=on")
		} else {
			assert.Empty(t, c.PersistEnv, c.id())
		}
	}
}

func Test_workloads_are_registered_once_and_agree_with_their_cells(t *testing.T) {
	seen := map[string]bool{}

	for _, w := range workloads {
		key := w.Service() + "/" + w.Name()
		assert.False(t, seen[key], "%s is registered twice", key)

		seen[key] = true

		assert.NotEmpty(t, w.Ops(), key)

		found, err := findWorkload(w.Service(), w.Name())
		require.NoError(t, err)
		assert.Equal(t, key, found.Service()+"/"+found.Name())

		for _, profile := range []string{profileFull, profileQuick} {
			for _, c := range w.Cells(profile) {
				assert.Equal(t, w.Service(), c.Service, c.id())
				assert.Equal(t, w.Name(), c.Workload, c.id())
				assert.True(t, strings.HasPrefix(c.id(), key+"/"), c.id())
			}
		}
	}

	_, err := findWorkload("kv", "objects")
	require.ErrorIs(t, err, errNoWorkload)

	_, err = findAgentic("kv")
	require.ErrorIs(t, err, errNoWorkload)
}

func Test_sqsCellOf_refuses_an_axis_value_it_has_no_cycle_for(t *testing.T) {
	good := sqsCellSpec(payloadSmall, queueFIFO, shapeBatch, protoQuery, "", levelMid)

	spec, err := sqsCellOf(good)
	require.NoError(t, err)
	assert.Equal(t,
		sqsCell{Payload: payloadSmall, Queue: queueFIFO, Shape: shapeBatch, Proto: protoQuery, Concurrency: levelMid}, spec)

	for _, key := range []string{optQueue, optShape, optProto} {
		bad := good
		bad.Options = []option{{Key: key, Value: "nope"}}

		_, err = sqsCellOf(bad)
		require.ErrorIs(t, err, errBadAxis, key)
	}

	_, err = sqsCellOf(cellSpec{Payload: payloadSmall, Concurrency: levelMid})
	require.ErrorIs(t, err, errBadAxis, "a cell with no axes")
}

func Test_parseCell_reads_the_cell_from_the_flags(t *testing.T) {
	const opt = "-opt"

	req, err := parseCell([]string{
		"-service", "sqs", "-workload", "messaging", "-payload", "65536", "-concurrency", "64",
		opt, "queue=fifo", opt, "shape=batch", opt, "proto=query", "-duration", "3s", endpointFlag, "http://x:1",
	})
	require.NoError(t, err)

	assert.Equal(t, sqsCellSpec(payloadMedium, queueFIFO, shapeBatch, protoQuery, "", levelHigh), req.cell)
	assert.Equal(t, "http://x:1", req.endpoint)
	assert.Equal(t, 3*time.Second, req.duration)

	// The flags the orchestrator writes for a cell read back as the same cell.
	again, err := parseCell(req.cell.loadgenArgs())
	require.NoError(t, err)
	assert.Equal(t, req.cell, again.cell)

	_, err = parseCell([]string{opt, "queue"})
	require.Error(t, err)
}
