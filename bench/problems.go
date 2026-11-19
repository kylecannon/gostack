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
//     memory, an agentic workload that did not run.
//
// A problem the head has and the base does not fails the comparison. A problem
// on both sides is listed and does not fail, whatever the two counts: the rule
// is that the head did not get worse in kind, and a count of operations is not
// comparable across runs of different length. The committed results hold no
// operation error or problem on any target, LocalStack included, so a problem
// today is a new one. LocalStack never gates, as for the metrics.

// errRunProblem is the answer when the head has a problem the base does not.
var errRunProblem = errors.New("benchmark run problem")

// problem is one row of the problems table: what a side reported, on both sides.
type problem struct {
	target, cell, what string
	base, head         string
	// fails is set when the head has the problem and the base does not.
	fails bool
}

// compareProblems lists the problems of one gated target in either side.
func compareProblems(base, head targetResult) []problem {
	var out []problem

	for _, bc := range base.Cells {
		hc, ok := head.cell(bc.ID)
		if ok {
			out = append(out, cellProblems(base.Name, bc, hc)...)
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

// cellProblems compares what the two sides of one cell reported.
func cellProblems(target string, base, head cellResult) []problem {
	if head.Problem != "" {
		return []problem{{
			target: target, cell: head.ID, what: "no completed run", base: runsText(base),
			head: runsText(head) + ": " + head.Problem, fails: base.Problem == "",
		}}
	}

	// A base with no run is listed under "Not compared" already.
	if base.Problem != "" {
		return nil
	}

	var out []problem

	if head.Runs != base.Runs || len(head.FailedRuns) > 0 || len(base.FailedRuns) > 0 {
		out = append(out, problem{
			target: target, cell: head.ID, what: "runs", base: runsText(base), head: runsText(head),
			fails: len(head.FailedRuns) > 0 && len(base.FailedRuns) == 0,
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
				fails: h > 0 && b == 0,
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
				head: strconv.Itoa(h), fails: h > 0 && b == 0,
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

// ownProblems compares the measurements of the target that could not be taken.
func ownProblems(base, head targetResult) []problem {
	baseLines, headLines := ownLines(base), ownLines(head)
	out := make([]problem, 0, len(headLines))

	for _, line := range headLines {
		out = append(out, problem{
			target: base.Name, cell: "(target)", what: "not measured", base: fmt.Sprintf("%d problems", len(baseLines)),
			head: line, fails: len(baseLines) == 0,
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

	b.WriteString("A problem the head has and the base does not fails the comparison. A problem on both sides is " +
		"listed and does not.\n\n")
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
