package sqs_test

import (
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The rows of the snapshot lists that have no bound on their length, which
// BenchmarkSnapshot's queue of 1000 groups does not reach: a FIFO queue whose
// 100000 messages are each in a group of their own, so that the file lists
// 100000 ready groups, and a State that holds 100000 move tasks. What a row
// shows is the bytes a save allocates: the file goes to the writer a piece at a
// time, and a save must not hold a list of this length whole.
const (
	listMembers = 100_000
	listBody    = 10
)

// moveTaskFile is a snapshot of one State that holds n completed move tasks.
func moveTaskFile(n int) string {
	var file strings.Builder

	file.WriteString(`{"v":1,"data":{"states":[{"account":"000000000000","region":"us-east-1","next":0,`)
	file.WriteString(`"queues":[],"moveTasks":[`)

	for i := range n {
		if i > 0 {
			file.WriteByte(',')
		}

		id := strconv.Itoa(i)

		file.WriteString(`{"id":"task-` + id + `","source":"arn:aws:sqs:us-east-1:000000000000:source",` +
			`"started":"2026-01-01T00:00:00Z","order":` + id + `,"status":"COMPLETED","moved":1,"toMove":1}`)
	}

	file.WriteString(`],"moveSeq":` + strconv.Itoa(n) + `}]}}`)

	return file.String()
}

// BenchmarkSnapshotLists saves a FIFO queue with a ready group for each of its
// messages, and a State with a completed move task for each of its tasks. A
// Provider holds a move task only after a move has run, so that row fills it by
// restoring a file.
func BenchmarkSnapshotLists(b *testing.B) {
	n := strconv.Itoa(listMembers)
	rows := []struct {
		name string
		fill func(e *benchEnv)
	}{
		{"queues=1/messages=" + n + "/queue=fifo/groups=" + n, func(e *benchEnv) {
			e.fill(e.queue(queueFIFO), queueFIFO, listMembers, listBody, 0, listMembers)
		}},
		{"queues=0/movetasks=" + n, func(e *benchEnv) {
			err := e.provider.Restore(strings.NewReader(moveTaskFile(listMembers)))
			if err != nil {
				e.b.Fatal(err)
			}
		}},
	}

	for _, row := range rows {
		b.Run(row.name, func(b *testing.B) {
			e := newBenchEnv(b)

			row.fill(e)

			// The file is counted, not kept: the benchmark needs its size only.
			var file countingWriter

			err := e.provider.Snapshot(&file)
			if err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(file.n))
			runtime.GC()

			for b.Loop() {
				err = e.provider.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
