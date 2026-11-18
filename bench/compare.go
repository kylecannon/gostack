package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// The regression gate of the system bench: `bench compare` holds one result
// file against another and fails when a gostack cell got slower or bigger.
//
// A metric regresses when both hold:
//
//   - its median moved the wrong way by more than the limit below, and, for a
//     latency, by at least the absolute floor below, and
//   - the ranges of the runs do not overlap: for a cost, the head's lowest run
//     is above the base's highest; for a throughput, the head's highest run is
//     below the base's lowest.
//
// The second rule keeps a noisy host from failing a build. A change past the
// limit whose ranges overlap is reported as noise and does not fail.
//
// Only the gostack and gostack-slim targets are compared, on the cells both
// files hold. LocalStack is a reference and never gates. Results from hosts
// with different fingerprints are not comparable: compare says so and exits 0.
//
// These constants are the one place the limits live. docs/standards.md names
// the gate; the numbers are here.
const (
	// limitP50 is how much a p50 latency may rise.
	limitP50 = 0.15
	// limitP99 is how much a p99 latency may rise.
	limitP99 = 0.30
	// floorP50 is how many milliseconds a p50 latency must also rise by to
	// regress. The results hold latency at 0.01 ms, so at c=1 one tick is a
	// 14% move of a 0.07 ms median.
	floorP50 = 0.05
	// floorP99 is the same for a p99 latency, in milliseconds.
	floorP99 = 0.5
	// limitThroughput is how much the cycles a second may fall.
	limitThroughput = 0.15
	// limitRSS is how much the peak RSS of a cell, or the idle RSS, may rise.
	limitRSS = 0.20
	// limitStartup is how much the time to the ready line may rise.
	limitStartup = 0.25

	maxListed = 25
)

// gated are the targets whose cells gate. LocalStack is reference only.
var gated = map[string]bool{targetGostack: true, targetSlim: true}

// errUsage is the answer to flags that name no pair of files.
var errUsage = errors.New("usage: bench compare -head B.json (-base A.json | -base-dir DIR)")

type metric struct {
	target, cell, name string
	base, head         spread
	// lowerIsBetter is true for latencies, memory and startup, false for throughput.
	lowerIsBetter bool
	limit         float64
	// floor is the least the median must move the wrong way, in the unit of the
	// results file, for the metric to count. Zero means no floor.
	floor float64
	// scale and unit render the numbers.
	scale float64
	unit  string
}

type outcome int

const (
	steady outcome = iota
	worse
	noisy
	improved
)

// change is the head's median against the base's, as a fraction.
func (m metric) change() float64 {
	if m.base.Median == 0 {
		return 0
	}

	return (m.head.Median - m.base.Median) / m.base.Median
}

// judge applies the two rules to a metric.
func (m metric) judge() outcome {
	if m.base.Median == 0 || m.head.Median == 0 {
		return steady
	}

	change := m.change()
	if !m.lowerIsBetter {
		change = -change
	}

	// A move under the floor is a tick of the clock, not a change.
	if change > 0 && math.Abs(m.head.Median-m.base.Median) < m.floor {
		return steady
	}

	switch {
	case change > m.limit && m.disjointWorse():
		return worse
	case change > m.limit:
		return noisy
	case change < -m.limit && m.disjointBetter():
		return improved
	default:
		return steady
	}
}

// disjointWorse reports whether the ranges of the runs do not overlap, with the
// head on the worse side of the base.
func (m metric) disjointWorse() bool {
	if m.lowerIsBetter {
		return m.head.Min > m.base.Max
	}

	return m.head.Max < m.base.Min
}

// disjointBetter is the same with the head on the better side.
func (m metric) disjointBetter() bool {
	if m.lowerIsBetter {
		return m.head.Max < m.base.Min
	}

	return m.head.Min > m.base.Max
}

func (m metric) render(s spread) string {
	return fmt.Sprintf("%.2f %s (%.2f-%.2f)", s.Median/m.scale, m.unit, s.Min/m.scale, s.Max/m.scale)
}

// metrics lists what is compared on one cell of a gated target.
func cellMetrics(target string, base, head cellResult) []metric {
	var out []metric

	add := func(name string, b, h spread, lower bool, limit, floor, scale float64, unit string) {
		if b.zero() || h.zero() {
			return
		}

		out = append(out, metric{
			target: target, cell: base.ID, name: name, base: b, head: h,
			lowerIsBetter: lower, limit: limit, floor: floor, scale: scale, unit: unit,
		})
	}

	// Every operation the base timed, whatever the service: the cell names them.
	for _, name := range base.opNames() {
		b, h := base.Ops[name], head.Ops[name]
		if b.Count == 0 || h.Count == 0 {
			continue
		}

		add(name+" p50", b.P50us, h.P50us, true, limitP50, floorP50*microsPerMilli, microsPerMilli, "ms")
		add(name+" p99", b.P99us, h.P99us, true, limitP99, floorP99*microsPerMilli, microsPerMilli, "ms")
	}

	add("throughput", base.CyclesPerSec, head.CyclesPerSec, false, limitThroughput, 0, 1, "cycles/s")
	add("peak RSS", base.PeakRSSBytes, head.PeakRSSBytes, true, limitRSS, 0, bytesPerMiB, "MiB")

	return out
}

// comparison is the result of holding two reports against each other.
type comparison struct {
	compared     int
	metrics      int
	regressions  []metric
	noise        []metric
	improvements []metric
	missing      []string
}

func compareTargets(base, head report) comparison {
	var cmp comparison

	for _, bt := range base.Targets {
		ht, ok := head.target(bt.Name)
		if !ok || !gated[bt.Name] {
			continue
		}

		var all []metric

		startup := metric{
			target: bt.Name, cell: "startup", name: "startup", base: bt.Startup.MS, head: ht.Startup.MS,
			lowerIsBetter: true, limit: limitStartup, scale: msPerSecond, unit: "s",
		}
		idle := metric{
			target: bt.Name, cell: "idle", name: "idle RSS", base: bt.Idle.Healthcheck.RSSBytes,
			head: ht.Idle.Healthcheck.RSSBytes, lowerIsBetter: true, limit: limitRSS, scale: bytesPerMiB, unit: "MiB",
		}

		all = append(all, startup, idle)

		for _, bc := range bt.Cells {
			hc, ok := ht.cell(bc.ID)
			if !ok || hc.Problem != "" || bc.Problem != "" {
				cmp.missing = append(cmp.missing, bt.Name+" "+bc.ID)

				continue
			}

			cmp.compared++

			all = append(all, cellMetrics(bt.Name, bc, hc)...)
		}

		for _, m := range all {
			if m.base.zero() || m.head.zero() {
				continue
			}

			cmp.metrics++

			switch m.judge() {
			case worse:
				cmp.regressions = append(cmp.regressions, m)
			case noisy:
				cmp.noise = append(cmp.noise, m)
			case improved:
				cmp.improvements = append(cmp.improvements, m)
			default:
			}
		}
	}

	return cmp
}

// compareReports writes the comparison and returns errRegression when a gated
// metric regressed. A pair of reports that cannot be compared is explained and
// returns nil.
func compareReports(base, head report, out io.Writer) error {
	var b strings.Builder

	b.WriteString("## System bench compare\n\n")
	fmt.Fprintf(&b, "Base: %s %s (%s profile). Head: %s %s (%s profile).\n\n",
		base.Date, base.Commit, base.Profile, head.Date, head.Commit, head.Profile)

	if base.Host.Fingerprint != head.Host.Fingerprint {
		fmt.Fprintf(&b, "**Not compared.** The results come from different hosts, so a difference would measure the "+
			"host and not the code.\n\n- base: %s\n- head: %s\n", base.Host.Fingerprint, head.Host.Fingerprint)

		return write(out, b.String())
	}

	cmp := compareTargets(base, head)

	if cmp.metrics == 0 {
		b.WriteString("Nothing to compare: the two results hold no gostack or gostack-slim cell in common.\n")

		return write(out, b.String())
	}

	fmt.Fprintf(&b, "Host: %s.\n\nCompared %d cells and %d metrics of the gostack targets. Limits: p50 +%.0f%%, "+
		"p99 +%.0f%%, throughput -%.0f%%, RSS +%.0f%%, startup +%.0f%%; a latency must also rise by %.2f ms (p50) "+
		"or %.1f ms (p99); a metric fails only when the ranges of the runs do not overlap.\n\n",
		head.Host.Fingerprint, cmp.compared, cmp.metrics,
		limitP50*percentScale, limitP99*percentScale, limitThroughput*percentScale, limitRSS*percentScale,
		limitStartup*percentScale, floorP50, floorP99)

	table(&b, "Regressions", cmp.regressions)
	table(&b, "Changes past the limit with overlapping ranges (noise, not failing)", cmp.noise)
	table(&b, "Improvements", cmp.improvements)

	if len(cmp.missing) > 0 {
		fmt.Fprintf(&b, "### Not compared\n\n%d cells of the base are not in the head, or did not run there:\n\n",
			len(cmp.missing))

		for _, id := range cmp.missing[:min(len(cmp.missing), maxListed)] {
			fmt.Fprintf(&b, "- %s\n", id)
		}

		b.WriteString("\n")
	}

	err := write(out, b.String())
	if err != nil {
		return err
	}

	if len(cmp.regressions) > 0 {
		return fmt.Errorf("%w: %d of %d metrics", errRegression, len(cmp.regressions), cmp.metrics)
	}

	return nil
}

func write(out io.Writer, s string) error {
	_, err := io.WriteString(out, s)
	if err != nil {
		return fmt.Errorf("write the comparison: %w", err)
	}

	return nil
}

func table(b *strings.Builder, title string, metrics []metric) {
	fmt.Fprintf(b, "### %s (%d)\n\n", title, len(metrics))

	if len(metrics) == 0 {
		b.WriteString("None.\n\n")

		return
	}

	b.WriteString("| Target | Cell | Metric | Base median (range) | Head median (range) | Change |\n")
	b.WriteString("|---|---|---|---:|---:|---:|\n")

	for _, m := range metrics[:min(len(metrics), maxListed)] {
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %+.1f%% |\n", m.target, m.cell, m.name,
			m.render(m.base), m.render(m.head), m.change()*percentScale)
	}

	if len(metrics) > maxListed {
		fmt.Fprintf(b, "\n%d more not listed.\n", len(metrics)-maxListed)
	}

	b.WriteString("\n")
}

// compareMain is `bench compare`.
func compareMain(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	basePath := flags.String("base", "", "the result file to compare against")
	baseDir := flags.String("base-dir", "", "a directory of committed results: the newest with the head's fingerprint")
	headPath := flags.String("head", "", "the result file to check")

	err := flags.Parse(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *headPath == "" || (*basePath == "") == (*baseDir == "") {
		return errUsage
	}

	return compareFiles(ctx, *basePath, *baseDir, *headPath, stdout)
}

// compareFiles compares head with the file basePath, or with the newest result
// in baseDir that the host of head can be compared with.
func compareFiles(ctx context.Context, basePath, baseDir, headPath string, out io.Writer) error {
	head, err := loadReport(headPath)
	if err != nil {
		return err
	}

	var base report

	if basePath != "" {
		base, err = loadReport(basePath)
		if err != nil {
			return err
		}
	} else {
		var found bool

		base, found, err = newestComparable(ctx, baseDir, headPath, head)
		if err != nil {
			return err
		}

		if !found {
			return write(out, "## System bench compare\n\nNo committed result in "+baseDir+" has the fingerprint of this "+
				"host ("+head.Host.Fingerprint.String()+"), so there is nothing to compare against.\n")
		}
	}

	return compareReports(base, head, out)
}

// newestComparable returns the base for head from the results in dir that git
// tracks and that were taken on a host with head's fingerprint. Each cell comes
// from the newest of those results that holds it, so a result of one service
// does not hide the cells of another that an older result has. The date, commit
// and the rest of the base are the newest result's. Newest is by the time
// inside the file, not by file name or modification time.
func newestComparable(ctx context.Context, dir, headPath string, head report) (report, bool, error) {
	paths, err := committedResults(ctx, dir)
	if err != nil {
		return report{}, false, err
	}

	headAbs, _ := filepath.Abs(headPath)

	var reports []report

	for _, path := range paths {
		abs, _ := filepath.Abs(path)
		if abs == headAbs {
			continue
		}

		rep, err := loadReport(path)
		if err != nil || rep.Schema < reportSchema || rep.Host.Fingerprint != head.Host.Fingerprint {
			continue
		}

		reports = append(reports, rep)
	}

	if len(reports) == 0 {
		return report{}, false, nil
	}

	slices.SortStableFunc(reports, func(a, b report) int { return strings.Compare(b.Time, a.Time) })

	return mergeCells(reports), true, nil
}

// mergeCells takes the first report, the newest, and adds to each of its targets
// the cells that only an older report has, from the newest of them. A cell that
// failed to run counts as missing.
func mergeCells(newestFirst []report) report {
	base := newestFirst[0]
	base.Targets = slices.Clone(base.Targets)

	for i := range base.Targets {
		base.Targets[i].Cells = slices.Clone(base.Targets[i].Cells)
	}

	for _, older := range newestFirst[1:] {
		for _, from := range older.Targets {
			i := slices.IndexFunc(base.Targets, func(t targetResult) bool { return t.Name == from.Name })
			if i < 0 {
				continue
			}

			cells := &base.Targets[i].Cells

			for _, c := range from.Cells {
				if c.Problem != "" {
					continue
				}

				at := slices.IndexFunc(*cells, func(have cellResult) bool { return have.ID == c.ID })

				if at < 0 {
					*cells = append(*cells, c)
				} else if (*cells)[at].Problem != "" {
					(*cells)[at] = c
				}
			}
		}
	}

	return base
}

// committedResults lists the result files git tracks in dir. Outside a git
// checkout it lists every json file there.
func committedResults(ctx context.Context, dir string) ([]string, error) {
	//nolint:gosec // dir is a flag of this tool
	out, err := exec.CommandContext(ctx, "git", "ls-files", "--", dir).Output()
	if err != nil {
		return jsonFiles(dir)
	}

	var paths []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if strings.HasSuffix(line, ".json") {
			paths = append(paths, line)
		}
	}

	return paths, nil
}

func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}

	var paths []string

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}

	return paths, nil
}
