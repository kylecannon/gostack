//go:build unix

package gateway_test

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The growth tests below take the method of Test_Decoder_BatchScalesWithEntries
// in internal/protocol/query, which gives its reasons: the cost of a size is the
// CPU time of the thread that serves it, the smallest over growthRounds rounds,
// and the test decides once, on the exponent fitted over every size and on the
// slope of the largest three. CPU time is not load-proof: a VM on a busy host is
// charged time its guest cannot see, so each test's comment gives the rate of
// false failures it measured.
const (
	// growthRounds is how many times every size is served.
	growthRounds = 15
	// growthExponent is the most the cost over every size may grow faster than
	// the size: linear is 1, a scan of the input per element is 2. It is the
	// limit of Test_Decoder_BatchScalesWithEntries, which gives the CI reading
	// behind it. The two tests are in different test packages, so they could share
	// the constant only through a package of their own for it; each keeps a copy.
	growthExponent = 1.5
	// growthTailSlope is the most the cost between the third largest size and the
	// largest may grow faster than the size. The two are four times apart, so a
	// quadratic step with a constant small enough for the linear cost to hide it at
	// the small sizes shows here.
	growthTailSlope = 1.6
	// growthCeiling is the CPU time of the whole test process one call may take
	// before the test gives up on it, so a quadratic call fails after seconds of
	// CPU and not the minutes it would take to finish.
	growthCeiling = 6 * time.Second
	// growthPoll is how often the ceiling is checked.
	growthPoll = 10 * time.Millisecond
)

// threadCPU is the CPU time the calling OS thread has used. Time the thread
// waits for a CPU does not count.
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

type servedCall struct {
	status int
	cpu    time.Duration
	err    error
}

// cpuServe serves r through h on a goroutine locked to its thread, and returns
// the thread's CPU time over the call alone. It gives up on a call once the
// process has used growthCeiling of CPU since it began, and fails the test; the
// abandoned call ends with the test binary.
func cpuServe(t *testing.T, h http.Handler, r *http.Request) servedCall {
	t.Helper()

	began := processCPU(t)
	done := make(chan servedCall, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		rec := httptest.NewRecorder()
		start, startErr := threadCPU()

		h.ServeHTTP(rec, r)

		end, endErr := threadCPU()

		done <- servedCall{status: rec.Code, cpu: end - start, err: errors.Join(startErr, endErr)}
	}()

	poll := time.NewTicker(growthPoll)
	defer poll.Stop()

	for {
		select {
		case got := <-done:
			require.NoError(t, got.err)

			return got
		case <-poll.C:
			if used := processCPU(t) - began; used > growthCeiling {
				t.Fatalf("one call used over %v of CPU", growthCeiling)
			}
		}
	}
}

// bestCosts is the smallest CPU time of serving each of sizes over growthRounds
// rounds. build makes the request of a size; the call must answer wantStatus.
// The collector runs between calls, so the last call's garbage is not charged to
// the next.
func bestCosts(
	t *testing.T, h http.Handler, sizes []int, wantStatus int, build func(size int) *http.Request,
) []time.Duration {
	t.Helper()

	best := make([]time.Duration, len(sizes))
	for i := range best {
		best[i] = time.Duration(math.MaxInt64)
	}

	for range growthRounds {
		for i, size := range sizes {
			r := build(size)

			runtime.GC() //nolint:revive // the last call's garbage goes before the next is measured.

			got := cpuServe(t, h, r)

			require.Equal(t, wantStatus, got.status, "size %d", size)

			best[i] = min(best[i], got.cpu)
		}
	}

	return best
}

// growthExponentOf is the slope of the least-squares line through the points
// (log size, log cost): the k in cost = c * size^k.
func growthExponentOf(sizes []int, costs []time.Duration) float64 {
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

// reportScale appends the figures of one test to the file named by
// GITHUB_STEP_SUMMARY, which GitHub Actions sets for every step, so a passing CI
// run shows them on its summary page. A passing `go test` prints nothing, and -v
// is not used in CI. Without the variable it does nothing.
func reportScale(t *testing.T, exponent, tail float64, costs []time.Duration) {
	t.Helper()

	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	_, err = fmt.Fprintf(file, "- `%s`: exponent %.3f (limit %.2f), tail slope %.3f (limit %.2f), CPU %v\n",
		t.Name(), exponent, growthExponent, tail, growthTailSlope, costs)
	require.NoError(t, errors.Join(err, file.Close()))
}

// requireNearLinear asserts the two figures of the cost against the size.
func requireNearLinear(t *testing.T, what string, sizes []int, costs []time.Duration) {
	t.Helper()

	from, to := len(sizes)-3, len(sizes)-1
	exponent := growthExponentOf(sizes, costs)
	tail := growthExponentOf([]int{sizes[from], sizes[to]}, []time.Duration{costs[from], costs[to]})

	t.Logf("%s: %v used %v of CPU: exponent %.3f, tail slope %.3f (linear is 1, quadratic is 2)",
		what, sizes, costs, exponent, tail)

	reportScale(t, exponent, tail, costs)

	assert.Less(t, exponent, growthExponent, "%s: the cost over all sizes", what)
	assert.Less(t, tail, growthTailSlope, "%s: the cost between the largest size and a quarter of it", what)
}
