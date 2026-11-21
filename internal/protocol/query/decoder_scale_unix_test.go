//go:build unix

package query_test

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	// scaleRounds is how many times every size is decoded. A round decodes the
	// sizes one after the other, the cost of a size is its smallest over the
	// rounds, and the test decides once, on that. Contention only adds time, so
	// the smallest is the closest figure to the decode's own cost, but a host
	// that is busy for longer than a few rounds inflates the largest size in all
	// of them. See Test_Decoder_BatchScalesWithEntries for the runs behind 15.
	scaleRounds = 15
	// scaleExponent is the most the cost over every size may grow faster than the
	// size: linear is 1, the sort of the keys adds a little, a scan of every key
	// per lookup is 2. See Test_Decoder_BatchScalesWithEntries for the figures.
	scaleExponent = 1.5
	// scaleTailSlope is the most the cost between 4,000 and 16,000 entries may
	// grow faster than the size. It catches a quadratic step whose constant is
	// small enough for the linear cost to hide it at the small sizes, where it
	// holds the fitted exponent down. The ends are four times apart, so noise in
	// one of them moves the slope half as far as it would between neighbours.
	scaleTailSlope = 1.6
	// scaleCeiling is the CPU time of the whole test process one decode may take
	// before the test gives up on it, so a quadratic decode fails after seconds of
	// CPU and not the minutes it would take to finish. A healthy decode of the
	// largest size takes under 0.25 s under the race detector. A machine under
	// load stretches the wall time and, on a VM, the CPU time by up to a fifth;
	// the ceiling is 16 times a healthy decode, so neither reaches it.
	scaleCeiling = 4 * time.Second
	// scalePoll is how often the ceiling is checked.
	scalePoll = 10 * time.Millisecond
)

// scaleSizes are the batch sizes decoded. Each doubles the one before, and the
// largest is where the old ratio test looked.
var scaleSizes = []int{1_000, 2_000, 4_000, 8_000, 16_000}

// scaleFigures are the two figures the test asserts: the exponent fitted over
// every size, and the slope between the largest size and the one a quarter of
// its size.
func scaleFigures(costs []time.Duration) (exponent, tail float64) {
	from, to := len(scaleSizes)-3, len(scaleSizes)-1

	return growthExponent(scaleSizes, costs),
		growthExponent([]int{scaleSizes[from], scaleSizes[to]}, []time.Duration{costs[from], costs[to]})
}

type decodeRun struct {
	entries, attributes int
	cpu                 time.Duration
	err                 error
}

// threadCPU is the CPU time the calling OS thread has used. Time the thread
// spends waiting for a CPU does not count, so a busy machine raises it far less
// than the wall time. It does raise it: a VM whose host is busy charges the guest
// time the guest cannot see (a fixed loop read 22% more CPU in a Linux VM beside
// two host test loops), and cache and memory contention slow the largest size
// most. The test takes the smallest of several rounds for that.
func threadCPU() (time.Duration, error) {
	var ts unix.Timespec

	err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts)
	if err != nil {
		return 0, fmt.Errorf("thread CPU clock: %w", err)
	}

	return time.Duration(ts.Nano()), nil
}

// processCPU is the CPU time the whole test process has used.
func processCPU(t *testing.T) time.Duration {
	t.Helper()

	var usage unix.Rusage
	require.NoError(t, unix.Getrusage(unix.RUSAGE_SELF, &usage))

	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// cpuDecode decodes one batch form on a goroutine locked to its thread and
// returns the thread's CPU time over the decode, so the figure counts that
// decode and nothing else. It gives up on a decode once the process has used
// scaleCeiling of CPU since it began, and fails the test, so a quadratic decode
// of the largest size costs seconds and not the minutes it would take to finish.
// The abandoned decode ends with the test binary.
func cpuDecode(t *testing.T, vals url.Values) decodeRun {
	t.Helper()

	began := processCPU(t)
	done := make(chan decodeRun, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		start, startErr := threadCPU()
		entries, attributes := decodeBatch(vals)
		end, endErr := threadCPU()

		done <- decodeRun{entries: entries, attributes: attributes, cpu: end - start, err: errors.Join(startErr, endErr)}
	}()

	poll := time.NewTicker(scalePoll)
	defer poll.Stop()

	for {
		select {
		case run := <-done:
			require.NoError(t, run.err)

			return run
		case <-poll.C:
			if used := processCPU(t) - began; used > scaleCeiling {
				t.Fatalf("one decode of %d keys used over %v of CPU", len(vals), scaleCeiling)
			}
		}
	}
}

// bestDecodes is the smallest CPU time of each of scaleSizes over scaleRounds
// rounds. The collector is off while it measures, and runs between decodes, so
// none of its work lands in one size's figure.
func bestDecodes(t *testing.T) []time.Duration {
	t.Helper()

	forms := make([]url.Values, len(scaleSizes))
	best := make([]time.Duration, len(scaleSizes))

	for i, n := range scaleSizes {
		forms[i] = batchForm(n)
		best[i] = time.Duration(math.MaxInt64)
	}

	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	for range scaleRounds {
		for i, n := range scaleSizes {
			runtime.GC() //nolint:revive // the collector is off, and the last decode's garbage goes before the next is measured.

			run := cpuDecode(t, forms[i])

			require.Equal(t, n, run.entries)
			require.Equal(t, n, run.attributes)

			best[i] = min(best[i], run.cpu)
		}
	}

	return best
}

// growthExponent is the slope of the least-squares line through the points
// (log size, log cost): the k in cost = c * size^k.
func growthExponent(sizes []int, costs []time.Duration) float64 {
	var sumX, sumY, sumXY, sumXX float64

	for i, n := range sizes {
		x, y := math.Log(float64(n)), math.Log(float64(costs[i]))
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}

	count := float64(len(sizes))

	return (count*sumXY - sumX*sumY) / (count*sumXX - sumX*sumX)
}

// Test_Decoder_BatchScalesWithEntries pins the decode of a batch to near-linear
// time. A key below another used to be found by scanning every key of the form,
// so decoding n entries took time in n squared. The test decodes five sizes and
// asserts two figures of the cost against the size: the exponent fitted over all
// of them, which catches a quadratic step; and the slope from 4,000 to 16,000
// entries, which catches one whose constant is small enough for the linear cost
// to hide it until the size is large. A quadratic step comes out near 2 on
// both, a decode over a sorted index near 1.1.
//
// The cost is CPU time, not wall-clock time: time spent waiting for a CPU was
// the noise that failed the wall-clock versions of this test on a loaded
// machine (a ratio limit of 40 failed at 40.4 and 41.4 in CI), and a version
// that added rounds until the figures passed let a borderline quadratic pass
// on a lucky round. This one decides once, on a fixed number of rounds.
//
// CPU time is not load-proof. A thread that waits for a CPU is not charged, but
// a VM on a busy host is charged time the guest cannot see, and cache and memory
// contention slow the largest size most. Taking the smallest of scaleRounds
// rounds per size removes it unless the host stays busy through all of them; 5
// rounds left a healthy decode at a tail slope of 1.367 and 1.435 in 2 of 100
// runs in a linux/arm64 container under -race beside two `go test -race ./...`
// loops on the macOS host (SQ-360), and 15 rounds left none in 200 (below).
//
// The test relies on not calling t.Parallel(): it switches the collector off for
// its whole run, and its ceiling counts the CPU time of the whole process, so a
// test running beside it would be charged to both.
//
// The limits are 1.5 and 1.6 because CI failed a healthy decode at the first
// ones, 1.3 and 1.35 (SQ-444). Run 44878816282, ubuntu-latest under -race, on a
// change to internal/persist alone, read 9.2, 20.3, 42.1, 114.1 and 279.5 ms for
// the five sizes: an exponent of 1.233 and a tail slope of 1.365. It is the only
// CPU reading CI has produced, since a passing run logs none; every other reading
// below is from a macOS host or a container on it. A healthy tail slope was 1.02
// to 1.30 in the container, 1.10 to 1.19 on darwin and 1.365 on that runner,
// where the 4,000 to 8,000 step alone cost 2.7 times (a slope of 1.44), so a point
// of the five can sit well off the line even as the smallest of 15 rounds.
// A tail limit of 1.6 lets the largest size cost 387 ms where that runner read
// 279 ms, 39% more, and an exponent limit of 1.5 sits 0.27 above its 1.233.
//
// A count of the keys looked at, kept by a hook in the Decoder, would not move
// with the machine, but it counts only what the Decoder's own code reports. The
// defect this test exists for is a loop over the form that the author wrote and
// did not report, as in the plants below, so a count passes it and CPU time does
// not. Nor does the cost show in allocations: a scan allocates nothing.
//
// What it is proven to catch, with a full scan of the form planted in hasBelow,
// on darwin with 15 rounds under -race (SQ-444). (a) a scan on every lookup: the
// ceiling stops it at 4,000 entries after 4 s of CPU; with the ceiling lifted and
// one round it reads an exponent of 2.087 and a tail slope of 2.040. (b) a scan on
// 1 lookup in 256: exponent 1.70, tail slope 1.85, 2 of 2 runs. (c) a scan on
// every lookup, only above 60,000 keys, which is the 16,000-entry batch alone:
// the ceiling stops it; with the ceiling lifted, a tail slope of 6.36. (d) a scan
// on 1 lookup in 256 above 60,000 keys: exponent 1.66, tail slope 2.46, 2 of 2
// runs. By rate, the tail slope of a scan on 1 lookup in 512 is 1.72, in 768 1.63
// and 1.64, in 1,024 1.58 and 1.59, in 2,048 1.42 and 1.43. So the smallest
// defect it catches with room to spare is a scan on 1 lookup in 768; from 1 in
// 1,024 down it may pass. The old limits caught 1 in 1,536.
//
// A healthy decode, in that container beside the two loops, measured an exponent
// of 1.09 to 1.22 and a tail slope of 1.02 to 1.30 (median 1.15, 99th percentile
// 1.30) over 200 runs. On darwin beside the same loops, at load average 64 to
// 164, it measured an exponent of 1.08 to 1.13 and a tail slope of 1.10 to 1.16
// over 100 runs; with 5 rounds it also ran 300 runs at load average 218 to 408
// and 50 under GOMAXPROCS=1. With the limits of SQ-444, 200 runs under -race
// beside a loop of `go test -race -count=1 ./...` on darwin at load average 30
// to 75 measured an exponent of 1.115 to 1.169 and a tail slope of 1.107 to 1.192,
// and passed every one. The race detector slows the sizes by nearly the same
// factor (a healthy tail slope of 1.16 with it, 1.21 without), so the test needs
// no skip under it.
func Test_Decoder_BatchScalesWithEntries(t *testing.T) {
	costs := bestDecodes(t)

	exponent, tail := scaleFigures(costs)
	t.Logf("%v entries used %v of CPU: exponent %.3f, tail slope %.3f (linear is 1, quadratic is 2)",
		scaleSizes, costs, exponent, tail)

	assert.Less(t, exponent, scaleExponent, "the cost over all sizes")
	assert.Less(t, tail, scaleTailSlope, "the cost between the largest size and a quarter of it")
}
