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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acceptCommit = "8fb22fa3d73e976a0225b14a39cfb276529d06aa"
	baseCommit   = "1cf05e19432a6689a7ec4cc04123530adb3a4d10"
	headCommit   = "563067b9990063b79d38d4f7ad445ca93468d9ef"

	routePkg  = "internal/route"
	unitSecOp = "sec/op"
	unitMiB   = "MiB"
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
		"900 cycles/s": {absolute: 900, unit: "cycles/s"},
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

// The committed list must load: CI reads it on every pull request.
func Test_committed_accepted_list_loads(t *testing.T) {
	entries, err := loadAccepted("accepted.yaml")
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
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
