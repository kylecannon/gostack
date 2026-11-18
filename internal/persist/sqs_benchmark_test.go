package persist_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	sqsservice "github.com/kylecannon/gostack/internal/service/sqs"
)

// The persistence matrix: saving and loading the whole SQS store, which is what
// PERSIST_SQS=1 does on every flush and at start (docs/standards.md,
// "Benchmarks"). The axes are the number of queues and the messages in each,
// with 64KiB bodies. It lives here, not with SQS, so that `make bench` runs it
// beside the SQS message matrix and not after it.
//
// The messages per queue are not 1000 everywhere. A snapshot is one json
// document held in memory twice over, so 1000 messages of 64KiB in each of 10
// queues is 640 MiB of bodies and 1000 queues of them is 62 GiB. Each cell
// that holds messages holds about 62 MiB of bodies instead:
//
//	queues=10    messages=100   (a few busy queues)
//	queues=1000  messages=1     (many queues, each with something in it)
//
// queues=1000 with messages=100 is dropped. It runs the same code as the cell
// above it, 100 times larger.
var snapshotCells = []struct{ queues, messages int }{
	{10, 0},
	{1000, 0},
	{10, 100},
	{1000, 1},
}

const (
	snapshotBodyBytes = 64 << 10
	snapshotAccount   = "000000000000"
	snapshotRegion    = "us-east-1"
)

// directCall is a context the provider takes as a call that came through the
// gateway.
func directCall(ctx context.Context) context.Context {
	rc := &request.Context{
		Service: "sqs", Protocol: protocol.KindJSON, AccountID: snapshotAccount, Region: snapshotRegion, RequestID: "r",
		Request: httptest.NewRequestWithContext(ctx, http.MethodPost, "/", nil),
	}

	return request.With(ctx, rc)
}

// filledProvider returns a provider holding the cell's queues and messages.
func filledProvider(b *testing.B, queues, messages int) *sqsservice.Provider {
	b.Helper()

	p := sqsservice.New(nil)
	ctx := directCall(b.Context())
	body := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", snapshotBodyBytes/36+1)[:snapshotBodyBytes]

	b.Cleanup(p.Close)

	for i := range queues {
		created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("snapshot-" + strconv.Itoa(i))})
		if err != nil {
			b.Fatal(err)
		}

		for range messages {
			_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: &body})
			if err != nil {
				b.Fatal(err)
			}
		}
	}

	return p
}

func snapshotName(queues, messages int) string {
	name := "queues=" + strconv.Itoa(queues) + "/messages=" + strconv.Itoa(messages)
	if messages > 0 {
		name += "/body=64KiB"
	}

	return name
}

func BenchmarkSQSSnapshot(b *testing.B) {
	for _, c := range snapshotCells {
		b.Run(snapshotName(c.queues, c.messages), func(b *testing.B) {
			p := filledProvider(b, c.queues, c.messages)

			var out bytes.Buffer

			err := p.Snapshot(&out)
			if err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(out.Len()))

			for b.Loop() {
				err = p.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQSRestore(b *testing.B) {
	for _, c := range snapshotCells {
		b.Run(snapshotName(c.queues, c.messages), func(b *testing.B) {
			var saved bytes.Buffer

			err := filledProvider(b, c.queues, c.messages).Snapshot(&saved)
			if err != nil {
				b.Fatal(err)
			}

			p := sqsservice.New(nil)
			b.Cleanup(p.Close)

			b.ReportAllocs()
			b.SetBytes(int64(saved.Len()))

			for b.Loop() {
				err = p.Restore(bytes.NewReader(saved.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
