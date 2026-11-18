package main

import (
	"bytes"
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
			got, err := findRegressions(strings.NewReader(tt.give))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			var out bytes.Buffer

			err = gateMain(strings.NewReader(tt.give), &out)
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
	opts, err := parseOptions([]string{"-only", "gostack,localstack", "-concurrency", "1, 8"})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 8}, opts.concurrencies)
	require.Len(t, opts.targets, 2)
	assert.Equal(t, "localstack", opts.targets[1].name)

	_, err = parseOptions([]string{"-only", "nothing"})
	require.Error(t, err)

	_, err = parseOptions([]string{"-concurrency", "0"})
	require.Error(t, err)
}
