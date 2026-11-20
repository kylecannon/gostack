package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const (
	acceptCommit = "8fb22fa3d73e976a0225b14a39cfb276529d06aa"
	baseCommit   = "1cf05e19432a6689a7ec4cc04123530adb3a4d10"
	headCommit   = "563067b9990063b79d38d4f7ad445ca93468d9ef"

	routePkg  = "internal/route"
	unitSecOp = "sec/op"
	unitMiB   = "MiB"

	unitCyclesPerSec = "cycles/s"
	bound1000        = "+1000%"
	bound100         = "+100%"
	fallPastFlag     = "-10.01"
	unitBPerSec      = "B/s"
)

// errNoObject stands in for git failing to find a commit.
var errNoObject = errors.New("no such commit")

// history fakes git: the head holds acceptCommit, and the base holds it only
// when expired is set.
func history(expired bool) containsFunc {
	return func(_ context.Context, ancestor, descendant string) (bool, error) {
		switch descendant {
		case headCommit:
			return ancestor == acceptCommit, nil
		case baseCommit:
			return expired && ancestor == acceptCommit, nil
		default:
			return false, errNoObject
		}
	}
}

// benchstatRoute is CI's benchstat output for PR #4, cut to the route package.
const benchstatRoute = `goos: linux
goarch: amd64
pkg: github.com/kylecannon/gostack/internal/route
cpu: AMD EPYC 7763 64-Core Processor
                                      │   base.txt    │               head.txt               │
                                      │    sec/op     │    sec/op     vs base                │
RouteDetect/signing_name_and_target-2   240.7n ± 0%   258.6n ± 1%   +7.39% (p=0.000 n=10)
RouteDetect/host-2                      113.0n ± 3%   163.0n ± 1%  +44.18% (p=0.000 n=10)
geomean                                 164.9n        205.3n       +24.47%
`

func benchstatEntry(pkg, benchmark, unit, bound string) string {
	return "- gate: benchstat\n  package: " + pkg + "\n  benchmark: " + benchmark + "\n  unit: " + unit +
		"\n  max: \"" + bound + "\"\n  reason: S3 host checks\n  pr: 4\n  commit: " + acceptCommit + "\n"
}

// commitFlags is the gate's flags for the compared commits.
func commitFlags(base, head string) []string {
	return []string{"-base", base, "-head", head}
}

func runGate(t *testing.T, list string, contains containsFunc, args ...string) (string, error) {
	t.Helper()

	return runGateOn(t, benchstatRoute, list, contains, args...)
}

// runGateOn is runGate on another benchstat output.
func runGateOn(t *testing.T, benchstat, list string, contains containsFunc, args ...string) (string, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "accepted.yaml")
	require.NoError(t, os.WriteFile(path, []byte(list), 0o600))

	var out bytes.Buffer

	args = append([]string{"-accepted", path}, args...)
	err := gateMain(t.Context(), args, strings.NewReader(benchstat), &out, contains)

	return out.String(), err
}

func Test_gate_accepted_list(t *testing.T) {
	routeHost := benchstatEntry("internal/route", "RouteDetect/host", "sec/op", "+60%")
	commits := commitFlags(baseCommit, headCommit)

	tests := []struct {
		name     string
		list     string
		contains containsFunc
		args     []string
		wantNote string
	}{
		{name: "listed past its bound fails", list: benchstatEntry("internal/route", "RouteDetect/host", "sec/op",
			"+40%"), contains: history(false), args: commits, wantNote: "past the accepted 40%"},
		{
			name: "unlisted fails", list: benchstatEntry("internal/route", "RouteDetect/other", "sec/op", "+60%"),
			contains: history(false), args: commits,
		},
		{
			name: "another unit fails", list: benchstatEntry("internal/route", "RouteDetect/host", "B/op", "+60%"),
			contains: history(false), args: commits,
		},
		{name: "another package fails", list: benchstatEntry("internal/gateway", "RouteDetect/host", "sec/op",
			"+60%"), contains: history(false), args: commits},
		{
			name: "an expired entry fails", list: routeHost, contains: history(true), args: commits,
			wantNote: "which the base 1cf05e1 already holds, so the entry has expired",
		},
		{
			name: "a head without the commit fails", list: routeHost, contains: history(false),
			args: commitFlags(baseCommit, baseCommit), wantNote: "the head 1cf05e1 does not hold",
		},
		{
			name: "a commit git cannot find fails", list: routeHost, contains: history(false),
			args: commitFlags("abcdef0", headCommit), wantNote: "git cannot tell",
		},
		{name: "unknown commits fail", list: routeHost, contains: history(false), wantNote: "is not a commit hash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runGate(t, tt.list, tt.contains, tt.args...)
			require.ErrorIs(t, err, errRegression)
			assert.Contains(t, out, "RouteDetect/host-2 sec/op +44.18% (p=0.000)")
			assert.NotContains(t, out, "Accepted (")
			assert.Contains(t, out, tt.wantNote)
		})
	}

	t.Run("listed within its bound passes and is shown", func(t *testing.T) {
		out, err := runGate(t, routeHost, history(false), commits...)
		require.NoError(t, err)
		assert.Contains(t, out, "Accepted (1):\n  RouteDetect/host-2 sec/op +44.18% (p=0.000), within 60%: "+
			"S3 host checks (PR #4, 8fb22fa)")
		assert.Contains(t, out, "no benchmark regressed")
	})

	t.Run("an empty list accepts nothing", func(t *testing.T) {
		out, err := runGate(t, "# nothing accepted\n", history(false), commits...)
		require.ErrorIs(t, err, errRegression)
		assert.Contains(t, out, "RouteDetect/host-2 sec/op +44.18%")
	})
}

// idleMax holds the idle RSS rise of idleReports.
const idleMax = "+55%"

const idleEntry = `- gate: system
  target: gostack
  cell: idle
  metric: idle RSS
  max: %s
  reason: S3 tables
  pr: 4
  commit: ` + acceptCommit + "\n"

func systemList(t *testing.T, bound string) acceptedSet {
	t.Helper()

	entries, err := parseAccepted([]byte(strings.Replace(idleEntry, "%s", bound, 1)), "test")
	require.NoError(t, err)

	return acceptedSet{entries: entries}
}

// idleReports are a base and a head whose idle RSS rose 37%, from 3.37 to 4.62
// MiB, as on PR #4, with every run of the head above every run of the base.
func idleReports() (base, head report) {
	base = testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	head = testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	base.Commit, head.Commit = baseCommit, headCommit
	base.Targets[0].Idle.Healthcheck.RSSBytes = around(3.37 * bytesPerMiB)
	head.Targets[0].Idle.Healthcheck.RSSBytes = around(4.62 * bytesPerMiB)

	return base, head
}

func Test_compare_accepted_list(t *testing.T) {
	base, head := idleReports()

	tests := []struct {
		name     string
		max      string
		expired  bool
		head     string
		wantErr  bool
		wantNote string
	}{
		{name: "listed within its percent passes", max: idleMax},
		{name: "listed within its absolute value passes", max: "5 MiB"},
		{name: "listed past its percent fails", max: "+30%", wantErr: true, wantNote: "past the accepted 30%"},
		{
			name: "listed past its absolute value fails", max: "4.5 MiB", wantErr: true,
			wantNote: "the head is past the accepted 4.5 MiB",
		},
		{name: "an absolute value in another unit fails", max: "5 ms", wantErr: true, wantNote: "not in MiB"},
		{name: "an expired entry fails", max: idleMax, expired: true, wantErr: true, wantNote: "has expired"},
		{
			name: "a head without the commit fails", max: idleMax, head: baseCommit, wantErr: true,
			wantNote: "does not hold",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accepted := systemList(t, tt.max)
			accepted.contains = history(tt.expired)

			h := head
			if tt.head != "" {
				h.Commit = tt.head
			}

			var out bytes.Buffer

			err := compareReports(t.Context(), base, h, accepted, &out)
			if !tt.wantErr {
				require.NoError(t, err)
				assert.Contains(t, out.String(), "### Regressions (0)")
				assert.Contains(t, out.String(), "### Accepted (1)")
				assert.Contains(t, out.String(), "| gostack | `idle` | idle RSS |")
				assert.Contains(t, out.String(), "S3 tables (PR #4, 8fb22fa)")

				return
			}

			require.ErrorIs(t, err, errRegression)
			assert.Contains(t, out.String(), "### Regressions (1)")
			assert.Contains(t, out.String(), "### Accepted (0)")
			assert.Contains(t, out.String(), "- gostack `idle` idle RSS: accepted list: ")
			assert.Contains(t, out.String(), tt.wantNote)
		})
	}

	t.Run("unlisted fails", func(t *testing.T) {
		accepted := systemList(t, idleMax)
		accepted.contains = history(false)
		accepted.entries[0].Cell = "startup"

		var out bytes.Buffer

		err := compareReports(t.Context(), base, head, accepted, &out)
		require.ErrorIs(t, err, errRegression)
		assert.Contains(t, out.String(), "### Accepted (0)")
		assert.NotContains(t, out.String(), "accepted list:")
	})

	t.Run("a listed metric that did not regress passes", func(t *testing.T) {
		accepted := systemList(t, idleMax)
		accepted.contains = history(false)

		var out bytes.Buffer

		require.NoError(t, compareReports(t.Context(), base, base, accepted, &out))
		assert.Contains(t, out.String(), "### Accepted (0)")
	})
}

func Test_parseAccepted_refuses(t *testing.T) {
	valid := benchstatEntry("internal/route", "RouteDetect/host", "sec/op", "+60%")

	tests := map[string]string{
		"unknown field":           valid + "  owner: me\n",
		"unknown gate":            strings.Replace(valid, "gate: benchstat", "gate: wall", 1),
		"no reason":               strings.Replace(valid, "  reason: S3 host checks\n", "", 1),
		"reason of two lines":     strings.Replace(valid, "reason: S3 host checks", "reason: \"a\\nb\"", 1),
		"no pr":                   strings.Replace(valid, "  pr: 4\n", "", 1),
		"no commit":               strings.Replace(valid, "  commit: "+acceptCommit+"\n", "", 1),
		"a commit that is a flag": strings.Replace(valid, acceptCommit, "--all", 1),
		"no unit":                 strings.Replace(valid, "  unit: sec/op\n", "", 1),
		"a system field":          valid + "  cell: idle\n",
		"an absolute benchstat":   strings.Replace(valid, "+60%", "160 ns", 1),
		"a percent of zero":       strings.Replace(valid, "+60%", "0%", 1),
		"no max":                  strings.Replace(valid, "  max: \"+60%\"\n", "", 1),
		"system without a cell":   strings.Replace(strings.Replace(idleEntry, "%s", idleMax, 1), "  cell: idle\n", "", 1),
		"a value with no unit":    strings.Replace(idleEntry, "%s", "6", 1),
	}

	for name, give := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseAccepted([]byte(give), "test")
			require.Error(t, err)
		})
	}

	t.Run("an empty file is an empty list", func(t *testing.T) {
		entries, err := parseAccepted([]byte("# none\n"), "test")
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}

// A bound is a finite number above zero, with at most one leading "+". An
// infinite or negative bound would hold every regression, so each is a load
// error and the gate fails on it.
func Test_parseAccepted_bounds(t *testing.T) {
	refused := []string{
		"Inf%", "+Inf%", "-Inf%", "infinity%", "+infinity%", "NaN%", "nan%", "0%", "+0%", "-0%", "0.0%",
		"-60%", "+-+60%", "++60%", "--60%", "%", "+%", "1e999%", "60 %", "six%",
	}

	for _, bound := range refused {
		t.Run("benchstat "+bound, func(t *testing.T) {
			_, err := parseAccepted([]byte(benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, bound)), "test")
			require.ErrorIs(t, err, errAccepted)
			assert.Contains(t, err.Error(), "test entry 1")
		})

		t.Run("system "+bound, func(t *testing.T) {
			_, err := parseAccepted([]byte(strings.Replace(idleEntry, "%s", `"`+bound+`"`, 1)), "test")
			require.ErrorIs(t, err, errAccepted)
		})
	}

	absolute := []string{
		"Inf MiB", "+Inf MiB", "-Inf MiB", "infinity MiB", "NaN MiB", "0 MiB", "-6 MiB", "-0 MiB", "+-6 MiB",
		"++6 MiB", "1e999 MiB", "6 furlongs", "6 mib", "6 %", unitMiB, "6MiB",
	}

	for _, bound := range absolute {
		t.Run("absolute "+bound, func(t *testing.T) {
			_, err := parseAccepted([]byte(strings.Replace(idleEntry, "%s", `"`+bound+`"`, 1)), "test")
			require.ErrorIs(t, err, errAccepted)
			assert.Contains(t, err.Error(), "test entry 1")
		})
	}

	accepted := map[string]limit{
		"60%": {percent: 60}, "+60%": {percent: 60}, "0.5%": {percent: 0.5}, " +60% ": {percent: 60},
		"6 MiB": {absolute: 6, unit: unitMiB}, "+6.5 MiB": {absolute: 6.5, unit: unitMiB},
		"20 ms": {absolute: 20, unit: "ms"}, "2 s": {absolute: 2, unit: "s"},
		"900 cycles/s": {absolute: 900, unit: unitCyclesPerSec},
	}

	for bound, want := range accepted {
		t.Run("accepts "+bound, func(t *testing.T) {
			entries, err := parseAccepted([]byte(strings.Replace(idleEntry, "%s", `"`+bound+`"`, 1)), "test")
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, want, entries[0].limit)
		})
	}

	t.Run("a percent is not an absolute bound for benchstat", func(t *testing.T) {
		_, err := parseAccepted([]byte(benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, "6 MiB")), "test")
		require.ErrorIs(t, err, errAccepted)
	})
}

// A bound of exactly the limit is inside it: the change counts up to and
// including the number written.
func Test_bound_is_inclusive(t *testing.T) {
	entry := benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, "+60%")
	commits := commitFlags(baseCommit, headCommit)
	atLimit := strings.Replace(benchstatRoute, "+44.18%", "+60.00%", 1)
	pastLimit := strings.Replace(benchstatRoute, "+44.18%", "+60.01%", 1)

	out, err := runGateOn(t, atLimit, entry, history(false), commits...)
	require.NoError(t, err)
	assert.Contains(t, out, "RouteDetect/host-2 sec/op +60.00% (p=0.000), within 60%")

	out, err = runGateOn(t, pastLimit, entry, history(false), commits...)
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "past the accepted 60%")

	assert.Empty(t, measured{head: 6, unit: unitMiB, lowerIsBetter: true}.within(limit{absolute: 6, unit: unitMiB}),
		"an absolute bound holds a head equal to it")
	assert.NotEmpty(t, measured{head: 6.01, unit: unitMiB, lowerIsBetter: true}.within(limit{absolute: 6, unit: unitMiB}))
}

// One entry holds one metric. A second for the same metric, whatever its bound
// or commit, is an error, because the gate would otherwise take whichever of
// them accepts the change.
func Test_parseAccepted_duplicates(t *testing.T) {
	route := benchstatEntry
	otherCommit := strings.Replace(route(routePkg, "RouteDetect/host", unitSecOp, "+5%"), acceptCommit,
		"0123456789abcdef0123456789abcdef01234567", 1)
	idle := strings.Replace(idleEntry, "%s", "+55%", 1)

	tests := map[string]string{
		"benchstat, another bound": route(routePkg, "RouteDetect/host", unitSecOp, "+5%") +
			route(routePkg, "RouteDetect/host", unitSecOp, "+60%"),
		"benchstat, another commit": route(routePkg, "RouteDetect/host", unitSecOp, "+60%") + otherCommit,
		"benchstat, the package spelled in full": route(routePkg, "RouteDetect/host", unitSecOp, "+5%") +
			route("github.com/kylecannon/gostack/internal/route", "RouteDetect/host", unitSecOp, "+60%"),
		"system, another bound": idle + strings.Replace(idleEntry, "%s", "6 MiB", 1),
	}

	for name, give := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseAccepted([]byte(give), "test")
			require.ErrorIs(t, err, errAccepted)
			assert.Contains(t, err.Error(), "test entry 2")
			assert.Contains(t, err.Error(), "same metric as entry 1")
		})
	}

	distinct := map[string]string{
		"benchstat, another unit": route(routePkg, "RouteDetect/host", unitSecOp, "+5%") +
			route(routePkg, "RouteDetect/host", "B/op", "+60%"),
		"benchstat, another benchmark": route(routePkg, "RouteDetect/host", unitSecOp, "+5%") +
			route(routePkg, "RouteDetect/other", unitSecOp, "+60%"),
		"benchstat, another package": route(routePkg, "RouteDetect/host", unitSecOp, "+5%") +
			route("internal/gateway", "RouteDetect/host", unitSecOp, "+60%"),
		"benchstat and system": route(routePkg, "RouteDetect/host", unitSecOp, "+5%") + idle,
		"system, another cell": idle + strings.Replace(idle, "cell: idle", "cell: startup", 1),
	}

	for name, give := range distinct {
		t.Run(name, func(t *testing.T) {
			entries, err := parseAccepted([]byte(give), "test")
			require.NoError(t, err)
			assert.Len(t, entries, 2)
		})
	}
}

// The list is one YAML document: entries after a second --- are never read.
func Test_parseAccepted_one_document(t *testing.T) {
	entry := benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, "+60%")
	other := benchstatEntry(routePkg, "RouteDetect/other", unitSecOp, "+60%")

	_, err := parseAccepted([]byte(entry+"---\n"+other), "test")
	require.ErrorIs(t, err, errAccepted)
	assert.Contains(t, err.Error(), "second YAML document")

	_, err = parseAccepted([]byte("---\n"+entry+"---\n# nothing\n"), "test")
	require.ErrorIs(t, err, errAccepted, "a document after --- is a second document, even an empty one")

	_, err = parseAccepted([]byte(entry+"---\n[: not yaml\n"), "test")
	require.ErrorIs(t, err, errAccepted)

	entries, err := parseAccepted([]byte("---\n"+entry), "test")
	require.NoError(t, err, "one leading --- starts the one document")
	assert.Len(t, entries, 1)
}

// An entry names its package by its import path, or by its path below the
// module. Another module nested in this one has the same trailing path.
func Test_benchRow_names_the_package(t *testing.T) {
	require.Equal(t, "github.com/kylecannon/gostack", modulePath(), "the build carries the module path")

	row := func(pkg, name string) benchRow {
		return benchRow{pkg: pkg, name: name, unit: unitSecOp}
	}
	entry := func(pkg string) *acceptedEntry {
		return &acceptedEntry{Gate: gateBenchstat, Package: pkg, Benchmark: "RouteDetect/host", Unit: unitSecOp}
	}

	const (
		root   = "github.com/kylecannon/gostack/internal/route"
		nested = "github.com/kylecannon/gostack/tools/internal/route"
	)

	tests := []struct {
		name  string
		entry string
		pkg   string
		want  bool
	}{
		{name: "the path below the module", entry: routePkg, pkg: root, want: true},
		{name: "the full import path", entry: root, pkg: root, want: true},
		{name: "a nested module's trailing path", entry: routePkg, pkg: nested},
		{name: "the nested module's own full path", entry: nested, pkg: nested, want: true},
		{name: "the nested package by the root's full path", entry: root, pkg: nested},
		{name: "a trailing part that starts below the module root", entry: "route", pkg: root},
		{name: "a part of a path element", entry: "nternal/route", pkg: root},
		{name: "another module", entry: routePkg, pkg: "example.com/other/internal/route"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, row(tt.pkg, "RouteDetect/host-2").matches(entry(tt.entry)))
		})
	}
}

// An entry names a benchmark without its -GOMAXPROCS suffix.
func Test_benchRow_names_the_benchmark(t *testing.T) {
	const pkg = "github.com/kylecannon/gostack/internal/route"

	entry := &acceptedEntry{Gate: gateBenchstat, Package: routePkg, Benchmark: "RouteDetect/host", Unit: unitSecOp}
	row := func(name string) benchRow { return benchRow{pkg: pkg, name: name, unit: unitSecOp} }

	assert.True(t, row("RouteDetect/host-2").matches(entry), "the suffix of a 2-CPU run")
	assert.True(t, row("RouteDetect/host-16").matches(entry), "the suffix of a 16-CPU run")
	assert.True(t, row("RouteDetect/host").matches(entry), "a 1-CPU run prints no suffix")
	assert.False(t, row("RouteDetect/host-1024-2").matches(entry), "a sub-benchmark's own number, then the suffix")
	assert.False(t, row("RouteDetect/hostile-2").matches(entry), "a longer name")
	assert.False(t, row("RouteDetect/host/tls-2").matches(entry), "a sub-benchmark")
	assert.False(t, row("RouteDetect/hos-2").matches(entry), "a shorter name")
	assert.False(t, row("RouteDetect/host-2x").matches(entry), "a suffix that is not a number")

	// benchstat does not print GOMAXPROCS, and a 1-CPU run has no suffix, so the
	// row "host-1024" of a 1-CPU run cannot be told from "host" on 1024 CPUs. The
	// entry takes it. This documents the limit: the name carries nothing to
	// separate them, and a runner with 2 CPUs or more does not hit it.
	assert.True(t, row("RouteDetect/host-1024").matches(entry), "known limit: -1024 reads as a GOMAXPROCS suffix")
}

// The committed list must load: CI reads it on every pull request. It holds no
// entries between a pull request that accepts a regression and the first change
// after its merge, so it need not hold any.
func Test_committed_accepted_list_loads(t *testing.T) {
	_, err := loadAccepted("accepted.yaml")
	require.NoError(t, err)
}

// A list with no entries is valid, and both gates read it as accepting nothing:
// a regression fails, and a run with none passes.
func Test_empty_accepted_list_gates(t *testing.T) {
	const empty = "# nothing accepted\n"

	quiet := "pkg: github.com/kylecannon/gostack/internal/route\n" +
		"RouteDetect/host-2   113.0n ± 3%   114.0n ± 1%   ~ (p=0.500 n=10)\n"

	out, err := runGateOn(t, quiet, empty, nil, commitFlags(baseCommit, headCommit)...)
	require.NoError(t, err)
	assert.Contains(t, out, "no benchmark regressed")

	out, err = runGate(t, empty, nil, commitFlags(baseCommit, headCommit)...)
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "RouteDetect/host-2 sec/op +44.18%")

	entries, err := parseAccepted([]byte(empty), "test")
	require.NoError(t, err)

	base, head := idleReports()

	var report bytes.Buffer

	set := acceptedSet{entries: entries}
	require.ErrorIs(t, compareReports(t.Context(), base, head, set, &report), errRegression)
	assert.Contains(t, report.String(), "### Accepted (0)")

	report.Reset()
	require.NoError(t, compareReports(t.Context(), base, base, set, &report))
}

// Two entries that can match one row are a load error: the gates would take
// whichever accepts the change. A benchmark and the same name with a
// -GOMAXPROCS-like suffix match the row of the longer name, but a name that
// only ends in digits is a real benchmark and loads alone.
func Test_parseAccepted_overlapping_entries(t *testing.T) {
	route := benchstatEntry
	const (
		host     = "RouteDetect/host"
		other    = "RouteDetect/other"
		numbered = "BenchmarkListObjectsV2/delimiter=slash/prefix=folder-7"
	)

	nl := "\n"
	body := "  gate: benchstat" + nl + "  package: internal/route" + nl + "  benchmark: " + host + nl +
		"  unit: sec/op" + nl + "  max: +5%" + nl + "  reason: r" + nl + "  pr: 4" + nl + "  commit: " + acceptCommit + nl
	mergeKey := "- &a" + nl + body + "- <<: *a" + nl + "  benchmark: " + host + "-2" + nl + "  max: +50%" + nl

	// two lists the benchmarks a then b, both in routePkg with unit sec/op.
	two := func(a, b string) string {
		return route(routePkg, a, unitSecOp, "+5%") + route(routePkg, b, unitSecOp, "+50%")
	}

	refused := map[string]string{
		"a name and the same with -2": two(host, host+"-2"),
		"the suffixed name first":     two(host+"-2", host),
		"a suffix of several digits":  two(host, host+"-128"),
		"the package spelled in full": route(routePkg, host, unitSecOp, "+5%") +
			route("github.com/kylecannon/gostack/internal/route", host+"-2", unitSecOp, "+50%"),
		"a name with digits in it, and without them": two(numbered, strings.TrimSuffix(numbered, "-7")),
		"a YAML merge key that makes a second entry": mergeKey,
	}

	for name, give := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			_, err := parseAccepted([]byte(give), "test")
			require.ErrorIs(t, err, errAccepted)
			assert.Contains(t, err.Error(), "test entry 2")
			assert.Contains(t, err.Error(), "same metric as entry 1")
		})
	}

	loads := map[string]string{
		"a benchmark whose own name ends in digits":   route(routePkg, numbered, unitSecOp, "+50%"),
		"two names that differ by more than a suffix": two(host, other),
		"a longer name that is not a suffix":          two(host, host+"ile"),
		"a suffix that is not only digits":            two(host, host+"-2x"),
		"two numbers, which no GOMAXPROCS suffix is":  two(host, host+"-1-2"),
		"names that differ in case":                   two(host, "RouteDetect/Host"),
		"another unit": route(routePkg, host, unitSecOp, "+5%") +
			route(routePkg, host+"-2", "B/op", "+50%"),
		"another package": route(routePkg, host, unitSecOp, "+5%") +
			route("internal/gateway", host+"-2", unitSecOp, "+50%"),
	}

	for name, give := range loads {
		t.Run("loads "+name, func(t *testing.T) {
			entries, err := parseAccepted([]byte(give), "test")
			require.NoError(t, err)
			assert.NotEmpty(t, entries)
		})
	}

	// The load check and the gate agree on what one row is: two names overlap
	// exactly when a row the gate could print matches both.
	names := []string{
		host, host + "-2", host + "-128", host + "ile", host + "-2x", host + "-1-2", other, numbered,
		strings.TrimSuffix(numbered, "-7"), "RouteDetect/Host",
	}

	for _, a := range names {
		for _, b := range names {
			ea := &acceptedEntry{Gate: gateBenchstat, Package: routePkg, Benchmark: a, Unit: unitSecOp}
			eb := &acceptedEntry{Gate: gateBenchstat, Package: routePkg, Benchmark: b, Unit: unitSecOp}

			var shared bool

			for _, row := range []string{a, b, a + "-2", b + "-2"} {
				r := benchRow{pkg: "github.com/kylecannon/gostack/internal/route", name: row, unit: unitSecOp}
				shared = shared || (r.matches(ea) && r.matches(eb))
			}

			assert.Equal(t, shared, ea.overlaps(eb), "%q and %q", a, b)
		}
	}
}

// A percent bound is at most 1000%, and an absolute one at most its unit's cap.
// A bound at the cap loads and one past it does not.
func Test_parseAccepted_caps(t *testing.T) {
	loadSystem := func(bound string) error {
		_, err := parseAccepted([]byte(strings.Replace(idleEntry, "%s", `"`+bound+`"`, 1)), "test")

		return err
	}

	loadBenchstat := func(bound string) error {
		_, err := parseAccepted([]byte(benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, bound)), "test")

		return err
	}

	for _, bound := range []string{"1000%", bound1000, "1000.0%", "999.99%"} {
		require.NoError(t, loadBenchstat(bound), bound)
		require.NoError(t, loadSystem(bound), bound)
	}

	for _, bound := range []string{"1000.01%", "+1001%", "5000%", "100000%"} {
		err := loadBenchstat(bound)
		require.ErrorIs(t, err, errAccepted, bound)
		assert.Contains(t, err.Error(), "1000% cap")
		require.ErrorIs(t, loadSystem(bound), errAccepted, bound)
	}

	caps := map[string]string{
		"MiB": "65536", "ms": "3600000", "s": "3600", unitCyclesPerSec: "10000000",
	}

	for unit, value := range caps {
		require.NoError(t, loadSystem(value+" "+unit), unit)

		past := loadSystem(value + "1 " + unit)
		require.ErrorIs(t, past, errAccepted, unit)
		assert.Contains(t, past.Error(), value+" "+unit+" cap")

		fraction := loadSystem(value + ".5 " + unit)
		require.ErrorIs(t, fraction, errAccepted, unit)
	}

	require.NoError(t, loadSystem("65535.5 MiB"))
}

// workflowFile is the part of .github/workflows/bench.yml the gate step test reads.
type workflowFile struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	TimeoutMinutes int            `yaml:"timeout-minutes"`
	Steps          []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
}

func readWorkflow(t *testing.T) workflowFile {
	t.Helper()

	text, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "bench.yml"))
	require.NoError(t, err)

	var workflow workflowFile

	require.NoError(t, yaml.Unmarshal(text, &workflow))

	return workflow
}

// A shard names its artifact from the index select wrote into its matrix entry,
// which is the name join reads, and not from strategy.job-index, whose order GitHub
// does not say follows the entries'. The shard job's timeout is the limit select
// holds a shard to.
func Test_workflow_shard_job(t *testing.T) {
	shard := readWorkflow(t).Jobs["shard"]

	assert.Equal(t, int(defaultShardLimit/time.Minute), shard.TimeoutMinutes,
		"selection fails a unit past the limit, so the limit is the job's timeout")

	var names []string

	for _, step := range shard.Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			names = append(names, step.With["name"])
		}
	}

	assert.Equal(t, []string{shardArtifactPrefix + "${{ matrix.index }}"}, names)
}

// The gate step fails when the gate exits 1, whatever shell the workflow
// defaults to, and puts the gate's stderr in the step summary. A load error of
// the accepted list is written to stderr.
func Test_workflow_gate_step(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}

	var script string

	for _, step := range readWorkflow(t).Jobs["benchstat"].Steps {
		if strings.Contains(step.Run, "bench gate") {
			script = step.Run
		}
	}

	require.NotEmpty(t, script, "the workflow has a step that runs the gate")

	dir := t.TempDir()
	stub := "#!/bin/sh\necho 'gate: accepted list: max is above the cap' >&2\necho 'regressions over 10%'\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(stub), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "benchstat.txt"), []byte("x\n"), 0o600))

	summary := filepath.Join(dir, "summary.md")

	// A bare bash, with none of the -e or -o pipefail the workflow's shell adds.
	cmd := exec.CommandContext(t.Context(), bash, "-c", script)
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_WORKSPACE=" + dir, "RUNNER_TEMP=" + dir, "GITHUB_STEP_SUMMARY=" + summary,
		"BASE_SHA=" + baseCommit, "HEAD_SHA=" + headCommit,
	}

	err = cmd.Run()

	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "the step fails when the gate exits 1")
	assert.Equal(t, 1, exit.ExitCode())

	written, err := os.ReadFile(summary)
	require.NoError(t, err)
	assert.Contains(t, string(written), "regressions over 10%")
	assert.Contains(t, string(written), "accepted list: max is above the cap", "stderr reaches the summary")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{
		"-C", dir, "-c", "user.name=t", "-c",
		"user.email=t@example.com", "-c", "commit.gpgsign=false",
	}, args...)...)

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	return strings.TrimSpace(string(out))
}

// isolateGit drops the GIT_* variables a caller set, such as GIT_DIR and
// GIT_INDEX_FILE in a pre-commit hook, so git works on the test's own
// repository and never on the caller's.
func isolateGit(t *testing.T) {
	t.Helper()

	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, value)
			require.NoError(t, os.Unsetenv(key))
		}
	}
}

// gitContains answers from real history, and refuses where history is cut.
func Test_gitContains(t *testing.T) {
	// Under the pre-commit hook an inherited GIT_DIR once let this test re-initialise and commit to the live repo.
	isolateGit(t)

	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	first := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "two")
	second := git(t, dir, "rev-parse", "HEAD")

	t.Chdir(dir)

	held, err := gitContains(t.Context(), first, second)
	require.NoError(t, err)
	assert.True(t, held, "the second commit holds the first")

	held, err = gitContains(t.Context(), second, first)
	require.NoError(t, err)
	assert.False(t, held, "the first commit does not hold the second")

	_, err = gitContains(t.Context(), "abcdef0123456", second)
	require.Error(t, err, "a commit git does not have is an error, not a no")

	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, dir, "clone", "-q", "--depth", "1", "file://"+dir, shallow)
	t.Chdir(shallow)

	_, err = gitContains(t.Context(), second, second)
	require.ErrorContains(t, err, "shallow")
}

// benchstatThroughput is a benchstat output with one throughput row; its delta
// is the one %s.
const benchstatThroughput = `goos: linux
goarch: amd64
pkg: github.com/kylecannon/gostack/internal/route
cpu: AMD EPYC 7763 64-Core Processor
                    │   base.txt    │               head.txt               │
                    │      B/s      │      B/s       vs base               │
RouteDetect/host-2   94.42Mi ± 1%   0.09Mi ± 1%   %s% (p=0.000 n=10)
`

// A throughput that falls d percent costs 100d/(100-d) percent more for the
// same work, and a percent bound reads as that rise: the bound100 that lets a
// latency double lets a throughput halve, and no more.
func Test_gate_accepted_throughput_bound(t *testing.T) {
	commits := commitFlags(baseCommit, headCommit)

	tests := []struct {
		name  string
		delta string
		max   string
		fail  bool
	}{
		{name: "a halving is a doubling", delta: "-50.00", max: bound100},
		{name: "a drop past a doubling fails", delta: "-50.01", max: bound100, fail: true},
		{name: "a 99.90% drop under +100% fails", delta: "-99.90", max: bound100, fail: true},
		{name: "a 99.90% drop under +50% fails", delta: "-99.90", max: "+50%", fail: true},
		{name: "a drop to an eleventh fits the cap", delta: "-90.90", max: bound1000},
		{name: "a drop past the cap fails", delta: "-90.92", max: bound1000, fail: true},
		{name: "a total loss fails whatever the bound", delta: "-100.00", max: bound1000, fail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := benchstatEntry(routePkg, "RouteDetect/host", "B/s", tt.max)
			give := strings.Replace(benchstatThroughput, "%s%", tt.delta+"%", 1)

			out, err := runGateOn(t, give, list, history(false), commits...)
			if !tt.fail {
				require.NoError(t, err)
				assert.Contains(t, out, "Accepted (1)")

				return
			}

			require.ErrorIs(t, err, errRegression)
			assert.NotContains(t, out, "Accepted (")
			assert.Contains(t, out, "past the accepted")
		})
	}
}

// An absolute bound is held to the percent cap too: the change a head allows
// is the head against the base, whichever way the bound is written.
func Test_compare_accepted_absolute_bound_cap(t *testing.T) {
	base, head := idleReports()
	head.Targets[0].Idle.Healthcheck.RSSBytes = around(60000 * bytesPerMiB)

	for name, bound := range map[string]string{
		"an absolute bound that allows 1.8 million percent": "65536 MiB",
		"the same pair under 1000%":                         "1000%",
	} {
		t.Run(name+" fails", func(t *testing.T) {
			accepted := systemList(t, bound)
			accepted.contains = history(false)

			var out bytes.Buffer

			err := compareReports(t.Context(), base, head, accepted, &out)
			require.ErrorIs(t, err, errRegression)
			assert.Contains(t, out.String(), "### Accepted (0)")
			assert.Contains(t, out.String(), "accepted list: ")
		})
	}

	t.Run("an absolute bound inside the cap still holds", func(t *testing.T) {
		_, near := idleReports()
		near.Targets[0].Idle.Healthcheck.RSSBytes = around(30 * bytesPerMiB)

		accepted := systemList(t, "40 MiB")
		accepted.contains = history(false)

		var out bytes.Buffer

		require.NoError(t, compareReports(t.Context(), base, near, accepted, &out))
		assert.Contains(t, out.String(), "### Accepted (1)")
	})
}

// A bound reads in the direction of the metric: a throughput's head must stay
// above it, and a drop the percent cap refuses is refused whatever the bound.
func Test_measured_absolute_bound_direction(t *testing.T) {
	// 27000 to 0.027 cycles/s: a floor of 0.000001 cycles/s holds the head, and
	// the drop is far past the cap.
	drop := measured{change: -99.9999, head: 0.027, unit: unitCyclesPerSec}
	assert.NotEmpty(t, drop.within(limit{absolute: 0.000001, unit: unitCyclesPerSec}))
	assert.NotEmpty(t, drop.within(limit{absolute: 0.03, unit: unitCyclesPerSec}), "a head under the floor fails")

	kept := measured{change: -50, head: 13500, unit: unitCyclesPerSec}
	assert.Empty(t, kept.within(limit{absolute: 10000, unit: unitCyclesPerSec}))
	assert.NotEmpty(t, kept.within(limit{absolute: 14000, unit: unitCyclesPerSec}))
}

func Test_unused_accepted_entries_are_reported(t *testing.T) {
	commits := commitFlags(baseCommit, headCommit)
	list := benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, "+60%") +
		benchstatEntry(routePkg, "RouteDetect/*", unitSecOp, "+60%") +
		benchstatEntry(routePkg, `"RouteDetect/signing_name_and_target "`, unitSecOp, "+60%") +
		benchstatEntry(routePkg, "RouteDetect/gone", unitSecOp, "+60%")

	out, err := runGate(t, list, history(false), commits...)
	require.NoError(t, err, "an unused entry does not fail the gate")
	assert.Contains(t, out, "Accepted (1)")
	assert.Contains(t, out, "Unused accepted entries (3):")
	assert.Contains(t, out, `benchstat "internal/route" "RouteDetect/*" "sec/op": matched no regression`)
	assert.Contains(t, out, `"RouteDetect/signing_name_and_target " "sec/op": matched no regression`,
		"a trailing space shows inside the quotes")
	assert.Contains(t, out, `benchstat "internal/route" "RouteDetect/gone" "sec/op": matched no regression`)
	assert.NotContains(t, out, `"RouteDetect/host"`)

	out, err = runGate(t, benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, "+60%"), history(false), commits...)
	require.NoError(t, err)
	assert.NotContains(t, out, "Unused", "a list whose entries all match says nothing")

	base, head := idleReports()
	accepted := systemList(t, idleMax)
	accepted.contains = history(false)
	accepted.entries = append(accepted.entries, accepted.entries[0])
	accepted.entries[1].Cell = "gone"

	var cmp bytes.Buffer

	require.NoError(t, compareReports(t.Context(), base, head, accepted, &cmp))
	assert.Contains(t, cmp.String(), "### Accepted (1)")
	assert.Contains(t, cmp.String(), "### Unused accepted entries (1)")
	assert.Contains(t, cmp.String(), `- system "gostack" "gone" "idle RSS": matched no regression`)
}

// rateReports are a base and a head of one cell whose throughput went from 2700
// cycles/s to the given one, and nothing else changed.
func rateReports(head spread) (base, h report) {
	base = testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	h = testReport(hostA, testCell(cellID, 800, 13000, 2700, 50*bytesPerMiB))
	base.Commit, h.Commit = baseCommit, headCommit
	h.Targets[0].Cells[0].CyclesPerSec = head

	return base, h
}

// throughputList is an accepted list with one entry for the throughput of the
// cell rateReports compares.
func throughputList(t *testing.T, bound string) acceptedSet {
	t.Helper()

	list := strings.Replace(idleEntry, "idle RSS", "throughput", 1)
	list = strings.Replace(list, "cell: idle", "cell: "+cellID, 1)

	entries, err := parseAccepted([]byte(strings.Replace(list, "%s", bound, 1)), "test")
	require.NoError(t, err)

	return acceptedSet{entries: entries, contains: history(false)}
}

// A percent bound reads a throughput's fall as the rise it costs, so a refusal
// says so: an 18% fall is a 22% rise, past a 20% bound.
func Test_refusal_states_the_rise_it_computed(t *testing.T) {
	commits := commitFlags(baseCommit, headCommit)
	give := strings.Replace(benchstatThroughput, "%s%", "-18.00%", 1)

	out, err := runGateOn(t, give, benchstatEntry(routePkg, "RouteDetect/host", "B/s", "+20%"), history(false), commits...)
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "a fall of 18.0% reads as a rise of +22.0% in cost, past the accepted 20%")

	list := benchstatEntry(routePkg, "RouteDetect/host", unitSecOp, "+40%")

	out, err = runGateOn(t, benchstatRoute, list, history(false), commits...)
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "the change is a rise of +44.2%, past the accepted 40%")

	base, head := rateReports(around(2700 * 0.82))
	accepted := throughputList(t, "+20%")

	var cmp bytes.Buffer

	require.ErrorIs(t, compareReports(t.Context(), base, head, accepted, &cmp), errRegression)
	assert.Contains(t, cmp.String(), "a fall of 18.0% reads as a rise of +22.0% in cost, past the accepted 20%")
}

// A change just past the cap prints with the decimals that show it past, not as
// the cap itself.
func Test_cap_refusal_shows_the_change_past_the_cap(t *testing.T) {
	over := measured{change: 1000.0001, head: 60000, unit: unitMiB, lowerIsBetter: true}

	note := over.within(limit{absolute: 65536, unit: unitMiB})
	assert.Contains(t, note, "allows a rise of +1000.0001%, past the 1000% cap on every bound")

	fall := measured{change: -100, unit: unitCyclesPerSec}
	assert.Contains(t, fall.within(limit{percent: 50}), "a fall to nothing is a rise without end, past the accepted 50%")
	assert.Contains(t, fall.within(limit{absolute: 1, unit: unitCyclesPerSec}), "allows a rise without end")
}

// A fall prints with the two decimals benchstat gives it: -10.01% under a bound
// of +11% reads as 10.01%, not as the 10.0% the gate flags at.
func Test_fall_prints_the_decimals_benchstat_gives(t *testing.T) {
	m := measured{change: -10.01, unit: unitBPerSec}
	assert.Contains(t, m.within(limit{percent: 11}), "a fall of 10.01% reads as a rise of +11.1% in cost")

	m = measured{change: -18, unit: unitBPerSec}
	assert.Contains(t, m.within(limit{percent: 20}), "a fall of 18.0% reads as a rise of +22.0% in cost")
}

// An entry whose bound is at or under what the gate flags can match a regression
// and hold none of them: a benchstat B/s fall is flagged past 10%, a rise of
// 11.11% in cost. It is listed as unused with the reason, and still fails.
func Test_dead_bounds_are_listed(t *testing.T) {
	commits := commitFlags(baseCommit, headCommit)

	tests := []struct {
		name  string
		unit  string
		delta string
		max   string
		dead  string
		held  bool
	}{
		{
			name: "a throughput bound inside the flagged band", unit: unitBPerSec, delta: fallPastFlag, max: "+10%",
			dead: "a fall past 10%, which reads as a rise past +11.11%",
		},
		{
			name: "a throughput bound at the edge", unit: unitBPerSec, delta: fallPastFlag, max: "+11.11%",
			dead: "so it holds none",
		},
		{name: "a throughput bound past the band holds", unit: unitBPerSec, delta: fallPastFlag, max: "+11.2%", held: true},
		{name: "a cost bound at the flag", unit: unitSecOp, delta: "+10.5", max: "+10%", dead: "a rise past 10%"},
		{name: "a cost bound past the flag holds", unit: unitSecOp, delta: "+10.5", max: "+11%", held: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			give := strings.Replace(benchstatThroughput, "%s%", tt.delta+"%", 1)
			give = strings.Replace(give, "B/s", tt.unit, 2)

			out, err := runGateOn(t, give, benchstatEntry(routePkg, "RouteDetect/host", tt.unit, tt.max), history(false),
				commits...)
			if tt.held {
				require.NoError(t, err)
				assert.Contains(t, out, "Accepted (1)")
				assert.NotContains(t, out, "Unused")

				return
			}

			require.ErrorIs(t, err, errRegression)
			assert.Contains(t, out, "Unused accepted entries (1):")
			assert.Contains(t, out, tt.dead)
			assert.NotContains(t, out, "matched no regression", "it matched the row")
		})
	}

	t.Run("a system throughput bound inside the flagged band", func(t *testing.T) {
		entries, err := parseAccepted([]byte(strings.Replace(
			strings.Replace(strings.Replace(idleEntry, "idle RSS", "throughput", 1), "cell: idle", "cell: "+cellID, 1),
			"%s", "+17%", 1)), "test")
		require.NoError(t, err)

		unused := acceptedSet{entries: entries}.unused(gateSystem, nil)
		require.Len(t, unused, 1)
		assert.Contains(t, unused[0], "a fall past 15%, which reads as a rise past +17.65%")

		entries[0].limit = limit{percent: 18}
		assert.Empty(t, acceptedSet{entries: entries}.unused(gateSystem, []func(*acceptedEntry) bool{
			func(*acceptedEntry) bool { return true },
		}))
	})
}

// benchstatZero is real benchstat output (golang.org/x/perf 4febdb0) for ten runs
// of a benchmark whose B/op and lock-ms go from 0 to a figure.
const benchstatZero = `goos: linux
goarch: amd64
pkg: example/p
           │ zero/base.txt │           zero/head.txt            │
           │    sec/op     │   sec/op     vs base               │
ZeroBase-2     105.5n ± 3%   105.5n ± 3%       ~ (p=1.000 n=10)
geomean        105.5n        105.5n       +0.00%

           │ zero/base.txt │        zero/head.txt         │
           │     B/op      │    B/op     vs base          │
ZeroBase-2       0.00 ± 0%   55.50 ± 6%  ? (p=0.000 n=10)

           │ zero/base.txt │        zero/head.txt         │
           │    lock-ms    │  lock-ms    vs base          │
ZeroBase-2      0.000 ± 0%   5.000 ± 0%  ? (p=0.000 n=10)
`

// A cost that rises from a zero base prints "?" in place of a percent and fails
// the gate all the same, and no bound holds it.
func Test_gate_fails_a_cost_that_rises_from_zero(t *testing.T) {
	out, err := runGateOn(t, benchstatZero, "# nothing accepted\n", history(false))
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "regressions over 10% at p<0.001:")
	assert.Contains(t, out, "ZeroBase-2 B/op rose from zero (p=0.000)")
	assert.Contains(t, out, "ZeroBase-2 lock-ms rose from zero (p=0.000)")
	assert.NotContains(t, out, "ZeroBase-2 sec/op", "a row without a change fails nothing")

	rows, err := findRegressions(strings.NewReader(benchstatZero))
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	commits := commitFlags(baseCommit, headCommit)
	list := benchstatEntry("example/p", "ZeroBase", "B/op", bound1000)

	out, err = runGateOn(t, benchstatZero, list, history(false), commits...)
	require.ErrorIs(t, err, errRegression, "no bound holds a change without an end")
	assert.NotContains(t, out, "Accepted (")
	assert.Contains(t, out, "the change is a rise without end, past the accepted 1000%")
}

// benchstatCollapse is the shape benchstat prints for a benchmark whose
// throughput falls to zero: its sec/op row rises far past the cap, and the B/s
// of each file is set in a table of its own (the head's unit becomes MB/s), so
// the gate reads no row for it. The sec/op row is the one that fails.
const benchstatCollapse = `goos: linux
goarch: amd64
pkg: example/p
           │ zero/base.txt │           zero/head.txt            │
           │    sec/op     │   sec/op      vs base              │
ZeroHead-2     105.5n ± 3%   105.5µ ± 3%  +99900.00% (p=0.000 n=10)

           │ zero/base.txt │
           │      B/s      │
ZeroHead-2    482.1Mi ± 1%

           │ zero/head.txt │
           │     MB/s      │
ZeroHead-2      0.000 ± 0%
`

// A throughput that collapses fails on its sec/op row, and no entry for either
// unit holds it: the rise is past the 1000% cap.
func Test_gate_fails_a_throughput_collapse_on_its_sec_op_row(t *testing.T) {
	out, err := runGateOn(t, benchstatCollapse, "# nothing accepted\n", history(false))
	require.ErrorIs(t, err, errRegression)
	assert.Contains(t, out, "ZeroHead-2 sec/op +99900.00% (p=0.000)")

	commits := commitFlags(baseCommit, headCommit)
	list := benchstatEntry("example/p", "ZeroHead", unitSecOp, bound1000) +
		benchstatEntry("example/p", "ZeroHead", unitBPerSec, bound1000)

	out, err = runGateOn(t, benchstatCollapse, list, history(false), commits...)
	require.ErrorIs(t, err, errRegression, "no bound holds a rise past the cap")
	assert.NotContains(t, out, "Accepted (")
	assert.Contains(t, out, "the change is a rise of +99900.0%, past the accepted 1000%")
}

// The cases around the two zero shapes that are not regressions.
func Test_gate_zero_shapes_that_pass(t *testing.T) {
	tests := []struct {
		name string
		give string
	}{
		{name: "a throughput that rises from zero", give: strings.NewReplacer(
			"B/op", "B/s", "55.50", "55.5Mi").Replace(
			"pkg: example/p\n│ a.txt │ b.txt │\n│ B/op │ B/op vs base │\nX-2  0.00 ± 0%  55.50 ± 6%  ? (p=0.000 n=10)\n")},
		{name: "a cost that stays zero", give: "pkg: example/p\n│ a.txt │ b.txt │\n│ B/op │ B/op vs base │\n" +
			"X-2  0.00 ± 0%  0.00 ± 0%  ~ (p=1.000 n=10)\n"},
		{
			name: "a cost that rises from zero at p=0.023",
			give: "pkg: example/p\n│ a.txt │ b.txt │\n│ B/op │ B/op vs base │\nX-2  0.00 ± 0%  55.50 ± 6%  ? (p=0.023 n=10)\n",
		},
		{name: "a cost that falls to zero", give: "pkg: example/p\n│ a.txt │ b.txt │\n│ B/op │ B/op vs base │\n" +
			"X-2  55.50 ± 6%  0.00 ± 0%  -100.00% (p=0.000 n=10)\n"},
		{name: "a throughput that stays", give: "pkg: example/p\n│ a.txt │ b.txt │\n│ B/s │ B/s vs base │\n" +
			"X-2  1Mi ± 0%  1Mi ± 0%  ~ (p=1.000 n=10)\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := findRegressions(strings.NewReader(tt.give))
			require.NoError(t, err)
			assert.Empty(t, rows)
		})
	}
}

// A system throughput that falls to zero fails: judge steadied on a zero
// median, and cellMetrics dropped a zero spread, so the largest fall there is
// went unseen.
func Test_compare_fails_a_throughput_that_falls_to_zero(t *testing.T) {
	for name, head := range map[string]spread{
		"a median of zero": {Median: 0, Max: 5},
		// Two runs of three completed no cycle and one ran at the base's rate, so the
		// ranges overlap, and the median is still a fall to nothing.
		"a median of zero over a run at the base's rate": {Min: 0, Median: 0, Max: 2800},
		"no run with a cycle":                            {},
		"a fall to a tenth":                              around(270),
		"a collapse to 0.0001":                           around(0.0001),
	} {
		t.Run(name, func(t *testing.T) {
			base, h := rateReports(head)

			var out bytes.Buffer

			require.ErrorIs(t, compareReports(t.Context(), base, h, acceptedSet{}, &out), errRegression)
			assert.Contains(t, out.String(), "### Regressions (1)")
			assert.Contains(t, out.String(), "| throughput |")
		})
	}

	t.Run("a base of zero compares nothing", func(t *testing.T) {
		base, h := rateReports(around(2700))
		base.Targets[0].Cells[0].CyclesPerSec = spread{}

		var out bytes.Buffer

		require.NoError(t, compareReports(t.Context(), base, h, acceptedSet{}, &out))
		assert.Contains(t, out.String(), "### Regressions (0)")
	})

	t.Run("a latency of zero is not a measurement", func(t *testing.T) {
		base, h := rateReports(around(2700))
		h.Targets[0].Cells[0].Ops[opSend] = opSpread{Count: 1000}

		var out bytes.Buffer

		require.NoError(t, compareReports(t.Context(), base, h, acceptedSet{}, &out))
	})

	t.Run("no accepted bound holds the fall", func(t *testing.T) {
		base, h := rateReports(spread{})

		for _, bound := range []string{"+1000%", "1 cycles/s"} {
			var out bytes.Buffer

			err := compareReports(t.Context(), base, h, throughputList(t, bound), &out)
			require.ErrorIs(t, err, errRegression, bound)
			assert.Contains(t, out.String(), "### Accepted (0)")
			assert.Contains(t, out.String(), "rise without end")
		}
	})
}

// The unused entries print when nothing compares too.
func Test_unused_entries_print_when_nothing_compares(t *testing.T) {
	base, head := testReport(hostA), testReport(hostA)
	base.Targets[0].Startup, head.Targets[0].Startup = startupResult{}, startupResult{}
	base.Targets[0].Idle, head.Targets[0].Idle = idleResult{}, idleResult{}

	entries, err := parseAccepted([]byte(strings.Replace(idleEntry, "%s", "+55%", 1)), "test")
	require.NoError(t, err)

	var out bytes.Buffer

	accepted := acceptedSet{entries: entries, contains: history(false)}

	require.NoError(t, compareReports(t.Context(), base, head, accepted, &out))
	assert.Contains(t, out.String(), "Nothing to compare")
	assert.Contains(t, out.String(), `- system "gostack" "idle" "idle RSS": matched no regression`)
}
