package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The second rule of `bench compare`, beside the metric limits in compare.go: a
// head that errored or lost runs where the base did not has not been measured
// the way the base was, and a faster or slower number from it means nothing.
//
// A problem is one of:
//
//   - a cell with no completed run (cellResult.Problem);
//   - a cell with runs that failed, such as a target container that was gone
//     when its run ended (cellResult.FailedRuns), or that has a different
//     number of runs than the other side;
//   - an operation that reported errors (opSpread.Errors), or an agentic
//     workload whose cycles did (agenticRun.Errors);
//   - a measurement of the target that could not be taken (the Problems lines
//     that do not start with cellProblemPrefix): the image size, startup, idle
//     memory, an agentic workload that did not run;
//   - a gated target, or a cell with a completed run, that the base has and the
//     head lacks.
//
// The head fails the comparison when it has:
//
//   - any problem the base does not have: an operation with errors where the
//     base has none, a cell with failed runs where the base has none, a cell
//     with no completed run where the base has runs, a kind of target problem
//     (the text before the first ":" of its line) the base lacks, or a problem
//     in a cell the base does not have at all;
//   - more of a problem the base has too: failed runs in a cell above the base's,
//     or operation errors in a cell and operation that are at least
//     minWorseErrors and more than worseErrors times the base's;
//   - a cell, or a gated target, that the base has and the head does not,
//     when both are results of one profile, the head selected the service and
//     the head's own matrix still has the cell.
//
// Any other problem on both sides is listed and does not fail. Zero tolerance
// for a head-only error is deliberate: the committed results hold none in over
// 900,000 operations, a stray error costs one rerun, and a floor would let a
// rare race through. LocalStack never gates, as for the metrics.

const (
	// worseErrors is how many times the base's operation errors the head's must
	// exceed to fail when the base has errors too.
	worseErrors = 10
	// minWorseErrors is the fewest operation errors the head needs for that.
	minWorseErrors = 10
)

// errRunProblem is the answer when the head has a problem the base does not.
var errRunProblem = errors.New("benchmark run problem")

// problem is one row of the problems table: what a side reported, on both sides.
type problem struct {
	target, cell, what string
	base, head         string
	// fails is set when the head has the problem and the base does not, or the
	// head has far more of it.
	fails bool
}

// expectation says which cells and targets of the base the head was asked to
// measure, and so are missing from it when it has none.
type expectation struct {
	// same is true when both results are of one profile. A quick run against a
	// full result was never asked for most of the base's cells.
	same bool
	// matrix holds the ids of every cell of the head's profile that a gated
	// target runs, for every service. compare runs from the head's checkout, so
	// this is the head's matrix: a base cell it lacks was removed on purpose.
	matrix map[string]bool
	// services are the services the head selected, or none for all of them.
	services []string
	// baseReps and headReps are the repetitions each result asked of a cell.
	baseReps, headReps int
}

func expectationOf(base, head report) expectation {
	want := expectation{
		same: base.Profile == head.Profile, matrix: map[string]bool{}, services: head.Settings.Services,
		baseReps: base.Settings.Reps, headReps: head.Settings.Reps,
	}

	// Both gated targets persist, so one matrix serves them.
	for _, c := range cellsFor(head.Profile, target{persistable: true}) {
		want.matrix[c.id()] = true
	}

	return want
}

// covers reports whether the head was asked to measure the base's cell.
func (e expectation) covers(c cellResult) bool {
	return e.same && e.matrix[c.ID] && (len(e.services) == 0 || slices.Contains(e.services, c.Spec.Service))
}

// removed reports whether the head's matrix no longer has the base's cell.
func (e expectation) removed(c cellResult) bool {
	return e.same && !e.matrix[c.ID]
}

// errorsWorse reports whether the head's count of an operation's errors fails
// against the base's: any at all where the base has none, otherwise far more.
func errorsWorse(base, head int) bool {
	if base == 0 {
		return head > 0
	}

	return head >= minWorseErrors && head > worseErrors*base
}

// compareProblems lists the problems of one gated target in either side.
func compareProblems(base, head targetResult, want expectation) []problem {
	var out []problem

	for _, bc := range base.Cells {
		hc, ok := head.cell(bc.ID)

		switch {
		case ok:
			out = append(out, cellProblems(base.Name, &bc, hc, want)...)
		case bc.Problem == "" && want.covers(bc):
			out = append(out, problem{
				target: base.Name, cell: bc.ID, what: "missing from the head", base: runsText(bc), head: "absent", fails: true,
			})
		default:
		}
	}

	for _, hc := range head.Cells {
		if _, ok := base.cell(hc.ID); !ok {
			out = append(out, cellProblems(base.Name, nil, hc, want)...)
		}
	}

	out = append(out, agenticProblems(base, head)...)

	return append(out, ownProblems(base, head)...)
}

// runsText says how many runs of a cell completed and how many did not.
func runsText(c cellResult) string {
	if len(c.FailedRuns) == 0 {
		return fmt.Sprintf("%d runs", c.Runs)
	}

	return fmt.Sprintf("%d runs, %d failed", c.Runs, len(c.FailedRuns))
}

// failedCount is how many runs of a cell failed. A cell with no completed run
// has failed every run its result asked for (reps), and an older result did not
// name them.
func failedCount(c cellResult, reps int) int {
	if c.Problem != "" {
		return max(len(c.FailedRuns), reps, 1)
	}

	return len(c.FailedRuns)
}

// cellProblems compares what the two sides of one cell reported. A head cell
// the base does not have is compared with none (base nil), so any problem in it
// is the head's alone. A base cell with no completed run has no
// figures, but what it reported still counts: the head's errors there fail
// unless they are of a kind the base's problem already was.
func cellProblems(target string, baseCell *cellResult, head cellResult, want expectation) []problem {
	var base cellResult

	baseRuns := "not in the base"

	if baseCell != nil {
		base = *baseCell
		baseRuns = runsText(base)
	}

	if head.Problem != "" {
		return []problem{{
			target: target, cell: head.ID, what: "no completed run", base: baseRuns,
			head: runsText(head) + ": " + head.Problem, fails: baseCell == nil || base.Problem == "",
		}}
	}

	var out []problem

	failedBase, failedHead := failedCount(base, want.baseReps), failedCount(head, want.headReps)
	baseRan := baseCell != nil && base.Problem == ""

	if failedHead > 0 || (baseRan && (failedBase > 0 || head.Runs != base.Runs)) {
		out = append(out, problem{
			target: target, cell: head.ID, what: "runs", base: baseRuns, head: runsText(head), fails: failedHead > failedBase,
		})
	}

	names := map[string]bool{}

	for name := range base.Ops {
		names[name] = true
	}

	for name := range head.Ops {
		names[name] = true
	}

	for _, name := range slices.Sorted(maps.Keys(names)) {
		b, h := base.Ops[name].Errors, head.Ops[name].Errors
		if b > 0 || h > 0 {
			out = append(out, problem{
				target: target, cell: head.ID, what: name + " errors", base: strconv.Itoa(b), head: strconv.Itoa(h),
				fails: errorsWorse(b, h),
			})
		}
	}

	return out
}

// agenticProblems compares the errors of each service's agentic workload.
func agenticProblems(base, head targetResult) []problem {
	services := map[string]bool{}

	for service := range base.Agentic {
		services[service] = true
	}

	for service := range head.Agentic {
		services[service] = true
	}

	var out []problem

	for _, service := range slices.Sorted(maps.Keys(services)) {
		b, h := base.Agentic[service].Errors, head.Agentic[service].Errors
		if b > 0 || h > 0 {
			out = append(out, problem{
				target: base.Name, cell: agenticCell(service), what: "cycle errors", base: strconv.Itoa(b),
				head: strconv.Itoa(h), fails: errorsWorse(b, h),
			})
		}
	}

	return out
}

// ownLines are the problems of the target itself: the Problems lines that are not
// about a cell or an agentic workload.
func ownLines(t targetResult) []string {
	return slices.DeleteFunc(slices.Clone(t.Problems), func(line string) bool {
		return strings.HasPrefix(line, cellProblemPrefix)
	})
}

// problemKind is the text of a Problems line before its first ":": the
// measurement that could not be taken, whatever the error said.
func problemKind(line string) string {
	kind, _, _ := strings.Cut(line, ":")

	return kind
}

// ownProblems compares the measurements of the target that could not be taken,
// by kind: the head fails for a kind the base has no line of.
func ownProblems(base, head targetResult) []problem {
	baseLines := map[string]string{}

	for _, line := range ownLines(base) {
		if _, ok := baseLines[problemKind(line)]; !ok {
			baseLines[problemKind(line)] = line
		}
	}

	headLines := ownLines(head)
	out := make([]problem, 0, len(headLines))

	for _, line := range headLines {
		baseLine, ok := baseLines[problemKind(line)]
		if !ok {
			baseLine = "none"
		}

		out = append(out, problem{
			target: base.Name, cell: "(target)", what: "not measured", base: baseLine, head: line, fails: !ok,
		})
	}

	return out
}

// failing counts the problems that fail the comparison.
func failing(problems []problem) int {
	n := 0

	for _, p := range problems {
		if p.fails {
			n++
		}
	}

	return n
}

// problemTable writes the problems of a comparison.
func problemTable(b *strings.Builder, problems []problem) {
	fmt.Fprintf(b, "### Errors and failed runs (%d, %d failing)\n\n", len(problems), failing(problems))

	if len(problems) == 0 {
		b.WriteString("None.\n\n")

		return
	}

	b.WriteString("A problem the head has and the base does not fails the comparison, and so does a head far worse " +
		"than a base that has it too, or a cell or target the head lacks. Any other problem on both sides is listed " +
		"and does not.\n\n")
	b.WriteString("| Target | Cell | What | Base | Head | Result |\n|---|---|---|---:|---|---|\n")

	// The failing rows first, so that the cap never hides one.
	problems = slices.SortedStableFunc(slices.Values(problems), func(a, b problem) int {
		switch {
		case a.fails == b.fails:
			return 0
		case a.fails:
			return -1
		default:
			return 1
		}
	})

	for _, p := range problems[:min(len(problems), maxListed)] {
		result := "listed"
		if p.fails {
			result = "**fails**"
		}

		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %s |\n", p.target, p.cell, p.what, p.base, p.head, result)
	}

	if len(problems) > maxListed {
		fmt.Fprintf(b, "\n%d more not listed.\n", len(problems)-maxListed)
	}

	b.WriteString("\n")
}
