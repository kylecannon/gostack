package sqs_test

import (
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
)

// The rows of the snapshot lists that have no bound on their length: a FIFO
// queue whose messages are each in a group of their own, so that the file lists
// a ready group for each of them, and a State that holds move tasks. What a row
// shows is the bytes a save allocates: the file goes to the writer a piece at a
// time, and a save must not hold a list of this length whole.
//
// A cell is sized to the 10 ms of docs/standards.md, so it is far smaller than
// the lists a busy queue has, and each list in it is still several times the
// 64 KiB the writer holds before it writes: 4000 move tasks are 0.8 MB of file,
// and 2000 groups of 100 characters are 0.2 MB of ready list in a file of 1.8
// MB. An operation took at most 2.3 ms for the groups and 1.8 ms for the tasks
// on an M1 Max, the slowest of six runs at -benchtime=500ms. The 100000-member lists
// these rows were first sized to took 156 to 287 ms and are left out; the
// writer's own test, Test_writeSnapshot_writesManyMembersInPieces, covers them.
const (
	listMessages = 2000
	listGroupPad = 100
	listBody     = 10
	listTasks    = 4000
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

// fillGroups sends n messages of a few bytes to a FIFO queue, each to a group of
// its own whose id is listGroupPad characters long, so that the ready list of
// the file is long with few messages.
func (e *benchEnv) fillGroups(url string, n int) {
	e.b.Helper()

	body := benchBody(listBody)
	pad := strings.Repeat("g", listGroupPad)

	for i := range n {
		e.sent++

		in := &sqsapi.SendMessageInput{
			QueueUrl:               &url,
			MessageBody:            &body,
			MessageGroupId:         new(pad + strconv.Itoa(i)),
			MessageDeduplicationId: new("fill-" + strconv.Itoa(e.sent)),
		}

		_, err := e.provider.SendMessage(e.ctx, in)
		if err != nil {
			e.b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotLists saves a FIFO queue with a ready group for each of its
// messages, and a State with a completed move task for each of its tasks. A
// Provider holds a move task only after a move has run, so that row fills it by
// restoring a file.
func BenchmarkSnapshotLists(b *testing.B) {
	messages := strconv.Itoa(listMessages)
	rows := []struct {
		name string
		fill func(e *benchEnv)
	}{
		{"queues=1/messages=" + messages + "/queue=fifo/groups=" + messages, func(e *benchEnv) {
			e.fillGroups(e.queue(queueFIFO), listMessages)
		}},
		{"queues=0/movetasks=" + strconv.Itoa(listTasks), func(e *benchEnv) {
			err := e.provider.Restore(strings.NewReader(moveTaskFile(listTasks)))
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
