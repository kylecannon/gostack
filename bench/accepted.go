package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"

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

	// The list is one document. Entries after a second --- would be dropped
	// unread, so the file would hold entries the gate never applies.
	var extra yaml.Node

	err = dec.Decode(&extra)
	if err == nil {
		return nil, fmt.Errorf("%w %s: a second YAML document follows the list; it is one document", errAccepted, name)
	}

	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w %s: %w", errAccepted, name, err)
	}

	for i := range entries {
		err = entries[i].validate()
		if err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", name, i+1, err)
		}

		for j := range i {
			if entries[j].overlaps(&entries[i]) {
				return nil, fmt.Errorf("%s entry %d: %w: it names the same metric as entry %d (%s), or one whose rows it "+
					"also matches; one entry holds a metric, whatever its bound or commit",
					name, i+1, errAccepted, j+1, entries[i].subject())
			}
		}
	}

	return entries, nil
}

// overlaps reports whether the entries can match one row, which the gates would
// resolve by taking whichever accepts the change. Two benchstat entries overlap
// when their benchmark names match a common row: "host" and "host-2" do (the
// row "host-2" is "host" on a 2-CPU run, and the benchmark "host-2" on a 1-CPU
// one), "host" and "hostile" do not. Names compare as benchRow.matches compares
// them: exactly, apart from the -GOMAXPROCS suffix.
func (e *acceptedEntry) overlaps(o *acceptedEntry) bool {
	if e.Gate != o.Gate {
		return false
	}

	if e.Gate == gateBenchstat {
		return e.Unit == o.Unit && e.fullPackage() == o.fullPackage() &&
			(namesBenchmark(e.Benchmark, o.Benchmark) || namesBenchmark(o.Benchmark, e.Benchmark))
	}

	return e.Target == o.Target && e.Cell == o.Cell && e.Metric == o.Metric
}

// namesBenchmark reports whether a row named row belongs to the benchmark bench.
// An entry leaves out the -GOMAXPROCS suffix, so it holds on a runner with any
// CPU count.
func namesBenchmark(row, bench string) bool {
	rest, ok := strings.CutPrefix(row, bench)

	return ok && (rest == "" || procsSuffix.MatchString(rest))
}

// subject names the metric an entry holds, for an error.
func (e *acceptedEntry) subject() string {
	if e.Gate == gateBenchstat {
		return fmt.Sprintf("benchstat %s %s %s", e.Package, e.Benchmark, e.Unit)
	}

	return fmt.Sprintf("system %s %s %s", e.Target, e.Cell, e.Metric)
}

// modulePath is the path of the module this tool is built in. An entry's package
// is written relative to it.
var modulePath = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	return info.Main.Path
})

// fullPackage is the entry's package as an import path. A package already
// inside the module is its own full path.
func (e *acceptedEntry) fullPackage() string {
	module := modulePath()
	if module == "" || strings.HasPrefix(e.Package, module+"/") {
		return e.Package
	}

	return module + "/" + e.Package
}

// namesPackage reports whether the entry's package is pkg: either pkg's full
// import path, or its path below the module. A trailing part of a path that
// does not start at the module root, such as the internal/route of another
// module nested in this one, names a different package.
func (e *acceptedEntry) namesPackage(pkg string) bool {
	return pkg == e.Package || pkg == e.fullPackage()
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

// maxPercent is the largest percent bound. A bound past it is a typo or a way
// to hold every regression, so it is a load error.
const maxPercent = 1000

// unitCaps are the units a system metric is written in (the unit field of the
// metrics `bench compare` builds) and the largest absolute bound for each: 64 GiB
// of memory, an hour, and 10 million cycles a second. A bound above its cap is
// not a value the metric takes on any machine this runs on, so it is a load
// error. docs/standards.md and the header of bench/accepted.yaml state them.
var unitCaps = map[string]float64{
	"MiB":      capMiB,
	"ms":       capMs,
	"s":        capSeconds,
	"cycles/s": capCyclesPerSec,
}

const (
	mibPerGiB = 1 << 10

	capMiB          = 64 * mibPerGiB       // 64 GiB
	capMs           = secondsPerHour * 1e3 // an hour
	capSeconds      = secondsPerHour
	capCyclesPerSec = 1e7

	secondsPerHour = 3600
)

// parseLimit reads "+50%" (the sign is the regression's direction and only the
// size counts) or, for a system metric, "6 MiB": the worst head median accepted.
// The number is finite and above zero, with at most one leading "+". A percent
// is at most maxPercent and an absolute value at most its unit's cap.
func parseLimit(raw, gate string) (limit, error) {
	text := strings.TrimSpace(raw)

	if number, ok := strings.CutSuffix(text, "%"); ok {
		pct, ok := positiveNumber(number)
		if !ok {
			return limit{}, fmt.Errorf("%w: max %q is not a finite percent above zero such as +50%%", errAccepted, raw)
		}

		if pct > maxPercent {
			return limit{}, fmt.Errorf("%w: max %q is above the %d%% cap", errAccepted, raw, maxPercent)
		}

		return limit{percent: pct}, nil
	}

	fields := strings.Fields(text)
	if gate != gateSystem || len(fields) != 2 {
		return limit{}, fmt.Errorf("%w: max %q is not a percent such as +50%%, or for a system metric a value "+
			"and its unit such as 6 MiB", errAccepted, raw)
	}

	value, ok := positiveNumber(fields[0])
	if !ok {
		return limit{}, fmt.Errorf("%w: max %q is not a finite value above zero", errAccepted, raw)
	}

	capValue, known := unitCaps[fields[1]]
	if !known {
		return limit{}, fmt.Errorf("%w: max %q names a unit that no system metric uses; they are MiB, ms, s and cycles/s",
			errAccepted, raw)
	}

	if value > capValue {
		return limit{}, fmt.Errorf("%w: max %q is above the %.0f %s cap", errAccepted, raw, capValue, fields[1])
	}

	return limit{absolute: value, unit: fields[1]}, nil
}

// positiveNumber reads a finite number above zero. One leading "+" is allowed;
// a second sign, or a "-", is not.
func positiveNumber(text string) (float64, bool) {
	text = strings.TrimPrefix(text, "+")
	if text == "" || text[0] == '+' || text[0] == '-' {
		return 0, false
	}

	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, false
	}

	return value, true
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

// unused names the entries of gate that match none of the regressions a gate
// run judged: an entry with a glob or a stray space, a benchmark that no longer
// exists, or a regression that has gone. It matches nothing, and says nothing
// until reported. matchers holds one per regression.
func (s acceptedSet) unused(gate string, matchers []func(*acceptedEntry) bool) []string {
	var names []string

	for i := range s.entries {
		e := &s.entries[i]
		if e.Gate != gate {
			continue
		}

		if !slices.ContainsFunc(matchers, func(matches func(*acceptedEntry) bool) bool { return matches(e) }) {
			names = append(names, e.subject())
		}
	}

	return names
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
// it is not. A percent limit and the cap that holds an absolute one both read
// the change as worsening reads it.
func (m measured) within(l limit) string {
	worse := m.worsening()

	switch {
	case l.percent > 0 && worse <= l.percent:
		return ""
	case l.percent > 0:
		return fmt.Sprintf("the change is past the accepted %s", l)
	case l.unit != m.unit:
		return fmt.Sprintf("the accepted %s is not in %s, the metric's unit", l, m.unit)
	case worse > maxPercent:
		return fmt.Sprintf("the accepted %s allows a change of %s, past the %d%% cap on a percent bound", l,
			describePercent(worse), maxPercent)
	case m.headWithin(l.absolute):
		return ""
	default:
		return fmt.Sprintf("the head is past the accepted %s", l)
	}
}

// worsening is how much worse the change is, as the rise in percent in what the
// work costs. A cost is its own change. A throughput that falls d percent takes
// 100/(100-d) times as long for the same work, a rise of 100d/(100-d) percent,
// so "+100%" lets a latency double and a throughput halve. A throughput that
// falls to nothing is a rise without end.
func (m measured) worsening() float64 {
	if m.lowerIsBetter {
		return m.change
	}

	const whole = 100.0

	if m.change <= -whole {
		return math.Inf(1)
	}

	return -whole * m.change / (whole + m.change)
}

// describePercent writes a percent for a message, and "without end" for the
// rise of a throughput that fell to nothing.
func describePercent(pct float64) string {
	if math.IsInf(pct, 1) {
		return "without end"
	}

	return fmt.Sprintf("+%.0f%%", pct)
}

// headWithin reports whether the head is no worse than an absolute limit.
func (m measured) headWithin(value float64) bool {
	const epsilon = 1e-9

	if m.lowerIsBetter {
		return m.head <= value+epsilon
	}

	return m.head >= value-epsilon
}
