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
// "Benchmarks"): saving and loading a queue of 100000 messages of 200 bytes with
// two attributes each, a tenth of them in flight. A FIFO queue spreads them over
// 1000 groups. The rows of many small queues are BenchmarkSQSSnapshot and
// BenchmarkSQSRestore in internal/persist.
//
// Besides the time, a row reports:
//
//	lock-ms     how long a save holds the queue's lock to copy it
//	alloc/file  the bytes a call allocates, in units of the size of the file
const (
	snapshotMessages = 100_000
	snapshotInflight = snapshotMessages / 10
	snapshotBody     = 200
	snapshotAttrs    = 2
	snapshotGroups   = 1000
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

func BenchmarkSnapshot(b *testing.B) {
	for _, kind := range []queueKind{queueStandard, queueFIFO} {
		b.Run(snapshotRow(kind), func(b *testing.B) {
			e := bigQueue(b, kind)

			var file bytes.Buffer

			err := e.provider.Snapshot(&file)
			if err != nil {
				b.Fatal(err)
			}

			held, err := sqs.QueueLockHold(e.provider)
			if err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(file.Len()))
			runtime.GC()

			before := allocated()

			for b.Loop() {
				err = e.provider.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(held.Microseconds())/1000, "lock-ms")
			b.ReportMetric(float64(allocated()-before)/float64(b.N)/float64(file.Len()), "alloc/file")
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, kind := range []queueKind{queueStandard, queueFIFO} {
		b.Run(snapshotRow(kind), func(b *testing.B) {
			var file bytes.Buffer

			err := bigQueue(b, kind).provider.Snapshot(&file)
			if err != nil {
				b.Fatal(err)
			}

			target := sqs.New(nil)
			b.Cleanup(target.Close)

			b.ReportAllocs()
			b.SetBytes(int64(file.Len()))
			runtime.GC()

			before := allocated()

			for b.Loop() {
				err = target.Restore(bytes.NewReader(file.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
			}

			b.ReportMetric(float64(allocated()-before)/float64(b.N)/float64(file.Len()), "alloc/file")
		})
	}
}
