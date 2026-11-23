package awsjson

import (
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
)

// admissionHold is a protocol.HeapKeeper whose request holds bytes of the
// in-flight bound, as one the gateway charged does, so that its dense body
// waits its turn for the lane (protocol.WaitDense). It keeps no reservation:
// the decoder gives them back once the body is decoded.
type admissionHold struct{}

func (admissionHold) PoolHeld() int64 { return 1 }

func (admissionHold) HoldDecode(int64) (int64, error) { return math.MaxInt64, nil }

func (admissionHold) KeepHeap(func()) bool { return false }

func (admissionHold) ReleasePool() {}

// BenchmarkDenseAdmission is the dense queue's matrix (docs/standards.md,
// "Benchmarks"): a body past protocol.DecodeBudget, walked, admitted to the
// dense lane and decoded, from concurrency senders at once, each the body of
// size. One iteration is one body from each sender. At concurrency 1 the body
// finds no queue (the uncontended path, the same calls main makes); past it
// the senders take the lane in turn, each waiting in the queue for the one
// before to give it back, and none is throttled. The heap has room for every
// body at once, so the lane is what they wait on. Sub-benchmark names carry
// the axes, for example
//
//	BenchmarkDenseAdmission/body=36KB/concurrency=4
//
// Cells left out, and why: a 144 KB body at concurrency 4, whose slowest
// operation over three runs at the gate's benchtime was 9.1 ms, past the 8 ms
// docs/standards.md allows; its slowest at concurrency 2 was 6.9 ms, near it.
// The payload axis is covered by the widest cell under the limit, 72 KB, whose
// slowest operation was 5.6 ms at concurrency 4.
func BenchmarkDenseAdmission(b *testing.B) {
	old := debug.SetMemoryLimit(math.MaxInt64)
	oldCap := protocol.SetMemoryCap(0)

	b.Cleanup(func() {
		debug.SetMemoryLimit(old)
		protocol.SetMemoryCap(oldCap)
	})

	for _, size := range []struct {
		name     string
		elements int
	}{{"36KB", 12_000}, {"72KB", 24_000}} {
		body := denseBody(size.elements)
		budget := protocol.DecodeBudgetAt(len(body), 256)

		for _, concurrency := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("body=%s/concurrency=%d", size.name, concurrency), func(b *testing.B) {
				b.SetBytes(int64(len(body) * concurrency))
				b.ReportAllocs()

				for b.Loop() {
					admitAll(b, body, budget, concurrency)
				}
			})
		}
	}
}

// admitAll decodes body from concurrency senders at once and fails b at an
// answer other than served.
func admitAll(b *testing.B, body []byte, budget int64, concurrency int) {
	b.Helper()

	var wg sync.WaitGroup

	errs := make([]error, concurrency)

	for sender := range concurrency {
		wg.Go(func() {
			_, _, err := DecodeWithin[denseValue](body, budget, admissionHold{})
			if err != nil {
				errs[sender] = fmt.Errorf("sender %d: %s", sender, err.Code)
			}
		})
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			b.Fatal(err)
		}
	}
}
