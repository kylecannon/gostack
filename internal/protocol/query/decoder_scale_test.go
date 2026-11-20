package query_test

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	// scaleRounds is how many times every size is decoded. A round decodes the
	// sizes one after the other, the cost of a size is its smallest over the
	// rounds, and the test decides once, on that.
	scaleRounds = 5
	// scaleExponent is the most the cost over every size may grow faster than the
	// size: linear is 1, the sort of the keys adds a little, a scan of every key
	// per lookup is 2. See Test_Decoder_BatchScalesWithEntries for the figures.
	scaleExponent = 1.3
	// scaleTailSlope is the most the cost between 4,000 and 16,000 entries may
	// grow faster than the size. It catches a quadratic step whose constant is
	// small enough for the linear cost to hide it at the small sizes, where it
	// holds the fitted exponent down. The ends are four times apart, so noise in
	// one of them moves the slope half as far as it would between neighbours.
	scaleTailSlope = 1.35
	// scaleCeiling is the CPU time of the whole test process one decode may take
	// before the test gives up on it, so a quadratic decode fails after seconds of
	// CPU and not the minutes it would take to finish. A healthy decode of the
	// largest size takes under 0.2 s under the race detector. A machine under
	// load stretches the wall time to the ceiling, never the CPU time.
	scaleCeiling = 4 * time.Second
	// scalePoll is how often the ceiling is checked.
	scalePoll = 10 * time.Millisecond

	elemOne    = "L.1"
	elemTwo    = "L.2"
	elemThree  = "L.3"
	nameMember = "Name"
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
// spends waiting for a CPU does not count, so a busy machine does not raise it.
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
// on a lucky round. This one decides once.
//
// What it is proven to catch, with a full scan of the form planted in hasBelow
// on 1 lookup in N, 10 runs each under -race at load average 191 to 387 on 10
// cores: every lookup, 1 in 16, 64, 256, 1,024, 1,280 and 1,536 fail 10 of 10
// (smallest tail slope at 1 in 1,536: 1.43), as does a scan on 1 lookup in
// 1,024 that starts only above 12,000 entries. 1 in 2,048 failed 10 of 10 with
// its smallest tail at 1.357, on the limit, and 1 in 3,072 failed 3 of 10. So
// the smallest defect it catches with room to spare is a scan on 1 lookup in
// 1,536; from 1 in 2,048 down it may pass. A healthy decode measured an exponent of
// 1.10 to 1.17 and a tail slope of 1.02 to 1.24 over 300 runs at load average
// 218 to 408 beside two `go test -race ./...` loops, and passed every one, as it did 50
// times under GOMAXPROCS=1. The race detector slows every size alike, so the
// test needs no skip under it.
func Test_Decoder_BatchScalesWithEntries(t *testing.T) {
	costs := bestDecodes(t)

	exponent, tail := scaleFigures(costs)
	t.Logf("%v entries used %v of CPU: exponent %.3f, tail slope %.3f (linear is 1, quadratic is 2)",
		scaleSizes, costs, exponent, tail)

	assert.Less(t, exponent, scaleExponent, "the cost over all sizes")
	assert.Less(t, tail, scaleTailSlope, "the cost between the largest size and a quarter of it")
}

// scanHas is the answer Has gives: the key itself or any key below it.
func scanHas(vals url.Values, key string) bool {
	if _, ok := vals[key]; ok {
		return true
	}

	for k := range vals {
		if strings.HasPrefix(k, key+".") {
			return true
		}
	}

	return false
}

// Test_Decoder_HasAgreesWithScan checks the sorted index against the scan it
// replaced, over keys that sit next to each other in sort order: a longer name,
// a dot, a digit, an uppercase and a lowercase letter, a dash.
func Test_Decoder_HasAgreesWithScan(t *testing.T) {
	t.Parallel()

	vals := values(t, "L.1.Name=a&L.1=b&L.10.Name=c&L.1a=d&L.2.Name.X=e&L-1=f&L=g&LA.1=h&M.1.Name=i&N.%2E=j")
	vals.Set("", "empty")

	probes := []string{
		"", "L", elemOne, "L.1.Name", "L.10", "L.10.Name", "L.1a", elemTwo, "L.2.Name", "L.2.Name.X",
		"L.2.Name.Y", elemThree, "L-1", "L-", "LA", "LA.1", "La", "M", "M.1", "M.1.Name", "M.1.Name.Z", "N", "N.", "N..",
		"Z", "A", "L.", "L.1.", "L.1.Nam",
	}

	for _, key := range probes {
		d := query.NewDecoder(vals)
		assert.Equal(t, scanHas(vals, key), d.Has(key), "key %q", key)
	}

	// One Decoder answers every probe, in any order, from one index.
	d := query.NewDecoder(vals)
	for _, key := range slices.Backward(probes) {
		assert.Equal(t, scanHas(vals, key), d.Has(key), "key %q", key)
	}
}

func Test_Decoder_HasOfEmptyForm(t *testing.T) {
	t.Parallel()

	d := query.NewDecoder(url.Values{})

	assert.False(t, d.Has("A"))
	assert.False(t, d.Has("A"), "the second lookup uses the index that is empty")
	assert.Nil(t, d.List("L", &query.ListSpec{Names: []string{"L"}, Complex: true}))
}

// Test_Decoder_ComplexListOrderAndGaps pins what a list of structures does with
// numbers out of order, a gap, and a key that repeats: the elements follow the
// element number, not the sort order of the keys or the order of the form; the
// first gap ends the list; and a repeated key counts once, with its first value.
func Test_Decoder_ComplexListOrderAndGaps(t *testing.T) {
	t.Parallel()

	spec := query.ListSpec{Names: []string{"L"}, Flattened: true, Complex: true}

	t.Run("numbers follow the element number, not the sort order", func(t *testing.T) {
		t.Parallel()

		var raw []string
		for i := 12; i >= 1; i-- {
			raw = append(raw, "L."+strconv.Itoa(i)+".Name=n"+strconv.Itoa(i))
		}

		d := query.NewDecoder(values(t, strings.Join(raw, "&")))

		var want []string
		for i := 1; i <= 12; i++ {
			want = append(want, "L."+strconv.Itoa(i))
		}

		got := d.List("", &spec)
		require.Equal(t, want, got)

		for i, k := range got {
			assert.Equal(t, "n"+strconv.Itoa(i+1), *d.String(query.Key(k, "Name")))
		}
	})

	t.Run("the first gap ends the list", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "L.1.Name=a&L.2.Name=b&L.4.Name=d&L.5.Name=e"))

		assert.Equal(t, []string{elemOne, elemTwo}, d.List("", &spec))
	})

	t.Run("a repeated key is one element and the first value wins", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "L.1.Name=a&L.1.Name=z&L.2.Name=b&L.2.Name=y"))

		got := d.List("", &spec)
		require.Equal(t, []string{elemOne, elemTwo}, got)
		assert.Equal(t, "a", *d.String("L.1.Name"))
		assert.Equal(t, "b", *d.String("L.2.Name"))
	})

	t.Run("an element is the key itself or any key below it", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "L.1=plain&L.2.Name=b&L.3.A.B=c"))

		assert.Equal(t, []string{elemOne, elemTwo, elemThree}, d.List("", &spec))
	})
}

// Test_Decoder_ComplexMapGaps pins the same for a map whose values are
// structures: entry numbers go in order, an entry needs its key and a value
// that is a key or has keys below it, and the first entry that lacks one ends
// the map.
func Test_Decoder_ComplexMapGaps(t *testing.T) {
	t.Parallel()

	spec := query.MapSpec{Names: []string{"M"}, Key: nameMember, Value: valueMember, Flattened: true, ComplexValue: true}

	var raw []string
	for i := 11; i >= 1; i-- {
		n := strconv.Itoa(i)
		raw = append(raw, "M."+n+".Name=k"+n, "M."+n+".Value.Type=String")
	}

	d := query.NewDecoder(values(t, strings.Join(raw, "&")))
	got := d.Map("", &spec)

	require.Len(t, got, 11)

	for i, e := range got {
		n := strconv.Itoa(i + 1)
		assert.Equal(t, query.Entry{Key: "M." + n + ".Name", Value: "M." + n + ".Value"}, e)
		assert.Equal(t, "k"+n, *d.String(e.Key))
	}

	gap := query.NewDecoder(values(t, "M.1.Name=a&M.1.Value.T=x&M.2.Name=b&M.3.Name=c&M.3.Value.T=z"))
	assert.Len(t, gap.Map("", &spec), 1, "entry 2 has no value, so the map ends before it")
}

// Test_Decoder_NestedComplexList is the shape that made Has hot twice: a list
// of structures whose members are lists of structures.
func Test_Decoder_NestedComplexList(t *testing.T) {
	t.Parallel()

	const outer, inner = 50, 20

	vals := url.Values{}

	for i := 1; i <= outer; i++ {
		for j := 1; j <= inner; j++ {
			vals.Set(fmt.Sprintf("O.%d.I.%d.V", i, j), "v")
		}
	}

	d := query.NewDecoder(vals)
	outerSpec := query.ListSpec{Names: []string{"O"}, Flattened: true, Complex: true}
	innerSpec := query.ListSpec{Names: []string{"I"}, Flattened: true, Complex: true}

	got := 0

	for _, o := range d.List("", &outerSpec) {
		keys := d.List(o, &innerSpec)
		require.Len(t, keys, inner)

		got += len(keys)
	}

	assert.Equal(t, outer*inner, got)
}
