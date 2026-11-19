package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The accepted-regressions list, bench/accepted.yaml. Both gates read it: `bench
// gate` for benchstat rows and `bench compare` for system bench metrics. An
// entry names one metric, the largest change accepted for it, why, and the
// pull request that accepted it.
//
// An entry applies to a comparison only when its commit is in the head and not
// in the base. The commit is one that already carries the cost, on the branch
// that brings it to main. Once main holds that commit, every later base holds
// it too and the entry stops applying, so it cannot hide a later regression of
// the same metric. A commit that never reaches main, as after a squash merge,
// is never in a later head either. When git cannot tell, the entry does not
// apply.

const (
	gateBenchstat = "benchstat"
	gateSystem    = "system"

	// fieldCell is the cell field's name in the list.
	fieldCell = "cell"
)

// errAccepted marks a malformed accepted list.
var errAccepted = errors.New("accepted list")

// commitHash is a full or abbreviated commit id. Anything else, such as a flag,
// never reaches git.
var commitHash = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// acceptedEntry is one entry of the list. A benchstat entry names the package,
// benchmark and unit; a system entry the target, cell and metric.
type acceptedEntry struct {
	Gate string `yaml:"gate"`

	Package   string `yaml:"package"`
	Benchmark string `yaml:"benchmark"`
	Unit      string `yaml:"unit"`

	Target string `yaml:"target"`
	Cell   string `yaml:"cell"`
	Metric string `yaml:"metric"`

	Max    string `yaml:"max"`
	Reason string `yaml:"reason"`
	PR     int    `yaml:"pr"`
	Commit string `yaml:"commit"`

	limit limit
}

// limit is an entry's max: a change in percent, or the worst head value in the
// metric's own unit.
type limit struct {
	percent  float64
	absolute float64
	unit     string
}

// containsFunc reports whether the commit descendant contains the commit
// ancestor.
type containsFunc func(ctx context.Context, ancestor, descendant string) (bool, error)

// acceptedSet is the list one gate run reads, with how it asks git about
// commits. The zero value accepts nothing.
type acceptedSet struct {
	entries  []acceptedEntry
	contains containsFunc
}

// openAccepted reads the list at path. An empty path is an empty list.
func openAccepted(path string) (acceptedSet, error) {
	entries, err := loadAccepted(path)
	if err != nil {
		return acceptedSet{}, err
	}

	return acceptedSet{entries: entries, contains: gitContains}, nil
}

func loadAccepted(path string) ([]acceptedEntry, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is a flag of this tool
	if err != nil {
		return nil, fmt.Errorf("read the accepted list: %w", err)
	}

	return parseAccepted(data, path)
}

func parseAccepted(data []byte, name string) ([]acceptedEntry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var entries []acceptedEntry

	err := dec.Decode(&entries)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errAccepted, name, err)
	}

	for i := range entries {
		err = entries[i].validate()
		if err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", name, i+1, err)
		}
	}

	return entries, nil
}

func (e *acceptedEntry) validate() error {
	own, other := e.fields()
	if own == nil {
		return fmt.Errorf("%w: gate %q is neither %s nor %s", errAccepted, e.Gate, gateBenchstat, gateSystem)
	}

	own["max"], own["reason"], own["commit"] = e.Max, e.Reason, e.Commit

	for name, value := range own {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s entry has no %s", errAccepted, e.Gate, name)
		}
	}

	for name, value := range other {
		if value != "" {
			return fmt.Errorf("%w: a %s entry has no %s field", errAccepted, e.Gate, name)
		}
	}

	switch {
	case e.PR <= 0:
		return fmt.Errorf("%w: entry has no pr", errAccepted)
	case strings.ContainsAny(e.Reason, "\r\n"):
		return fmt.Errorf("%w: the reason is one line", errAccepted)
	case !commitHash.MatchString(e.Commit):
		return fmt.Errorf("%w: commit %q is not a commit hash", errAccepted, e.Commit)
	}

	var err error

	e.limit, err = parseLimit(e.Max, e.Gate)

	return err
}

// fields returns the fields the entry's gate needs and those it must leave out.
func (e *acceptedEntry) fields() (own, other map[string]string) {
	benchstat := map[string]string{"package": e.Package, "benchmark": e.Benchmark, "unit": e.Unit}
	system := map[string]string{"target": e.Target, fieldCell: e.Cell, "metric": e.Metric}

	switch e.Gate {
	case gateBenchstat:
		return benchstat, system
	case gateSystem:
		return system, benchstat
	default:
		return nil, nil
	}
}

// parseLimit reads "+50%" (the sign is the regression's direction and only the
// size counts) or, for a system metric, "6 MiB": the worst head median accepted.
func parseLimit(raw, gate string) (limit, error) {
	text := strings.TrimSpace(raw)

	if number, ok := strings.CutSuffix(text, "%"); ok {
		pct, err := strconv.ParseFloat(strings.TrimLeft(number, "+-"), 64)
		if err != nil || pct <= 0 {
			return limit{}, fmt.Errorf("%w: max %q is not a positive percent", errAccepted, raw)
		}

		return limit{percent: pct}, nil
	}

	fields := strings.Fields(text)
	if gate != gateSystem || len(fields) != 2 {
		return limit{}, fmt.Errorf("%w: max %q is not a percent such as +50%%, or for a system metric a value "+
			"and its unit such as 6 MiB", errAccepted, raw)
	}

	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || value <= 0 {
		return limit{}, fmt.Errorf("%w: max %q is not a positive value", errAccepted, raw)
	}

	return limit{absolute: value, unit: fields[1]}, nil
}

// label names the entry in the gates' output.
func (e *acceptedEntry) label() string {
	return fmt.Sprintf("%s (PR #%d, %s)", e.Reason, e.PR, short(e.Commit))
}

// verdict is what the list says of one regression: the entry that accepts it,
// or why the entries that name it do not.
type verdict struct {
	entry *acceptedEntry
	note  string
}

// judge looks for an entry that matches, applies to base..head and holds the
// change. within returns "" when the change is inside the entry's limit, and
// otherwise says how it is not.
func (s acceptedSet) judge(ctx context.Context, base, head string, matches func(*acceptedEntry) bool,
	within func(limit) string,
) verdict {
	var notes []string

	for i := range s.entries {
		e := &s.entries[i]
		if !matches(e) {
			continue
		}

		why := s.outOfScope(ctx, e, base, head)
		if why == "" {
			why = within(e.limit)
		}

		if why == "" {
			return verdict{entry: e}
		}

		notes = append(notes, "accepted list: "+why)
	}

	return verdict{note: strings.Join(notes, "; ")}
}

// outOfScope returns "" when the entry applies to a comparison of base and head,
// and otherwise why it does not.
func (s acceptedSet) outOfScope(ctx context.Context, e *acceptedEntry, base, head string) string {
	for _, c := range []string{base, head} {
		if !commitHash.MatchString(c) {
			return fmt.Sprintf("listed for %s, but the compared commit %q is not a commit hash", short(e.Commit), c)
		}
	}

	inHead, err := s.contains(ctx, e.Commit, head)
	if err != nil {
		return fmt.Sprintf("listed for %s, but git cannot tell whether the head holds it: %v", short(e.Commit), err)
	}

	if !inHead {
		return fmt.Sprintf("listed for %s, which the head %s does not hold", short(e.Commit), short(head))
	}

	inBase, err := s.contains(ctx, e.Commit, base)
	if err != nil {
		return fmt.Sprintf("listed for %s, but git cannot tell whether the base holds it: %v", short(e.Commit), err)
	}

	if inBase {
		return fmt.Sprintf("listed for %s, which the base %s already holds, so the entry has expired",
			short(e.Commit), short(base))
	}

	return ""
}

func (l limit) String() string {
	if l.percent > 0 {
		return fmt.Sprintf("%g%%", l.percent)
	}

	return fmt.Sprintf("%g %s", l.absolute, l.unit)
}

func short(commit string) string {
	const shortLen = 7

	return commit[:min(len(commit), shortLen)]
}

// gitContains asks git whether descendant contains ancestor. A shallow clone
// cannot answer: its history ends early, so a base would look like it lacks a
// commit it holds.
func gitContains(ctx context.Context, ancestor, descendant string) (bool, error) {
	shallow, err := exec.CommandContext(ctx, "git", "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse: %w", err)
	}

	if strings.TrimSpace(string(shallow)) != "false" {
		return false, errors.New("the checkout is shallow; fetch its full history")
	}

	var stderr bytes.Buffer

	//nolint:gosec // both commits passed the commitHash check, so neither is a flag
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err == nil {
		return true, nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}

	return false, fmt.Errorf("git merge-base: %w: %s", err, strings.TrimSpace(stderr.String()))
}

// measured is one regressed metric as an accepted limit reads it.
type measured struct {
	// change is the head against the base, in percent.
	change float64
	// head is the head's value in unit, the unit an absolute limit is written in.
	head float64
	unit string
	// lowerIsBetter is true for a cost, false for a throughput.
	lowerIsBetter bool
}

// within returns "" when the change is inside the limit, and otherwise says how
// it is not.
func (m measured) within(l limit) string {
	switch {
	case l.percent > 0 && m.worsening() <= l.percent:
		return ""
	case l.percent > 0:
		return fmt.Sprintf("the change is past the accepted %s", l)
	case l.unit != m.unit:
		return fmt.Sprintf("the accepted %s is not in %s, the metric's unit", l, m.unit)
	case m.headWithin(l.absolute):
		return ""
	default:
		return fmt.Sprintf("the head is past the accepted %s", l)
	}
}

// worsening is how much worse the change is, in percent: a rise for a cost, a
// fall for a throughput.
func (m measured) worsening() float64 {
	if m.lowerIsBetter {
		return m.change
	}

	return -m.change
}

// headWithin reports whether the head is no worse than an absolute limit.
func (m measured) headWithin(value float64) bool {
	const epsilon = 1e-9

	if m.lowerIsBetter {
		return m.head <= value+epsilon
	}

	return m.head >= value-epsilon
}
