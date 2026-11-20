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

// snapshotCases are the item counts at each item size. A million items of
// 4 KiB would be 4 GB, so the large size stops at 100,000.
var snapshotCases = []snapshotCase{
	{1_000, itemPayloads[0]},
	{100_000, itemPayloads[0]},
	{1_000_000, itemPayloads[0]},
	{1_000, itemPayloads[1]},
	{100_000, itemPayloads[1]},
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
	for _, c := range snapshotCases {
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
}

func BenchmarkRestore(b *testing.B) {
	for _, c := range snapshotCases {
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
