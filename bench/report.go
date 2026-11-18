package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	bytesPerMiB = 1 << 20
	bytesPerGiB = 1 << 30
	msPerSecond = 1000
	dirMode     = 0o750
	fileMode    = 0o600
	noValue     = "n/a"

	// reportSchema is the version of the result file. Version 1, the first
	// system bench, had no cells and no fingerprint. Version 2 had SQS's send,
	// receive and delete as fields of a cell, and a cell id without a service.
	// Version 3 keys a cell's operations by name and starts its id with the
	// service and workload; loadReport converts version 2.
	reportSchema = 3

	// slowP99us is the p99 above which a cell at concurrency 16 is listed.
	slowP99us   = 50_000
	attentionAt = levelMid

	// cycleColumns are the columns of a cell table after its operations: cycles a
	// second and peak RSS.
	cycleColumns = 2
)

// report is the content of bench/results/<date>-<sha>.json.
type report struct {
	Schema   int            `json:"schema"`
	Date     string         `json:"date"`
	Time     string         `json:"time"`
	Commit   string         `json:"commit"`
	Dirty    bool           `json:"dirty"`
	Profile  string         `json:"profile"`
	Host     hostInfo       `json:"host"`
	Settings settings       `json:"settings"`
	Targets  []targetResult `json:"targets"`
}

// fingerprint is what has to match for two results to be comparable. A run on a
// different OS, architecture, core count, Docker or VM layer measures a
// different machine.
type fingerprint struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	Docker   string `json:"docker"`
	OrbStack bool   `json:"orbstack"`
}

func (f fingerprint) String() string {
	return fmt.Sprintf("%s/%s, %d CPUs, Docker %s, OrbStack %t", f.OS, f.Arch, f.CPUs, f.Docker, f.OrbStack)
}

type hostInfo struct {
	Docker      string      `json:"docker"`
	OS          string      `json:"os"`
	Kernel      string      `json:"kernel"`
	Arch        string      `json:"arch"`
	CPUs        int         `json:"cpus"`
	MemoryBytes uint64      `json:"memory_bytes"`
	Fingerprint fingerprint `json:"fingerprint"`
}

type settings struct {
	Profile          string  `json:"profile"`
	StartupRuns      int     `json:"startup_runs"`
	IdleRuns         int     `json:"idle_runs"`
	SettleSeconds    float64 `json:"settle_seconds"`
	IdleSeconds      float64 `json:"idle_seconds"`
	CellSeconds      float64 `json:"cell_seconds"`
	Reps             int     `json:"reps"`
	Cycles           int     `json:"cycles"`
	CycleConcurrency int     `json:"cycle_concurrency"`
}

// spread is the minimum, median and maximum of the runs of one measurement.
// compare calls two measurements different only when their spreads do not
// overlap.
type spread struct {
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

func spreadOf(values []float64) spread {
	if len(values) == 0 {
		return spread{}
	}

	return spread{Min: slices.Min(values), Median: median(values), Max: slices.Max(values)}
}

func (s spread) zero() bool { return s == spread{} }

// opSpread is the latency of one operation over the runs of a cell.
type opSpread struct {
	Count  int    `json:"count"`
	Errors int    `json:"errors"`
	P50us  spread `json:"p50_us"`
	P99us  spread `json:"p99_us"`
}

// cellResult is every run of one cell. Ops holds each operation the workload
// timed, by name.
type cellResult struct {
	ID           string              `json:"id"`
	Spec         cellSpec            `json:"spec"`
	Runs         int                 `json:"runs"`
	Ops          map[string]opSpread `json:"ops"`
	CyclesPerSec spread              `json:"cycles_per_sec"`
	PeakRSSBytes spread              `json:"peak_rss_bytes"`
	CPUPercent   spread              `json:"cpu_percent"`
	Counters     map[string]int      `json:"counters,omitempty"`
	Problem      string              `json:"problem,omitempty"`
}

// summarizeCell folds the runs of one cell.
func summarizeCell(spec cellSpec, runs []runResult) cellResult {
	cell := cellResult{ID: spec.id(), Spec: spec, Runs: len(runs), Ops: map[string]opSpread{}}

	names := map[string]bool{}

	for _, run := range runs {
		for name := range run.Ops {
			names[name] = true
		}
	}

	for name := range names {
		p50, p99 := make([]float64, 0, len(runs)), make([]float64, 0, len(runs))

		var out opSpread

		for _, run := range runs {
			stats, ok := run.Ops[name]
			if !ok {
				continue
			}

			out.Count += stats.Count
			out.Errors += stats.Errors

			p50 = append(p50, float64(stats.P50us))
			p99 = append(p99, float64(stats.P99us))
		}

		out.P50us, out.P99us = spreadOf(p50), spreadOf(p99)
		cell.Ops[name] = out
	}

	rate, peak, cpu := make([]float64, 0, len(runs)), make([]float64, 0, len(runs)), make([]float64, 0, len(runs))

	for _, run := range runs {
		rate = append(rate, run.CyclesPerSec)
		peak = append(peak, float64(run.Usage.PeakRSSBytes))
		cpu = append(cpu, run.Usage.CPUPercent)

		for name, n := range run.Counters {
			if cell.Counters == nil {
				cell.Counters = map[string]int{}
			}

			cell.Counters[name] += n
		}
	}

	cell.CyclesPerSec, cell.PeakRSSBytes, cell.CPUPercent = spreadOf(rate), spreadOf(peak), spreadOf(cpu)

	return cell
}

// targetResult holds every measurement of one image. A measurement that could
// not be taken is absent and named in Problems. Agentic is by service.
type targetResult struct {
	Name       string                `json:"name"`
	Image      string                `json:"image"`
	ImageBytes uint64                `json:"image_bytes"`
	Startup    startupResult         `json:"startup"`
	Idle       idleResult            `json:"idle"`
	Cells      []cellResult          `json:"cells"`
	Agentic    map[string]agenticRun `json:"agentic,omitempty"`
	Problems   []string              `json:"problems,omitempty"`
}

type startupResult struct {
	SamplesMS []float64 `json:"samples_ms"`
	MS        spread    `json:"ms"`
}

// idleVariant is the idle container with the image HEALTHCHECK on or off,
// measured on several containers. RSS is the median of each container's samples.
type idleVariant struct {
	Runs          int     `json:"runs"`
	SettleSeconds float64 `json:"settle_seconds"`
	Seconds       float64 `json:"seconds"`
	RSSBytes      spread  `json:"rss_bytes"`
	CPUPercent    spread  `json:"cpu_percent"`
}

type idleResult struct {
	Healthcheck   idleVariant `json:"healthcheck"`
	NoHealthcheck idleVariant `json:"no_healthcheck,omitzero"`
}

func (t *targetResult) problem(what string, err error) {
	if err != nil {
		t.Problems = append(t.Problems, what+": "+err.Error())
	}
}

func (t *targetResult) cell(id string) (cellResult, bool) {
	for _, c := range t.Cells {
		if c.ID == id {
			return c, true
		}
	}

	return cellResult{}, false
}

func (r report) target(name string) (*targetResult, bool) {
	for i := range r.Targets {
		if r.Targets[i].Name == name {
			return &r.Targets[i], true
		}
	}

	return nil, false
}

var errNewerSchema = errors.New("result file is of a newer schema than this tool reads")

// loadReport reads a result file. A file of schema 1 loads with no cells, and a
// file of schema 2 is converted to schema 3.
func loadReport(path string) (report, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a path the user names
	if err != nil {
		return report{}, fmt.Errorf("read %s: %w", path, err)
	}

	var head struct {
		Schema int `json:"schema"`
	}

	err = json.Unmarshal(raw, &head)
	if err != nil {
		return report{}, fmt.Errorf("decode %s: %w", path, err)
	}

	if head.Schema > reportSchema {
		return report{}, fmt.Errorf("%w: %s is schema %d, this tool reads up to %d",
			errNewerSchema, path, head.Schema, reportSchema)
	}

	var rep report

	if head.Schema == schemaV2 {
		rep, err = decodeV2(raw)
	} else {
		err = json.Unmarshal(raw, &rep)
	}

	if err != nil {
		return report{}, fmt.Errorf("decode %s: %w", path, err)
	}

	return rep, nil
}

// writeReport writes <dir>/<date>-<commit>.json and .md and returns the path
// without extension.
func writeReport(dir string, rep report) (string, error) {
	err := os.MkdirAll(dir, dirMode)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	base := filepath.Join(dir, rep.Date+"-"+rep.Commit)

	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the report: %w", err)
	}

	err = os.WriteFile(base+".json", append(raw, '\n'), fileMode)
	if err != nil {
		return "", fmt.Errorf("write the report: %w", err)
	}

	err = os.WriteFile(base+".md", []byte(rep.markdown()), fileMode)
	if err != nil {
		return "", fmt.Errorf("write the summary: %w", err)
	}

	return base, nil
}

// markdown renders the summary tables.
func (r report) markdown() string {
	var b strings.Builder

	state := "clean tree"
	if r.Dirty {
		state = "dirty tree"
	}

	fmt.Fprintf(&b, "# System benchmark %s, %s, %s profile (%s)\n\n", r.Date, r.Commit, r.Profile, state)
	fmt.Fprintf(&b, "Host: %s, kernel %s, %.1f GiB.\n\n", r.Host.Fingerprint, r.Host.Kernel,
		float64(r.Host.MemoryBytes)/bytesPerGiB)
	fmt.Fprintf(&b, "Every cell ran %d times of %.0f s, each time against a fresh container. Numbers are the median of "+
		"the runs, with the range in the JSON. Memory is the resident set of the container (anon plus mapped file "+
		"pages). CPU is a percentage of one core.\n\n", r.Settings.Reps, r.Settings.CellSeconds)

	r.footprintTable(&b)

	for _, t := range r.Targets {
		cellTable(&b, t)
	}

	r.agenticTable(&b)
	r.attention(&b)
	r.problemList(&b)

	return b.String()
}

func (r report) footprintTable(b *strings.Builder) {
	b.WriteString("## Image, startup and idle\n\n")
	fmt.Fprintf(b, "Startup is the median of %d containers. Idle is the median of %d containers, each sampled for "+
		"%.0f s after %.0f s.\n\n",
		r.Settings.StartupRuns, r.Settings.IdleRuns, r.Settings.IdleSeconds, r.Settings.SettleSeconds)
	b.WriteString("| Target | Image | Image size | Startup | Idle RSS | Idle CPU | " +
		"Idle RSS, no HEALTHCHECK | Idle CPU, no HEALTHCHECK |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|\n")

	for _, t := range r.Targets {
		on, off := t.Idle.Healthcheck, t.Idle.NoHealthcheck
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %s | %s | %s |\n", t.Name, t.Image, mib(t.ImageBytes),
			seconds(t.Startup.MS.Median), mib(uint64(on.RSSBytes.Median)), percent(on.CPUPercent.Median, on.Seconds),
			mib(uint64(off.RSSBytes.Median)), percent(off.CPUPercent.Median, off.Seconds))
	}

	fmt.Fprintln(b)
}

// workloadCells groups cells by the workload they belong to, in the order the
// workloads first appear.
func workloadCells(cells []cellResult) ([]cellSpec, [][]cellResult) {
	var (
		heads  []cellSpec
		groups [][]cellResult
	)

	for _, c := range cells {
		i := slices.IndexFunc(heads, func(h cellSpec) bool {
			return h.Service == c.Spec.Service && h.Workload == c.Spec.Workload
		})
		if i < 0 {
			heads = append(heads, c.Spec)
			groups = append(groups, nil)
			i = len(heads) - 1
		}

		groups[i] = append(groups[i], c)
	}

	return heads, groups
}

func cellTable(b *strings.Builder, t targetResult) {
	fmt.Fprintf(b, "## Cells: %s\n\n", t.Name)

	if len(t.Cells) == 0 {
		b.WriteString("No cell was measured.\n\n")

		return
	}

	b.WriteString("Milliseconds, p50 / p99, of each operation of the cell's cycle. Cycles/s is whole cycles " +
		"(a batch cycle moves ten messages).\n\n")

	heads, groups := workloadCells(t.Cells)
	for i, cells := range groups {
		fmt.Fprintf(b, "### %s/%s\n\n", heads[i].Service, heads[i].Workload)

		ops := orderedOps(heads[i].Service, heads[i].Workload, cells)

		b.WriteString("| Cell | " + strings.Join(ops, " | ") + " | Cycles/s | Peak RSS |\n")
		b.WriteString("|---|" + strings.Repeat("---:|", len(ops)+cycleColumns) + "\n")

		for _, c := range cells {
			if c.Problem != "" {
				fmt.Fprintf(b, "| `%s` | %s |%s\n", c.ID, c.Problem, strings.Repeat(" |", len(ops)+cycleColumns-1))

				continue
			}

			fmt.Fprintf(b, "| `%s` |", c.ID)

			for _, op := range ops {
				fmt.Fprintf(b, " %s |", opPair(c.Ops[op]))
			}

			fmt.Fprintf(b, " %.0f | %s |\n", c.CyclesPerSec.Median, mib(uint64(c.PeakRSSBytes.Median)))
		}

		fmt.Fprintln(b)
	}
}

// agenticServices lists the services some target has an agentic result for, and
// the services this program can run one for.
func (r report) agenticServices() []string {
	seen := map[string]bool{}

	for _, w := range agenticWorkloads {
		seen[w.Service()] = true
	}

	for _, t := range r.Targets {
		for service := range t.Agentic {
			seen[service] = true
		}
	}

	return slices.Sorted(maps.Keys(seen))
}

func (r report) agenticTable(b *strings.Builder) {
	b.WriteString("## Agentic workload\n\n")
	fmt.Fprintf(b, "%d cycles of create, use and delete per service, at concurrency %d.\n\n",
		r.Settings.Cycles, r.Settings.CycleConcurrency)
	b.WriteString("| Target | Service | Total time | Cycle p50 / p99 | Errors | RSS before | Peak RSS |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|\n")

	for _, service := range r.agenticServices() {
		for _, t := range r.Targets {
			a := t.Agentic[service]
			if a.Cycles == 0 {
				fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s |\n",
					t.Name, service, noValue, noValue, noValue, noValue, noValue)

				continue
			}

			fmt.Fprintf(b, "| %s | %s | %.2f s | %.1f / %.1f ms | %d | %s | %s |\n", t.Name, service, a.Seconds,
				a.CycleP50ms, a.CycleP99ms, a.Errors, mib(a.Usage.BaselineRSSBytes), mib(a.Usage.PeakRSSBytes))
		}
	}

	fmt.Fprintln(b)
}

// attention lists the cells the task asks to see: where a gostack target is
// slower than LocalStack, and where its p99 is over 50 ms at concurrency 16.
// Both are observations, not failures.
func (r report) attention(b *strings.Builder) {
	var slower, slow []string

	reference, hasReference := r.target(targetLocalstack)

	for _, t := range r.Targets {
		if !strings.HasPrefix(t.Name, targetGostack) {
			continue
		}

		for _, c := range t.Cells {
			if c.Problem != "" {
				continue
			}

			if hasReference {
				if ref, ok := reference.cell(c.ID); ok && ref.Problem == "" {
					if why := slowerThan(c, ref); why != "" {
						slower = append(slower, fmt.Sprintf("- %s `%s`: %s", t.Name, c.ID, why))
					}
				}
			}

			slow = append(slow, slowTail(t.Name, c)...)
		}
	}

	b.WriteString("## Cells where gostack is slower than LocalStack\n\n")
	writeList(b, slower, "none (or LocalStack was not measured)")
	b.WriteString("## Cells with p99 above 50 ms at concurrency 16\n\n")
	writeList(b, slow, "none")
}

// slowTail lists the operations of a cell at concurrency 16 whose p99 is over 50 ms.
func slowTail(target string, c cellResult) []string {
	if c.Spec.Concurrency != attentionAt {
		return nil
	}

	var lines []string

	for _, name := range c.opNames() {
		if p99 := c.Ops[name].P99us.Median; p99 > slowP99us {
			lines = append(lines, fmt.Sprintf("- %s `%s`: %s p99 %.1f ms", target, c.ID, name, p99/microsPerMilli))
		}
	}

	return lines
}

// opNames lists the operations of the cell in report order.
func (c cellResult) opNames() []string {
	return orderedOps(c.Spec.Service, c.Spec.Workload, []cellResult{c})
}

func writeList(b *strings.Builder, lines []string, none string) {
	if len(lines) == 0 {
		b.WriteString(none + "\n\n")

		return
	}

	b.WriteString(strings.Join(lines, "\n") + "\n\n")
}

// slowerThan says how a gostack cell is slower than its LocalStack twin, by
// median: fewer cycles a second, or a higher p50 on some operation. It returns
// "" when it is not.
func slowerThan(c, ref cellResult) string {
	var reasons []string

	if c.CyclesPerSec.Median < ref.CyclesPerSec.Median {
		reasons = append(reasons, fmt.Sprintf("%.0f cycles/s against %.0f", c.CyclesPerSec.Median, ref.CyclesPerSec.Median))
	}

	for _, name := range c.opNames() {
		got, want := c.Ops[name].P50us.Median, ref.Ops[name].P50us.Median
		if got > want && want > 0 {
			reasons = append(reasons, fmt.Sprintf("%s p50 %.2f ms against %.2f", name,
				got/microsPerMilli, want/microsPerMilli))
		}
	}

	return strings.Join(reasons, "; ")
}

func (r report) problemList(b *strings.Builder) {
	wrote := false

	for _, t := range r.Targets {
		for _, p := range t.Problems {
			if !wrote {
				b.WriteString("## Not measured\n\n")

				wrote = true
			}

			fmt.Fprintf(b, "- %s: %s\n", t.Name, p)
		}
	}
}

func mib(bytes uint64) string {
	if bytes == 0 {
		return noValue
	}

	return fmt.Sprintf("%.1f MiB", float64(bytes)/bytesPerMiB)
}

func seconds(ms float64) string {
	if ms == 0 {
		return noValue
	}

	return fmt.Sprintf("%.2f s", ms/msPerSecond)
}

func percent(value, window float64) string {
	if window == 0 {
		return noValue
	}

	return fmt.Sprintf("%.3f%%", value)
}

func opPair(o opSpread) string {
	if o.Count == 0 {
		return noValue
	}

	return fmt.Sprintf("%.2f / %.2f", o.P50us.Median/microsPerMilli, o.P99us.Median/microsPerMilli)
}
