package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
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
	// alone puts about 34 of them under p<0.05 (688 x 0.05), roughly 17 in
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
	wholePercent    = 100.0
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

// fromZeroRow matches a row whose change is "?" because the base is zero: name,
// the base value, the head value and the p-value.
var fromZeroRow = regexp.MustCompile(
	`^(\S+)\s+(\S+)(?:\s+±\s+\S+)?\s+(\S+)(?:\s+±\s+\S+)?\s+\?\s+\(p=(\d+(?:\.\d+)?)\s+n=`)

// procsSuffix is the -GOMAXPROCS suffix benchstat keeps on a benchmark name.
var procsSuffix = regexp.MustCompile(`^-\d+$`)

// benchRow is one regressed benchstat row.
type benchRow struct {
	pkg, name, unit string
	delta           float64
	pValue          string
	// shape says a change no percent describes, such as a cost that rose from
	// zero, in place of the percent.
	shape string
}

func (r benchRow) String() string {
	if r.shape != "" {
		return fmt.Sprintf("%s %s %s (p=%s)", r.name, r.unit, r.shape, r.pValue)
	}

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
	var (
		held, failed []string
		matchers     = make([]func(*acceptedEntry) bool, 0, len(rows))
	)

	for _, row := range rows {
		matchers = append(matchers, row.matches)
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

	// An unused entry is shown and never fails the gate: it holds nothing now.
	if unused := accepted.unused(gateBenchstat, matchers); len(unused) > 0 {
		fmt.Fprintf(&b, "Unused accepted entries (%d):\n  %s\n", len(unused), strings.Join(unused, "\n  "))
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
// (sec/op, B/op, allocs/op) is a cost, where a rise is. One row carries no
// percent and is a regression all the same: a cost whose base is zero prints
// "?" for its change. A throughput that falls to zero is not found here. Its
// benchmark takes as many times as long for the same work, so its sec/op row
// shows a rise past the 1000% cap that no entry can hold.
func findRegressions(in io.Reader) ([]benchRow, error) {
	var scan benchScan

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		err := scan.line(scanner.Text())
		if err != nil {
			return nil, err
		}
	}

	err := scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("read benchstat output: %w", err)
	}

	return scan.found, nil
}

// benchScan reads benchstat's text a line at a time.
type benchScan struct {
	pkg, unit string
	found     []benchRow
}

func (s *benchScan) line(line string) error {
	if p, ok := strings.CutPrefix(line, pkgPrefix); ok {
		s.pkg = strings.TrimSpace(p)

		return nil
	}

	if strings.Contains(line, baseMarker) && strings.Contains(line, columnSeparator) {
		s.unit = unitOf(line)

		return nil
	}

	return s.comparisonRow(line)
}

// comparisonRow reads a row with a change: a percent, or the "?" of a base that
// is zero.
func (s *benchScan) comparisonRow(line string) error {
	if match := fromZeroRow.FindStringSubmatch(line); match != nil {
		return s.fromZero(match)
	}

	match := comparisonRow.FindStringSubmatch(line)
	if match == nil || match[1] == geomeanRow {
		return nil
	}

	delta, err := strconv.ParseFloat(match[2], 64)
	if err != nil {
		return fmt.Errorf("parse delta %q: %w", match[2], err)
	}

	pValue, err := strconv.ParseFloat(match[3], 64)
	if err != nil {
		return fmt.Errorf("parse p-value %q: %w", match[3], err)
	}

	if worse := regressed(s.unit, delta); worse && pValue < significance {
		s.found = append(s.found, benchRow{pkg: s.pkg, name: match[1], unit: s.unit, delta: delta, pValue: match[3]})
	}

	return nil
}

// fromZero judges a row whose change benchstat prints as "?": the base is zero,
// so no percent exists. A cost that rises from zero regresses by more than any
// bound allows; a throughput that rises from zero improved.
func (s *benchScan) fromZero(match []string) error {
	pValue, err := strconv.ParseFloat(match[4], 64)
	if err != nil {
		return fmt.Errorf("parse p-value %q: %w", match[4], err)
	}

	if !isZero(match[2]) || isZero(match[3]) || pValue >= significance || strings.HasSuffix(s.unit, "/s") {
		return nil
	}

	s.found = append(s.found, benchRow{
		pkg: s.pkg, name: match[1], unit: s.unit, delta: math.Inf(1), pValue: match[4], shape: "rose from zero",
	})

	return nil
}

// isZero reports whether a benchstat value, such as "0.000" or "0.00Mi", is zero.
func isZero(value string) bool {
	number, err := strconv.ParseFloat(strings.TrimRightFunc(value, unicode.IsLetter), 64)

	return err == nil && number == 0
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
