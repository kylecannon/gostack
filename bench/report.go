package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	bytesPerMiB = 1 << 20
	bytesPerGiB = 1 << 30
	msPerSecond = 1000
	dirMode     = 0o750
	fileMode    = 0o600
	noValue     = "n/a"
)

// report is the content of bench/results/<date>-<sha>.json.
type report struct {
	Date     string         `json:"date"`
	Commit   string         `json:"commit"`
	Dirty    bool           `json:"dirty"`
	Host     hostInfo       `json:"host"`
	Settings settings       `json:"settings"`
	Targets  []targetResult `json:"targets"`
}

type hostInfo struct {
	Docker      string `json:"docker"`
	OS          string `json:"os"`
	Kernel      string `json:"kernel"`
	Arch        string `json:"arch"`
	CPUs        int    `json:"cpus"`
	MemoryBytes uint64 `json:"memory_bytes"`
}

type settings struct {
	StartupRuns      int     `json:"startup_runs"`
	SettleSeconds    float64 `json:"settle_seconds"`
	IdleSeconds      float64 `json:"idle_seconds"`
	LoadSeconds      float64 `json:"load_seconds"`
	Concurrencies    []int   `json:"concurrencies"`
	Cycles           int     `json:"cycles"`
	CycleConcurrency int     `json:"cycle_concurrency"`
}

// targetResult holds every measurement of one image. A measurement that could
// not be taken is absent and named in Problems.
type targetResult struct {
	Name       string        `json:"name"`
	Image      string        `json:"image"`
	ImageBytes uint64        `json:"image_bytes"`
	Startup    startupResult `json:"startup"`
	Idle       idleResult    `json:"idle"`
	Latency    []latencyRun  `json:"latency"`
	Agentic    agenticRun    `json:"agentic"`
	Problems   []string      `json:"problems,omitempty"`
}

type startupResult struct {
	SamplesMS []float64 `json:"samples_ms"`
	MedianMS  float64   `json:"median_ms"`
}

type idleResult struct {
	SettleSeconds float64 `json:"settle_seconds"`
	Seconds       float64 `json:"seconds"`
	Usage         usage   `json:"usage"`
}

func (t *targetResult) problem(what string, err error) {
	if err != nil {
		t.Problems = append(t.Problems, what+": "+err.Error())
	}
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

	fmt.Fprintf(&b, "# System benchmark %s, %s (%s)\n\n", r.Date, r.Commit, state)
	fmt.Fprintf(&b, "Host: Docker %s, %s, kernel %s, %s, %d CPUs, %.1f GiB.\n\n",
		r.Host.Docker, r.Host.OS, r.Host.Kernel, r.Host.Arch, r.Host.CPUs, float64(r.Host.MemoryBytes)/bytesPerGiB)
	b.WriteString("Memory is the resident set of the container (anon plus mapped file pages). " +
		"CPU is a percentage of one core.\n\n")

	r.footprintTable(&b)
	r.latencyTable(&b)
	r.agenticTable(&b)
	r.problemList(&b)

	return b.String()
}

func (r report) footprintTable(b *strings.Builder) {
	b.WriteString("## Image, startup and idle\n\n")
	fmt.Fprintf(b, "| Target | Image | Image size | Startup, median of %d | Idle RSS | Idle CPU over %.0fs |\n",
		r.Settings.StartupRuns, r.Settings.IdleSeconds)
	b.WriteString("|---|---|---:|---:|---:|---:|\n")

	for _, t := range r.Targets {
		idle := t.Idle.Usage
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %s |\n", t.Name, t.Image,
			mib(t.ImageBytes), seconds(t.Startup.MedianMS), mib(idle.EndRSSBytes), percent(idle.CPUPercent, t.Idle.Seconds))
	}

	fmt.Fprintln(b)
}

func (r report) latencyTable(b *strings.Builder) {
	fmt.Fprintf(b, "## SQS latency, %.0fs per level\n\n", r.Settings.LoadSeconds)
	b.WriteString("Milliseconds, p50 / p99. Each worker loops SendMessage, ReceiveMessage (one message), " +
		"DeleteMessage on one queue.\n\n")
	b.WriteString("| Target | Concurrency | Send | Receive | Delete | Cycles/s | Peak RSS | CPU |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|\n")

	for _, t := range r.Targets {
		for _, run := range t.Latency {
			fmt.Fprintf(b, "| %s | %d | %s | %s | %s | %.0f | %s | %s |\n", t.Name, run.Concurrency,
				pair(run.Send), pair(run.Receive), pair(run.Delete), run.CyclesPerSec,
				mib(run.Usage.PeakRSSBytes), percent(run.Usage.CPUPercent, run.Seconds))
		}
	}

	fmt.Fprintln(b)
}

func (r report) agenticTable(b *strings.Builder) {
	b.WriteString("## Agentic workload\n\n")
	fmt.Fprintf(b, "%d cycles of create queue, send 10, receive 10, delete queue, at concurrency %d.\n\n",
		r.Settings.Cycles, r.Settings.CycleConcurrency)
	b.WriteString("| Target | Total time | Cycle p50 / p99 | Errors | RSS before | Peak RSS |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|\n")

	for _, t := range r.Targets {
		a := t.Agentic
		if a.Cycles == 0 {
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", t.Name, noValue, noValue, noValue, noValue, noValue)

			continue
		}

		fmt.Fprintf(b, "| %s | %.2f s | %.1f / %.1f ms | %d | %s | %s |\n", t.Name, a.Seconds, a.CycleP50ms, a.CycleP99ms,
			a.Errors, mib(a.Usage.BaselineRSSBytes), mib(a.Usage.PeakRSSBytes))
	}

	fmt.Fprintln(b)
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

func pair(s opStats) string {
	if s.Count == 0 {
		return noValue
	}

	return fmt.Sprintf("%.2f / %.2f", float64(s.P50us)/microsPerMilli, float64(s.P99us)/microsPerMilli)
}
