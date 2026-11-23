package sqs_test

import (
	"bytes"
	"io"
	"runtime"
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sqs"
)

// The persistence rows of one queue that holds many messages (docs/standards.md,
// "Benchmarks"): saving and loading a queue of 1000 messages of 200 bytes with
// two attributes each, a tenth of them in flight. A FIFO queue spreads them over
// 100 groups. The rows of many small queues are BenchmarkSQSSnapshot and
// BenchmarkSQSRestoreExact in internal/persist.
//
// The queue held 100000 messages in 1000 groups, which is left out
// (docs/standards.md, "Benchmarks"): the slowest of three runs took 76.5 ms
// (standard) and 177.1 ms (FIFO) to save, and 314.3 ms and 446.8 ms to restore.
// The cost grows with the count, so the 1000 stands for the ladder, and it no
// longer shows a save's lock hold or allocation at the size of a very large
// queue.
//
// Besides the time, a row reports:
//
//	lock-ms     how long a save holds the queue's lock to copy it
//	alloc/file  the bytes a call allocates, in units of the size of the file
const (
	snapshotMessages = 1000
	snapshotInflight = snapshotMessages / 10
	snapshotBody     = 200
	snapshotAttrs    = 2
	snapshotGroups   = 100
)

// bigQueue returns an environment whose one queue holds the messages above.
func bigQueue(b *testing.B, kind queueKind) *benchEnv {
	b.Helper()

	e := newBenchEnv(b)
	url := e.queue(kind)

	e.fill(url, kind, snapshotMessages, snapshotBody, snapshotAttrs, snapshotGroups)
	e.handles(url, snapshotInflight)

	return e
}

// snapshotRow is the name of a row: the FIFO one carries its queue axis.
func snapshotRow(kind queueKind) string {
	name := "queues=1/messages=" + strconv.Itoa(snapshotMessages)
	if kind == queueFIFO {
		name += "/queue=fifo"
	}

	return name
}

// allocated is the bytes the process has allocated so far.
func allocated() uint64 {
	var stats runtime.MemStats

	runtime.ReadMemStats(&stats)

	return stats.TotalAlloc
}

// countingWriter counts the bytes written to it.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)

	return len(p), nil
}

func BenchmarkSnapshot(b *testing.B) {
	for _, kind := range []queueKind{queueStandard, queueFIFO} {
		b.Run(snapshotRow(kind), func(b *testing.B) {
			e := bigQueue(b, kind)

			// The file is counted, not kept: the benchmark needs its size only.
			var file countingWriter

			err := e.provider.Snapshot(&file)
			if err != nil {
				b.Fatal(err)
			}

			held, err := sqs.QueueLockHold(e.provider)
			if err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(file.n))
			runtime.GC()

			before := allocated()

			for b.Loop() {
				err = e.provider.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(held.Microseconds())/1000, "lock-ms")
			b.ReportMetric(float64(allocated()-before)/float64(b.N)/float64(file.n), "alloc/file")
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, kind := range []queueKind{queueStandard, queueFIFO} {
		b.Run(snapshotRow(kind), func(b *testing.B) {
			var file bytes.Buffer

			source := bigQueue(b, kind)

			err := source.provider.Snapshot(&file)
			if err != nil {
				b.Fatal(err)
			}

			// The file holds the state now. Emptying the source lets the heap
			// shed it, so the benchmark holds one copy of the queue and not two.
			source.provider.Reset("", "")

			target := sqs.New(nil, nil)
			// Reset stops the deadline timers of the in-flight messages, which
			// would keep the restored queue in memory for 30 seconds.
			b.Cleanup(func() {
				target.Close()
				target.Reset("", "")
			})

			b.ReportAllocs()
			b.SetBytes(int64(file.Len()))
			runtime.GC()

			before := allocated()

			for b.Loop() {
				// A restore reads a file into an empty Provider, as at start-up.
				// Emptying it first, with the timer stopped, keeps the last
				// restore's queue from being in memory beside the new one.
				b.StopTimer()
				target.Reset("", "")
				b.StartTimer()

				err = target.Restore(bytes.NewReader(file.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(allocated()-before)/float64(b.N)/float64(file.Len()), "alloc/file")
		})
	}
}
