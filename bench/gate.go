package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxRegressionPercent = 10.0
	// significance is the p-value a row has to beat to be flagged. benchstat
	// prints p to three decimals, so the gate flags a row only when it
	// prints p=0.000, which is p<0.0005.
	//
	// The gate makes one comparison per benchmark row and unit: 688 in PR
	// #7's run (CI run 44524892109: 14 packages; sec/op, B/op, allocs/op,
	// B/s and custom units). With identical code on both sides, chance
	// alone puts about 34 of them under p<0.001 (688 x 0.05), roughly 17 in
	// the worse direction. A noisy runner that also moves one of those over
	// 10% flags unchanged code: PR #7 at p=0.023 (SQS
	// ReceiveMessage/proto=query/queue=standard/max=10/payload=1MiB, sec/op
	// +11.28%) and PR #6 at p=0.018 (SQS Snapshot/queues=1/messages=100000,
	// lock-ms +16.93%). At p<0.0005 the same 688 comparisons give 688 x
	// 0.0005 = 0.34 chance crossings in either direction, about 0.17 in the
	// worse one, before the 10% filter.
	//
	// n=10 samples a side still reaches it: the smallest p a Mann-Whitney
	// test gives is 2/C(20,10) = 1.08e-5, so a regression that separates
	// the two sides is far inside the threshold. PR #4's RouteDetect/host
	// (+26.83%) printed p=0.000.
	significance    = 0.001
	columnSeparator = "│"
	baseMarker      = "vs base"
	geomeanRow      = "geomean"
	pkgPrefix       = "pkg: "
	unitColumn      = 1 // the text between the first two separators of a header row
)

// errRegression is returned when a benchmark got slower, or hungrier, than the
// gate allows.
var errRegression = errors.New("benchmark regression")

// errEmptyInput is returned for an empty or missing benchstat file. Nothing was
// compared, so the gate cannot say nothing regressed.
var errEmptyInput = errors.New("gate: benchstat output is empty, so nothing was compared")

// comparisonRow matches a benchstat row that carries a delta: name, the two
// columns, the delta and its p-value. A row without a significant change prints
// "~" and does not match.
var comparisonRow = regexp.MustCompile(`^(\S+)\s.*?([+-]\d+(?:\.\d+)?)%\s+\(p=(\d+(?:\.\d+)?)\s+n=`)

// procsSuffix is the -GOMAXPROCS suffix benchstat keeps on a benchmark name.
var procsSuffix = regexp.MustCompile(`^-\d+$`)

// benchRow is one regressed benchstat row.
type benchRow struct {
	pkg, name, unit string
	delta           float64
	pValue          string
}

func (r benchRow) String() string {
	return fmt.Sprintf("%s %s %+.2f%% (p=%s)", r.name, r.unit, r.delta, r.pValue)
}

func (r benchRow) lowerIsBetter() bool {
	return !strings.HasSuffix(r.unit, "/s")
}

// matches reports whether a benchstat entry names this row. The entry leaves
// out the -GOMAXPROCS suffix, so it holds on a runner with any CPU count, and
// names the package by its import path or its path below the module.
//
// benchstat's output does not say what GOMAXPROCS was: a benchmark's name only
// carries the "-N" when N is above 1. A row that is "host-1024" on a 1-CPU run
// therefore reads the same as "host" on a 1024-CPU run, and the entry for
// "host" takes both. Telling them apart would take the run's GOMAXPROCS, which
// the gate is not given. A runner with 2 or more CPUs is not affected: its rows
// end in "-N" after any such number.
func (r benchRow) matches(e *acceptedEntry) bool {
	if e.Gate != gateBenchstat || e.Unit != r.unit || !e.namesPackage(r.pkg) {
		return false
	}

	return namesBenchmark(r.name, e.Benchmark)
}

// gateMain is `bench gate`: it reads benchstat's text output and fails on a
// regression over maxRegressionPercent at p < significance that the accepted
// list (-accepted, for the commits -base and -head) does not hold. Empty input
// is an error: a gate that read nothing has not checked anything.
func gateMain(ctx context.Context, args []string, in io.Reader, out io.Writer, contains containsFunc) error {
	flags := flag.NewFlagSet("gate", flag.ContinueOnError)
	acceptedPath := flags.String("accepted", "", "the accepted-regressions list, such as bench/accepted.yaml")
	base := flags.String("base", "", "the commit benchstat's base column was measured on")
	head := flags.String("head", "", "the commit benchstat's head column was measured on")

	err := flags.Parse(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	accepted, err := openAccepted(*acceptedPath)
	if err != nil {
		return err
	}

	if contains != nil {
		accepted.contains = contains
	}

	text, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read benchstat output: %w", err)
	}

	if len(bytes.TrimSpace(text)) == 0 {
		return errEmptyInput
	}

	rows, err := findRegressions(bytes.NewReader(text))
	if err != nil {
		return err
	}

	return reportGate(ctx, rows, accepted, *base, *head, out)
}

// reportGate splits the regressed rows into those the list accepts and the
// rest, writes both, and fails on the rest.
func reportGate(ctx context.Context, rows []benchRow, accepted acceptedSet, base, head string, out io.Writer) error {
	var held, failed []string

	for _, row := range rows {
		v := accepted.judge(ctx, base, head, row.matches, measured{
			change: row.delta, unit: row.unit, lowerIsBetter: row.lowerIsBetter(),
		}.within)

		switch {
		case v.entry != nil:
			held = append(held, fmt.Sprintf("%s, within %s: %s", row, v.entry.limit, v.entry.label()))
		case v.note != "":
			failed = append(failed, row.String()+"; "+v.note)
		default:
			failed = append(failed, row.String())
		}
	}

	var b strings.Builder

	if len(held) > 0 {
		fmt.Fprintf(&b, "Accepted (%d):\n  %s\n", len(held), strings.Join(held, "\n  "))
	}

	if len(failed) == 0 {
		b.WriteString("no benchmark regressed over 10% at p<0.001 outside the accepted list\n")

		return write(out, b.String())
	}

	b.WriteString("regressions over 10% at p<0.001:\n  " + strings.Join(failed, "\n  ") + "\n")

	err := write(out, b.String())
	if err != nil {
		return err
	}

	return fmt.Errorf("%w: %d over the limit", errRegression, len(failed))
}

// findRegressions returns each regressed benchmark and unit. Units that end in
// "/s" are throughputs, where a drop is the regression; every other unit
// (sec/op, B/op, allocs/op) is a cost, where a rise is.
func findRegressions(in io.Reader) ([]benchRow, error) {
	var (
		found     []benchRow
		pkg, unit string
	)

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := scanner.Text()

		if p, ok := strings.CutPrefix(line, pkgPrefix); ok {
			pkg = strings.TrimSpace(p)

			continue
		}

		if strings.Contains(line, baseMarker) && strings.Contains(line, columnSeparator) {
			unit = unitOf(line)

			continue
		}

		match := comparisonRow.FindStringSubmatch(line)
		if match == nil || match[1] == geomeanRow {
			continue
		}

		delta, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			return nil, fmt.Errorf("parse delta %q: %w", match[2], err)
		}

		pValue, err := strconv.ParseFloat(match[3], 64)
		if err != nil {
			return nil, fmt.Errorf("parse p-value %q: %w", match[3], err)
		}

		if worse := regressed(unit, delta); worse && pValue < significance {
			found = append(found, benchRow{pkg: pkg, name: match[1], unit: unit, delta: delta, pValue: match[3]})
		}
	}

	err := scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("read benchstat output: %w", err)
	}

	return found, nil
}

// unitOf reads the unit from a header row such as "│ sec/op │ sec/op vs base │".
func unitOf(header string) string {
	parts := strings.Split(header, columnSeparator)
	if len(parts) <= unitColumn {
		return ""
	}

	return strings.TrimSpace(parts[unitColumn])
}

func regressed(unit string, delta float64) bool {
	if strings.HasSuffix(unit, "/s") {
		return delta < -maxRegressionPercent
	}

	return delta > maxRegressionPercent
}
