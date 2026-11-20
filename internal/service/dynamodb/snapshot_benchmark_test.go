package dynamodb_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// The persistence rows of the DynamoDB matrix (docs/standards.md,
// "Benchmarks", axis "Persistence"): PutItem and UpdateItem under persist=on
// (items_benchmark_test.go, reads_benchmark_test.go), and the snapshot and
// restore of one table by item count and item size.

const (
	persistOn = "persist=on"
	// persistFrequency is the flush delay of the engine: short enough that a
	// save is always running or due while the workers write.
	persistFrequency = 10 * time.Millisecond
)

// persistedStack is newStack with DynamoDB persisted under a temp dir, as
// PERSIST_DYNAMODB=1 wires it. It checks at the end that a snapshot reached
// the disk, so a row cannot pass with persistence off.
func persistedStack(tb testing.TB) *stack {
	tb.Helper()

	dir := tb.TempDir()
	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: persistFrequency,
		MountDetector: func(string) bool { return false },
	})

	s := &stack{}
	s.settings.Store(&dynamodb.Settings{Host: testHost})
	s.provider = dynamodb.New(func() dynamodb.Settings { return *s.settings.Load() }, eng.MarkDirty)
	reg := s.provider.Registration()
	eng.Register(ddbName, reg.Persist)
	require.NoError(tb, eng.Load(tb.Context()))

	tb.Cleanup(func() {
		err := eng.Close(context.WithoutCancel(tb.Context()))
		if err != nil {
			tb.Errorf("close the engine: %v", err)
		}

		_, err = os.Stat(filepath.Join(dir, ddbName, "store.json"))
		if err != nil {
			tb.Errorf("DynamoDB saved no snapshot: %v", err)
		}
	})

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{reg}, nil, eng,
	)
	require.NoError(tb, err)

	s.handler = gw.Handler()

	return s
}

// snapshotCase is one row of the snapshot matrix.
type snapshotCase struct {
	items   int
	payload itemPayload
}

// largeEnv, set to 1, adds the rows too slow for the benchstat gate, which
// judges ten runs of each row: every op of these takes 0.1 s to 2 s.
// docs/architecture.md (Persistence, "DynamoDB") quotes the million-item rows.
const largeEnv = "GOSTACK_BENCH_LARGE"

// snapshotCases are the item counts at each item size, each op under 50 ms.
// The large cases add 100,000 and 1,000,000 items of 100 B and 100,000 of
// 4 KiB; a million items of 4 KiB would be 4 GB.
func snapshotCases() []snapshotCase {
	cases := []snapshotCase{
		{1_000, itemPayloads[0]},
		{10_000, itemPayloads[0]},
		{1_000, itemPayloads[1]},
	}

	if os.Getenv(largeEnv) == "1" {
		cases = append(cases,
			snapshotCase{100_000, itemPayloads[0]},
			snapshotCase{1_000_000, itemPayloads[0]},
			snapshotCase{100_000, itemPayloads[1]},
		)
	}

	return cases
}

func (c snapshotCase) name() string {
	return fmt.Sprintf("items=%d/payload=%s", c.items, c.payload.name)
}

// generated is a DynamoDB snapshot of one table, keyed by id, with a global
// index on g, holding count items, each with a d of size bytes.
func generated(count, size int) []byte {
	var buf bytes.Buffer

	buf.WriteString(`{"v":1,"accounts":[],"states":[{"account":"000000000000","region":"us-east-1","tables":[`)
	buf.WriteString(`{"meta":{"Name":"bench","Attributes":[{"AttributeName":"id","AttributeType":"S"},` +
		`{"AttributeName":"g","AttributeType":"S"}],"Key":[{"AttributeName":"id","KeyType":"HASH"}],` +
		`"PayPerRequest":true,"Global":[{"Name":"by-g","Key":[{"AttributeName":"g","KeyType":"HASH"}],` +
		`"Projection":{"ProjectionType":"KEYS_ONLY"}}]},"items":[`)

	d := strings.Repeat("x", size)

	for i := range count {
		if i > 0 {
			buf.WriteByte(',')
		}

		fmt.Fprintf(&buf, `{"id":{"S":"k%08d"},"g":{"S":"g%d"},"n":{"N":"%d"},"d":{"S":"%s"}}`, i, i%100, i, d)
	}

	buf.WriteString(`]}]}]}`)

	return buf.Bytes()
}

// restoredProvider is a provider holding the generated table.
func restoredProvider(b *testing.B, file []byte) *dynamodb.Provider {
	b.Helper()

	p := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
	require.NoError(b, p.Restore(bytes.NewReader(file)))

	return p
}

func BenchmarkSnapshot(b *testing.B) {
	for _, c := range snapshotCases() {
		b.Run(c.name(), func(b *testing.B) {
			p := restoredProvider(b, generated(c.items, c.payload.size))

			var counter countingWriter
			require.NoError(b, p.Snapshot(&counter))
			b.SetBytes(counter.n)
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, p.Snapshot(io.Discard))
			}
		})
	}

	// The table snapshot of tables with stream records: with the streams
	// persisted its cut keeps their records for the streams snapshot.
	for _, c := range streamCases() {
		b.Run(fmt.Sprintf("items=1000/tables=%d/%s", streamTables, c.name()), func(b *testing.B) {
			p := streamedProvider(b, 1_000, c)
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, p.Snapshot(io.Discard))
			}
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, c := range snapshotCases() {
		b.Run(c.name(), func(b *testing.B) {
			file := generated(c.items, c.payload.size)
			p := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)

			b.SetBytes(int64(len(file)))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, p.Restore(bytes.NewReader(file)))
			}

			// The restore allocation bound: what the restored state holds,
			// plus the file once. Measured after the loop, whose timing its
			// collections would otherwise skew.
			b.StopTimer()

			held := heldBytes(b, file)
			b.ReportMetric(float64(held), "held-B")
			b.ReportMetric(float64(held+int64(len(file))), "bound-B")
		})
	}
}

// heldBytes restores file into a new provider and returns the heap the
// restored state holds once the collector has run: the in-memory floor no
// decoder can go under, since every byte of it is the state itself.
func heldBytes(b *testing.B, file []byte) int64 {
	b.Helper()

	var before, after runtime.MemStats

	p := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
	require.NoError(b, p.Restore(bytes.NewReader([]byte(`{"v":1,"accounts":[],"states":[]}`))))
	collect()
	runtime.ReadMemStats(&before)

	require.NoError(b, p.Restore(bytes.NewReader(file)))
	collect()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(p)
	runtime.KeepAlive(file) // else the collector frees it between the two reads

	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

// collect runs the collector twice, so the heap holds only what is reachable:
// the second empties what sync.Pool kept from the first.
func collect() {
	runtime.GC() //nolint:revive // heldBytes reads the heap the state holds
	runtime.GC() //nolint:revive // heldBytes reads the heap the state holds
}

// countingWriter counts the bytes written to it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))

	return len(p), nil
}

// BenchmarkSnapshotCut is the span a snapshot holds its locks for: one copy
// of every table at one moment, four tables of the item count between them.
// A write to any table waits for it at most. docs/architecture.md
// (Persistence, "DynamoDB") quotes the million-item row.
func BenchmarkSnapshotCut(b *testing.B) {
	counts := []int{10_000}
	if os.Getenv(largeEnv) == "1" {
		counts = append(counts, 1_000_000)
	}

	const tables = 4

	for _, count := range counts {
		b.Run(fmt.Sprintf("items=%d/tables=%d", count, tables), func(b *testing.B) {
			p := restoredProvider(b, generatedTables(tables, count/tables, itemPayloads[0].size))

			for b.Loop() {
				require.NoError(b, dynamodb.Cut(p))
			}
		})
	}

	for _, c := range streamCases() {
		b.Run(fmt.Sprintf("items=10000/tables=%d/%s", streamTables, c.name()), func(b *testing.B) {
			p := streamedProvider(b, 10_000, c)
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, dynamodb.Cut(p))
			}
		})
	}
}

// BenchmarkSnapshotStreams is one flush of DynamoDB and DynamoDB Streams,
// both persisted, with stream records: the table snapshot, its commit, then
// the streams snapshot, as the engine saves the pair. The 100,000-record row
// is a large one.
func BenchmarkSnapshotStreams(b *testing.B) {
	for _, c := range streamCases() {
		if !c.persisted || c.records > 1_000 && os.Getenv(largeEnv) != "1" {
			continue
		}

		b.Run(fmt.Sprintf("items=1000/tables=%d/%s", streamTables, c.name()), func(b *testing.B) {
			p := streamedProvider(b, 1_000, c)
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, p.Snapshot(io.Discard))

				if committer, ok := any(p).(interface{ Committed() }); ok {
					committer.Committed()
				}

				require.NoError(b, p.SnapshotStreams(io.Discard))
			}
		})
	}
}

// streamTables is the tables of the stream rows, each with a KEYS_ONLY stream.
const streamTables = 4

// streamCase is a row with stream records: records across the shards of
// streamTables tables, and whether persistence saves the streams, as
// PERSIST_DYNAMODBSTREAMS sets it.
type streamCase struct {
	records   int
	persisted bool
}

// streamCases are 1,000 and 100,000 records, with the streams saved and not.
func streamCases() []streamCase {
	return []streamCase{{1_000, true}, {1_000, false}, {100_000, true}, {100_000, false}}
}

func (c streamCase) name() string {
	streams := "off"
	if c.persisted {
		streams = "on"
	}

	return fmt.Sprintf("records=%d/streams=%s", c.records, streams)
}

// streamedProvider is a provider holding streamTables tables, with the given
// number of items between them, each with a KEYS_ONLY stream holding its share of c.records,
// written now so retention keeps them. It is told whether a follower saves the
// streams, as persistence tells it before each table snapshot.
func streamedProvider(b *testing.B, items int, c streamCase) *dynamodb.Provider {
	b.Helper()

	created := time.Now().UTC()
	at := created.Format(time.RFC3339Nano)
	each := c.records / streamTables

	file := string(generatedTables(streamTables, items/streamTables, itemPayloads[0].size))
	file = strings.ReplaceAll(file, `"PayPerRequest":true,`, `"PayPerRequest":true,`+
		`"Stream":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"},"StreamLabel":"2026-01-01T00:00:00.000",`)
	file = strings.ReplaceAll(file, `}]},"items":[`, fmt.Sprintf(
		`}]},"shard":{"created":%q,"low":1,"next":%d,"viewType":"KEYS_ONLY","region":"us-east-1"},"items":[`,
		at, each+1))

	var streams bytes.Buffer

	streams.WriteString(`{"v":1,"shards":[`)

	for t := range streamTables {
		if t > 0 {
			streams.WriteByte(',')
		}

		fmt.Fprintf(&streams, `{"account":"000000000000","region":"us-east-1","table":"bench-%d","created":%q,"records":[`,
			t, at)

		for i := range each {
			if i > 0 {
				streams.WriteByte(',')
			}

			fmt.Fprintf(&streams, `{"seq":%d,"event":"INSERT","keys":{"id":{"S":"k%08d"}},"size":14,"at":%q}`,
				i+1, i%(items/streamTables), at)
		}

		streams.WriteString(`]}`)
	}

	streams.WriteString(`]}`)

	p := restoredProvider(b, []byte(file))
	require.NoError(b, p.RestoreStreams(&streams))

	for t := range streamTables {
		shard := dynamodb.Shard(p, "us-east-1", fmt.Sprintf("bench-%d", t))
		require.Len(b, shard.Records(), each, "the shard holds its records")
	}

	if leader, ok := any(p).(interface{ Followed(saved bool) }); ok {
		leader.Followed(c.persisted)
	}

	return p
}

// generatedTables is generated's snapshot with n tables of count items
// each, named bench-0 on.
func generatedTables(n, count, size int) []byte {
	one := string(generated(count, size))
	start := strings.Index(one, `{"meta":`)
	table := one[start : len(one)-len(`]}]}`)]

	parts := make([]string, n)
	for i := range parts {
		parts[i] = strings.Replace(table, `"Name":"bench"`, fmt.Sprintf(`"Name":"bench-%d"`, i), 1)
	}

	return []byte(one[:start] + strings.Join(parts, ",") + `]}]}`)
}
