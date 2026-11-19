package main

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The report flags three things and nothing else, the standing benchmark rule: a
// gostack cell an order of magnitude slower than LocalStack, memory that grows
// with object size, and CPU burned while idle. A smaller gap is in the tables
// and is not chased.
const (
	// orderOfMagnitude is the ratio a gostack figure has to reach over LocalStack's.
	orderOfMagnitude = 10
	// slowFloorUS is the p50 a gostack operation has to reach as well, so a tick
	// of the timer's resolution is not called slow.
	slowFloorUS = 1000

	// growthFloorBytes and growthShare set when RSS has grown with the payload and
	// not with the runtime's own warming up: by at least the floor, and by at
	// least this share of the bytes in flight (payload times concurrency). Memory
	// that streams stays near the cost of a few buffers a worker.
	growthFloorBytes = 16 * bytesPerMiB
	growthShare      = 0.5

	// idleCPULimit is the share of one core, in percent, over which an idle
	// container is burning CPU. The target is no wakeups at all; a HEALTHCHECK
	// that runs every ten seconds costs about 0.2%.
	idleCPULimit = 1.0
)

// flags writes the three flags of the report.
func (r report) flags(b *strings.Builder) {
	b.WriteString("## Flags\n\n")
	b.WriteString("Only three things are flagged. Everything else is in the tables above.\n\n")

	b.WriteString("### A gostack cell ten times slower than LocalStack\n\n")
	b.WriteString("A p50 ten times LocalStack's and over 1 ms, or a tenth of LocalStack's cycles a second. " +
		"A persistence-on cell is held against LocalStack's persistence-off cell, which cannot persist.\n\n")
	writeList(b, r.slowerFlags(), "none (or LocalStack was not measured)")

	b.WriteString("### Memory that grows with object size\n\n")
	b.WriteString("Peak RSS above the start of the run, held against the smallest payload of the same cell at the " +
		"same concurrency: a rise of at least 16 MiB and half the bytes in flight. A cell with one payload size is " +
		"flagged when it rises by the whole object.\n\n")
	writeList(b, r.memoryFlags(), "none")

	b.WriteString("### CPU while idle\n\n")
	fmt.Fprintf(b, "An idle container over %.0f%% of one core.\n\n", idleCPULimit)
	writeList(b, r.idleFlags(), "none")
}

// isGostack is true for the gostack images, full and slim.
func isGostack(t targetResult) bool { return strings.HasPrefix(t.Name, targetGostack) }

// slowerFlags lists the gostack cells a LocalStack twin beats tenfold.
func (r report) slowerFlags() []string {
	reference, ok := r.target(targetLocalstack)
	if !ok {
		return nil
	}

	var lines []string

	for _, t := range r.Targets {
		if !isGostack(t) {
			continue
		}

		for _, c := range t.Cells {
			twin := c.Spec
			twin.PersistEnv = ""

			ref, found := reference.cell(twin.id())
			if c.Problem != "" || !found || ref.Problem != "" {
				continue
			}

			if why := tenfoldSlower(c, ref); why != "" {
				lines = append(lines, fmt.Sprintf("- %s `%s`: %s", t.Name, c.ID, why))
			}
		}
	}

	return lines
}

// tenfoldSlower says how a cell is ten times slower than its LocalStack twin, or
// returns "".
func tenfoldSlower(c, ref cellResult) string {
	var reasons []string

	for _, name := range c.opNames() {
		got, want := c.Ops[name].P50us.Median, ref.Ops[name].P50us.Median
		if want > 0 && got >= orderOfMagnitude*want && got >= slowFloorUS {
			reasons = append(reasons, fmt.Sprintf("%s p50 %.2f ms against %.2f", name, got/microsPerMilli, want/microsPerMilli))
		}
	}

	got, want := c.CyclesPerSec.Median, ref.CyclesPerSec.Median
	if got > 0 && want > 0 && got*orderOfMagnitude <= want {
		reasons = append(reasons, fmt.Sprintf("%.2f cycles/s against %.2f", got, want))
	}

	return strings.Join(reasons, "; ")
}

// growth is how far a cell's resident set rose above the start of its runs.
func (c cellResult) growth() float64 {
	return max(c.PeakRSSBytes.Median-c.BaselineRSSBytes.Median, 0)
}

// memoryFlags lists the gostack cells whose memory follows the size of the
// object.
func (r report) memoryFlags() []string {
	var lines []string

	for _, t := range r.Targets {
		if !isGostack(t) {
			continue
		}

		groups := map[string][]cellResult{}

		for _, c := range t.Cells {
			if c.Problem != "" || c.PeakRSSBytes.Median == 0 {
				continue
			}

			same := c.Spec
			same.Payload = 0
			groups[same.id()] = append(groups[same.id()], c)
		}

		for _, id := range slices.Sorted(maps.Keys(groups)) {
			lines = append(lines, memoryGrowth(t.Name, groups[id])...)
		}
	}

	return lines
}

// memoryGrowth checks the cells of one group, which differ only in payload.
func memoryGrowth(target string, group []cellResult) []string {
	slices.SortFunc(group, func(a, b cellResult) int { return cmp.Compare(a.Spec.Payload, b.Spec.Payload) })

	lines := make([]string, 0, len(group))
	smallest := group[0]

	for i, c := range group {
		inFlight := float64(c.Spec.Payload) * float64(c.Spec.Concurrency)
		rise := c.growth()
		against := "above the start of the run"

		switch {
		case len(group) == 1:
			// One size, and big enough to tell from the runtime warming up: the
			// whole object, held in memory, would show as its own size.
			if c.Spec.Payload < growthFloorBytes || rise < float64(c.Spec.Payload) {
				continue
			}
		case i == 0:
			continue
		default:
			rise -= smallest.growth()
			against = fmt.Sprintf("above the %s cell", payloadName(smallest.Spec.Payload))

			if rise < max(growthFloorBytes, growthShare*inFlight) {
				continue
			}
		}

		lines = append(lines, fmt.Sprintf("- %s `%s`: peak RSS %s, %.1f MiB %s (%s in flight)", target, c.ID,
			mib(uint64(c.PeakRSSBytes.Median)), rise/bytesPerMiB, against, mib(uint64(inFlight))))
	}

	return lines
}

// idleFlags lists the gostack images that burn CPU with nothing to do.
func (r report) idleFlags() []string {
	var lines []string

	for _, t := range r.Targets {
		if !isGostack(t) {
			continue
		}

		for _, v := range []struct {
			name string
			idle idleVariant
		}{{"with the HEALTHCHECK", t.Idle.Healthcheck}, {"without the HEALTHCHECK", t.Idle.NoHealthcheck}} {
			if v.idle.CPUPercent.Median > idleCPULimit {
				lines = append(lines, fmt.Sprintf("- %s %s: %.2f%% of one core over %.0f s", t.Name, v.name,
					v.idle.CPUPercent.Median, v.idle.Seconds))
			}
		}
	}

	return lines
}
