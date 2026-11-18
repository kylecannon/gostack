package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxRegressionPercent = 10.0
	significance         = 0.05
	columnSeparator      = "│"
	baseMarker           = "vs base"
	geomeanRow           = "geomean"
	unitColumn           = 1 // the text between the first two separators of a header row
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

// gateMain is `bench gate`: it reads benchstat's text output and fails on a
// regression over maxRegressionPercent at p < significance. Empty input is an
// error: a gate that read nothing has not checked anything.
func gateMain(in io.Reader, out io.Writer) error {
	text, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read benchstat output: %w", err)
	}

	if len(bytes.TrimSpace(text)) == 0 {
		return errEmptyInput
	}

	regressions, err := findRegressions(bytes.NewReader(text))
	if err != nil {
		return err
	}

	if len(regressions) == 0 {
		_, err = io.WriteString(out, "no benchmark regressed over 10% at p<0.05\n")

		return wrapWrite(err)
	}

	_, err = io.WriteString(out, "regressions over 10% at p<0.05:\n  "+strings.Join(regressions, "\n  ")+"\n")
	if err != nil {
		return wrapWrite(err)
	}

	return fmt.Errorf("%w: %d over the limit", errRegression, len(regressions))
}

func wrapWrite(err error) error {
	if err != nil {
		return fmt.Errorf("write result: %w", err)
	}

	return nil
}

// findRegressions returns one line per regressed benchmark and unit. Units that
// end in "/s" are throughputs, where a drop is the regression; every other unit
// (sec/op, B/op, allocs/op) is a cost, where a rise is.
func findRegressions(in io.Reader) ([]string, error) {
	var (
		found []string
		unit  string
	)

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := scanner.Text()

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
			found = append(found, fmt.Sprintf("%s %s %+.2f%% (p=%s)", match[1], unit, delta, match[3]))
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
