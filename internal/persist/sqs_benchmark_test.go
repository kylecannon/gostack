package persist_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	sqsservice "github.com/kylecannon/gostack/internal/service/sqs"
)

// snapshotCell is one cell of the persistence matrix.
type snapshotCell struct{ queues, messages int }

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
var snapshotCells = []snapshotCell{
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

// filledProvider returns a provider holding the cell's queues and messages. It
// lives until the benchmark ends.
func filledProvider(b *testing.B, queues, messages int) *sqsservice.Provider {
	b.Helper()

	p := newFilledProvider(b, queues, messages)

	b.Cleanup(p.Close)

	return p
}

// newFilledProvider is filledProvider for a caller that closes the provider
// itself: a closure registered with b.Cleanup keeps the provider and every body
// it holds reachable until the benchmark ends.
func newFilledProvider(b *testing.B, queues, messages int) *sqsservice.Provider {
	b.Helper()

	p := sqsservice.New(nil, nil)
	ctx := directCall(b.Context())
	body := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", snapshotBodyBytes/36+1)[:snapshotBodyBytes]

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

// BenchmarkSQSFlushWrite saves the whole SQS store through the engine, as a
// flush does, to a directory where fsync costs nothing: a tmpfs under /dev/shm,
// or the temp dir where the host has none (macOS, or a /dev/shm too small for
// the cell). It measures the writer: the snapshot, its encoding and the file
// writes, and the engine's own work around them.
//
// The cells that hold messages write about 64 MiB per operation. On a disk each
// operation also fsyncs it, and that wait is the disk's: the first fsyncs after
// a quiet period are cheap and later ones are not, so sec/op fell as the
// iteration count rose and a faster writer, which gets more iterations from the
// same -benchtime, read as slower (SQ-489: +17% to +28% for the same code at a
// fixed count). Memory has no such state, so sec/op here is the same at -benchtime
// 4x, 8x and 16x. BenchmarkSQSFlushFsync keeps the fsync path.
//
// Set beside BenchmarkSQSSnapshot, its B/op shows what the engine holds on top
// of the snapshot itself.
func BenchmarkSQSFlushWrite(b *testing.B) {
	benchFlush(b, snapshotCells, memoryDir)
}

// BenchmarkSQSFlushFsync is BenchmarkSQSFlushWrite on the temp dir, so each
// flush fsyncs its file and the directory. It runs the cells with no messages,
// whose file is small enough for the disk to take it at one cost whatever the
// iteration count. The cells that hold messages are left to the row above: their
// fsync time is how fast the runner's disk takes 64 MiB, and that is not this
// code.
func BenchmarkSQSFlushFsync(b *testing.B) {
	benchFlush(b, fsyncCells, func(b *testing.B, _ int64) string { return b.TempDir() })
}

// fsyncCells are the cells of snapshotCells that hold no messages.
var fsyncCells = slices.DeleteFunc(slices.Clone(snapshotCells), func(c snapshotCell) bool { return c.messages > 0 })

// memoryFS is the tmpfs Linux mounts by default.
const memoryFS = "/dev/shm"

// memoryDir returns a directory with room for need bytes on a memory
// filesystem, and the temp dir when there is none. It checks the room by
// writing need bytes, since a container's /dev/shm is 64 MiB by default and a
// flush that finds it full fails.
func memoryDir(b *testing.B, need int64) string {
	b.Helper()

	dir, err := os.MkdirTemp(memoryFS, "gostack-bench-") //nolint:usetesting // b.TempDir cannot be placed under /dev/shm
	if err != nil {
		b.Logf("no %s (%v): flushes go to the temp dir and fsync", memoryFS, err)

		return b.TempDir()
	}

	b.Cleanup(func() { _ = os.RemoveAll(dir) })

	if !hasRoom(dir, need) {
		b.Logf("%s has no room for %d bytes: flushes go to the temp dir and fsync", memoryFS, need)

		return b.TempDir()
	}

	return dir
}

// hasRoom reports whether dir takes need bytes.
func hasRoom(dir string, need int64) bool {
	f, err := os.CreateTemp(dir, "probe-")
	if err != nil {
		return false
	}

	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()

	chunk := make([]byte, 1<<20)

	for written := int64(0); written < need; written += int64(len(chunk)) {
		_, err = f.Write(chunk)
		if err != nil {
			return false
		}
	}

	return true
}

// flushSpace is the room a flush of the cell needs: its new file is written
// beside the old one, then renamed over it.
func flushSpace(queues, messages int) int64 {
	return 2*int64(queues)*int64(messages)*snapshotBodyBytes + 32<<20
}

func benchFlush(b *testing.B, cells []snapshotCell, dirFor func(b *testing.B, need int64) string) {
	for _, c := range cells {
		b.Run(snapshotName(c.queues, c.messages), func(b *testing.B) {
			p := filledProvider(b, c.queues, c.messages)
			base := dirFor(b, flushSpace(c.queues, c.messages))

			e := benchEngine(base, time.Hour)
			e.Register(sqs, p)

			flush := func() {
				e.MarkDirty(sqs)

				err := e.Flush(b.Context())
				if err != nil {
					b.Fatal(err)
				}
			}

			flush()

			// pad keeps this closure's frame at 0xc8 bytes (go tool objdump on a
			// linux/amd64 test binary shows SUBQ $0xc8, SP), which is the stack
			// alignment main's benchmark had. encoding/json/v2's string quoting
			// runs about 10% slower or faster on AMD Zen 3 and Zen 4 depending
			// on stack alignment, so without the pad this benchmark measures
			// where its frame lands instead of the code under test. Only this
			// benchmark carries it; no production frame does.
			var pad [1]byte

			pad[len(base)%len(pad)] = 1

			if pad[0] > 1 {
				b.Fatal("unreachable")
			}

			info, err := os.Stat(filepath.Join(base, sqs, jsonFile))
			if err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.SetBytes(info.Size())

			for b.Loop() {
				flush()
			}
		})
	}
}

// BenchmarkSQSRestoreExact loads the whole SQS store from the bytes of its
// snapshot, as the start of a process with PERSIST_SQS=1 does.
//
// The input is a slice of exactly the snapshot's length, and the heap is
// collected before the timer starts. The Snapshot that builds the input grows a
// bytes.Buffer, whose capacity depends on how the encoder writes: 144 MiB for
// one encoder and 72 MiB for another, for the same 62 MiB of snapshot. Left as
// it was, the heap at the first Restore was 217 MiB for one and 109 MiB for the
// other, and the collector's pacing follows the live heap, so the row measured
// how the input was built (SQ-489). The provider that built it is closed and
// dropped before the timer for the same reason.
func BenchmarkSQSRestoreExact(b *testing.B) {
	for _, c := range snapshotCells {
		b.Run(snapshotName(c.queues, c.messages), func(b *testing.B) {
			saved := snapshotBytes(b, c.queues, c.messages)

			p := sqsservice.New(nil, nil)
			b.Cleanup(p.Close)

			runtime.GC()

			b.ReportAllocs()
			b.SetBytes(int64(len(saved)))

			for b.Loop() {
				err := p.Restore(bytes.NewReader(saved))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// snapshotBytes returns the snapshot of the cell's provider in a slice with no
// spare capacity. The provider and the buffer that held the snapshot are
// garbage when it returns.
func snapshotBytes(b *testing.B, queues, messages int) []byte {
	b.Helper()

	p := newFilledProvider(b, queues, messages)

	var buf bytes.Buffer

	err := p.Snapshot(&buf)
	if err != nil {
		b.Fatal(err)
	}

	p.Close()

	saved := make([]byte, buf.Len())
	copy(saved, buf.Bytes())

	return saved
}
