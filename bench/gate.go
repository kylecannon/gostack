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
	minRateFields   = 2 // a benchmark's name and its value
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
	switch {
	case r.shape != "" && r.pValue == "":
		return fmt.Sprintf("%s %s %s", r.name, r.unit, r.shape)
	case r.shape != "":
		return fmt.Sprintf("%s %s %s (p=%s)", r.name, r.unit, r.shape, r.pValue)
	default:
		return fmt.Sprintf("%s %s %+.2f%% (p=%s)", r.name, r.unit, r.delta, r.pValue)
	}
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
// (sec/op, B/op, allocs/op) is a cost, where a rise is. Two rows carry no
// percent and are regressions all the same: a cost whose base is zero prints
// "?" for its change, and a throughput that falls to zero moves to a table of
// its own, because benchstat names the unit of a zero MB/s differently.
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

	return append(scan.found, scan.fellToZero()...), nil
}

// benchScan reads benchstat's text a line at a time.
type benchScan struct {
	pkg, unit string
	// header counts the header lines of the table being read: file labels, then
	// units.
	header int
	labels []string
	// baseLabel is the label of the base file, and single the label of the file a
	// table with one file column holds, or "" outside such a table.
	baseLabel, single string
	found             []benchRow
	// baseRates and headRates are the throughput rows of the tables with one file
	// column, by package and benchmark.
	baseRates, headRates []rateRow
}

// rateRow is a throughput row of a table that compares nothing.
type rateRow struct {
	pkg, name, unit string
	zero            bool
}

func (s *benchScan) line(line string) error {
	trimmed := strings.TrimSpace(line)

	switch {
	case trimmed == "":
		s.header, s.single = 0, ""
	case strings.HasPrefix(line, pkgPrefix):
		s.pkg = strings.TrimSpace(strings.TrimPrefix(line, pkgPrefix))
	case strings.HasPrefix(trimmed, columnSeparator):
		s.headerLine(line)
	case s.single != "":
		s.rateRow(line)
	default:
		return s.comparisonRow(line)
	}

	return nil
}

// headerLine reads a header row of a table: the labels of its files, then the
// units. A table of one file holds benchmarks the other file has no row for.
func (s *benchScan) headerLine(line string) {
	s.header++
	cells := columnsOf(line)

	switch {
	case strings.Contains(line, baseMarker):
		s.unit, s.single = unitOf(line), ""
		if len(s.labels) > 1 && s.baseLabel == "" {
			s.baseLabel = s.labels[0]
		}
	case s.header == 1:
		s.labels = cells
	case len(s.labels) == 1 && len(cells) == 1:
		s.unit, s.single = cells[0], s.labels[0]
		if s.baseLabel == "" {
			s.baseLabel = s.single
		}
	default:
	}
}

// rateRow keeps a throughput row of a table of one file.
func (s *benchScan) rateRow(line string) {
	fields := strings.Fields(line)
	if len(fields) < minRateFields || fields[0] == geomeanRow || !strings.HasSuffix(s.unit, "/s") {
		return
	}

	row := rateRow{pkg: s.pkg, name: fields[0], unit: s.unit, zero: isZero(fields[1])}
	if s.single == s.baseLabel {
		s.baseRates = append(s.baseRates, row)
	} else {
		s.headRates = append(s.headRates, row)
	}
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

// fellToZero returns the throughputs whose head is zero where the base was not.
// benchstat does not compare them: a zero MB/s is a different unit from the
// base's B/s, so each file gets a table with one column.
func (s *benchScan) fellToZero() []benchRow {
	var rows []benchRow

	for _, head := range s.headRates {
		if !head.zero {
			continue
		}

		for _, base := range s.baseRates {
			if base.pkg == head.pkg && base.name == head.name && !base.zero {
				rows = append(rows, benchRow{
					pkg: base.pkg, name: base.name, unit: base.unit, delta: -wholePercent, shape: "fell to zero",
				})
			}
		}
	}

	return rows
}

// isZero reports whether a benchstat value, such as "0.000" or "0.00Mi", is zero.
func isZero(value string) bool {
	number, err := strconv.ParseFloat(strings.TrimRightFunc(value, unicode.IsLetter), 64)

	return err == nil && number == 0
}

// columnsOf is the text of each column of a header row.
func columnsOf(header string) []string {
	var cells []string

	for part := range strings.SplitSeq(header, columnSeparator) {
		if text := strings.TrimSpace(part); text != "" {
			cells = append(cells, text)
		}
	}

	return cells
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
