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
// that holds messages holds 25 or 16 MiB of bodies instead, what one snapshot
// takes in a time the gate can compare (docs/standards.md, "Benchmarks";
// queues=10 messages=100 took 11.0 ms and queues=1000 messages=1 12.3 ms, the
// slowest of three runs):
//
//	queues=10    messages=40   (a few busy queues, 25 MiB, 4.4 ms)
//	queues=250   messages=1    (many queues, each with something in it, 16 MiB, 3.1 ms)
//
// queues=1000 with messages=100 is dropped. It runs the same code as the cell
// above it, 100 times larger.
var snapshotCells = []snapshotCell{
	{10, 0},
	{1000, 0},
	{10, 40},
	{250, 1},
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

// roundTripCells are the cells of BenchmarkSQSFlushWrite and
// BenchmarkSQSRestoreExact: snapshotCells cut down to what the gate can compare.
// The cells that held messages in snapshotCells before it was cut held 62 MiB of
// bodies, and an operation on them took 61 to 166 ms (docs/standards.md,
// "Benchmarks"). These hold 2.5 MiB, the 64KiB payload kept:
//
//	queues=10    messages=4    (a few busy queues)
//	queues=40    messages=1    (many queues, each with something in it)
//
// The cells left out are queues=10 messages=100 and queues=1000 messages=1,
// which BenchmarkSQSSnapshot still runs at the sizes of snapshotCells. An
// operation took at most 3.3 ms of flush (queues=1000 messages=0) and 4.3 ms of
// restore (queues=40 messages=1) on tmpfs, and the slowest of three runs on an
// ext4 volume was 6.7 ms for BenchmarkSQSFlushFsync (queues=250 messages=0, the
// fsync of the snapshot and its directory), 4.1 ms for BenchmarkSQSRestoreExact
// (queues=40 messages=1) and 2.2 ms for BenchmarkSQSFlushWrite (queues=1000
// messages=0).
//
// The gate's runners (linux/amd64, 4 cpus: AMD EPYC 7763 and 9V45, Intel Xeon
// 6975P-C; 8 hosts, 8 runs each at -benchtime=500ms, verification/SQ-466/
// amd-558134a) timed queues=1000 messages=0 under the names these cells had
// before: restore took 2.48 to 6.49 ms at 94 or more iterations, and flush, then
// on the runner's disk with its fsync, 3.09 to 6.69 ms at 79 or more. Neither
// was over 10 ms, and the restore time is above the arm64 figure. queues=10
// messages=4 and queues=40 messages=1 have no x86 time: they did not exist.
var roundTripCells = []snapshotCell{
	{10, 0},
	{1000, 0},
	{10, 4},
	{40, 1},
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
// the cell). The row name ends in fs=tmpfs or fs=disk, whichever the cell ran
// on, so a cell that fell back is never read as one that did not. It measures
// the writer: the snapshot, its encoding and the file writes, and the engine's
// own work around them.
//
// On a disk each flush also fsyncs, and that wait is the disk's: the first
// fsyncs after a quiet period are cheap and later ones are not, so with a lot
// of data sec/op fell as the iteration count rose and a faster writer, which
// gets more iterations from the same -benchtime, read as slower (SQ-489: +17%
// to +28% for the same code at a fixed count). Memory has no such state.
// BenchmarkSQSFlushFsync keeps the fsync path.
func BenchmarkSQSFlushWrite(b *testing.B) {
	for _, c := range roundTripCells {
		dir, fs := memoryDir(b, flushSpace(c))
		benchFlush(b, c, dir, "/fs="+fs)
	}
}

// BenchmarkSQSFlushFsync is BenchmarkSQSFlushWrite on the temp dir, so each
// flush fsyncs its file and the directory. It runs cells with no messages,
// whose file is small enough for the disk to take it at one cost whatever the
// iteration count. The cells that hold messages are left to the row above: their
// fsync time is how fast the runner's disk takes their data, and that is not
// this code.
//
// queues=1000 is left out for it (docs/standards.md, "Benchmarks"): on the disk
// of a Docker VM an operation took 7.0 to 8.1 ms, too near the limit for a
// runner with a slower disk. The two below took 1.7 to 9.2 ms (queues=10, a 3.9
// KB file) and 2.4 to 5.4 ms (queues=250) there, in 9 runs with nothing else
// using the disk. Those are that VM's disk, which swung that much between runs;
// no cell is smaller than queues=10, and queues=10 is kept at its size on main.
//
// The gate's runners (linux/amd64, 8 hosts, 8 runs each at -benchtime=500ms,
// verification/SQ-466/amd-558134a) timed queues=10 under its old name,
// BenchmarkSQSFlush queues=10/messages=0, which wrote to b.TempDir as this does
// (eb8c43e6): 0.31 to 1.08 ms at 658 or more iterations, 0.30 to 0.90 ms with
// the chunked writer. queues=250 has no x86 time. queues=1000 took 3.09 to 6.69
// ms at 79 or more iterations on the runners, and is left out for the arm64
// time above.
func BenchmarkSQSFlushFsync(b *testing.B) {
	for _, c := range fsyncCells {
		benchFlush(b, c, b.TempDir(), "")
	}
}

var fsyncCells = []snapshotCell{{10, 0}, {250, 0}}

// memoryFS is the tmpfs Linux mounts by default.
const memoryFS = "/dev/shm"

// memoryDir returns a directory with room for need bytes on a memory
// filesystem and "tmpfs", or the temp dir and "disk" when there is none. It
// checks the room by writing need bytes, since a container's /dev/shm is 64 MiB
// by default and a flush that finds it full fails.
//
// "tmpfs" means the directory is under /dev/shm, not that the filesystem was
// checked: a volume mounted at /dev/shm reads fs=tmpfs and is not memory.
func memoryDir(b *testing.B, need int64) (dir, fs string) {
	b.Helper()

	shm, err := os.MkdirTemp(memoryFS, "gostack-bench-") //nolint:usetesting // b.TempDir cannot be placed under /dev/shm
	if err != nil {
		b.Logf("no %s (%v): flushes go to the temp dir and fsync", memoryFS, err)

		return b.TempDir(), "disk"
	}

	// The cleanup holds shm, so a return of the temp dir below leaves it alone.
	b.Cleanup(func() { _ = os.RemoveAll(shm) })

	if !hasRoom(shm, need) {
		b.Logf("%s has no room for %d bytes: flushes go to the temp dir and fsync", memoryFS, need)

		return b.TempDir(), "disk"
	}

	return shm, "tmpfs"
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
func flushSpace(c snapshotCell) int64 {
	return 2*int64(c.queues)*int64(c.messages)*snapshotBodyBytes + 8<<20
}

// benchFlush runs one cell of a flush benchmark with its state under dir. The
// cell's name is the matrix name and suffix.
//
// The closure it runs holds this benchmark's hot loop in the stack. encoding/json/v2's
// string quoting runs about 10% slower or faster on AMD Zen 3 and Zen 4 depending
// on where that frame sits (SQ-455, SQ-460). The frame is 0xb0 bytes on
// linux/amd64 (go1.27.4): `GOOS=linux GOARCH=amd64 go test -c -o persist.test
// ./internal/persist && go tool objdump -s 'benchFlush\.func1$' persist.test`
// prints `SUBQ $0xb0, SP`. Nothing holds it there: a local added to the closure
// moves it, and a comparison of two commits that differ in it is a comparison of
// frames.
func benchFlush(b *testing.B, c snapshotCell, dir, suffix string) {
	b.Run(snapshotName(c.queues, c.messages)+suffix, func(b *testing.B) {
		p := filledProvider(b, c.queues, c.messages)

		e := benchEngine(dir, time.Hour)
		e.Register(sqs, p)

		flush := func() {
			e.MarkDirty(sqs)

			err := e.Flush(b.Context())
			if err != nil {
				b.Fatal(err)
			}
		}

		flush()

		info, err := os.Stat(filepath.Join(dir, sqs, jsonFile))
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

// BenchmarkSQSRestoreExact loads the whole SQS store from the bytes of its
// snapshot, as the start of a process with PERSIST_SQS=1 does.
//
// The input is a slice of exactly the snapshot's length, and the heap is
// collected before the timer starts. The Snapshot that builds the input grows a
// bytes.Buffer, whose capacity depends on how the encoder writes. At the 62 MiB
// of snapshot the cells had before roundTripCells, that was 144 MiB for one
// encoder and 72 MiB for another, and the heap at the first Restore 217 MiB for
// one and 109 MiB for the other. The collector's pacing follows the live heap,
// so the row measured how the input was built (SQ-489); the copy and the
// collection were both needed to equalize it at that size. The cells are
// smaller now. At 2.5 MiB the copy equalizes nothing between encoders, and the
// collection is what equalizes the start; the copy stays as a guard against an
// encoder that grows the buffer differently. The provider that built the input is closed and
// dropped before the timer for the same reason.
func BenchmarkSQSRestoreExact(b *testing.B) {
	for _, c := range roundTripCells {
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
