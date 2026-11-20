package dynamodb_test

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/stretchr/testify/require"
)

// The table and streams files of one flush, saved by the persistence engine
// with the real providers, built from the SQ-363 probes (SQ-365 A to C). Each
// case restores the two files into a fresh engine and counts the items and
// stream records of table tbl, which three puts left with {3, 3}.

var errTableDisk = errors.New("test: the table file's disk")

// hookedTables is DynamoDB's snapshotter with a change that lands after its
// cut and before the streams save of the same flush, and a failure of the
// table save after its cut.
type hookedTables struct {
	p     *dynamodb.Provider
	after func()
	fail  bool
}

func (h *hookedTables) Snapshot(w io.Writer) error {
	err := h.p.Snapshot(w)

	if h.after != nil {
		h.after()
		h.after = nil
	}

	if h.fail {
		return errTableDisk
	}

	return err //nolint:wrapcheck // a test double
}

func (h *hookedTables) Restore(r io.Reader) error {
	return h.p.Restore(r) //nolint:wrapcheck // a test double
}

// Committed passes persistence's commit on to the provider.
func (h *hookedTables) Committed() {
	if c, ok := any(h.p).(interface{ Committed() }); ok {
		c.Committed()
	}
}

// Followed passes on whether a streams save follows.
func (h *hookedTables) Followed(saved bool) {
	h.p.Followed(saved)
}

// pairStack is a stack whose DynamoDB and DynamoDB Streams persist in dir,
// loaded from it.
func pairStack(t *testing.T, dir string) (*stack, *persist.Engine, *hookedTables) {
	t.Helper()

	return tableStack(t, dir, true)
}

// tableStack is a stack whose DynamoDB persists in dir, loaded from it, and
// whose DynamoDB Streams persists too when streams is set.
//
//nolint:revive // flag-parameter: streams mirrors PERSIST_DYNAMODBSTREAMS, the setting under test.
func tableStack(t *testing.T, dir string, streams bool) (*stack, *persist.Engine, *hookedTables) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour,
		MountDetector: func(string) bool { return false },
	})

	s := &stack{}
	s.settings.Store(&dynamodb.Settings{Host: testHost})
	s.provider = dynamodb.New(func() dynamodb.Settings { return *s.settings.Load() }, eng.MarkDirty)
	reg := s.provider.Registration()
	tables := &hookedTables{p: s.provider}
	eng.Register(ddbName, tables)

	if streams {
		eng.Register(streamsName, dynamodbstreams.New(s.provider).Registration().Persist)
	}
	require.NoError(t, eng.Load(t.Context()))

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{reg}, nil, eng,
	)
	require.NoError(t, err)

	s.handler = gw.Handler()

	return s, eng, tables
}

const keysOnly = `"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"}`

// pairSetup is table tbl with a stream and three puts.
func pairSetup(t *testing.T, s *stack) {
	t.Helper()

	s.ok(t, opCreate, hashTable("tbl"))
	s.ok(t, opUpdate, `{"TableName":"tbl",`+keysOnly+`}`)

	for _, id := range []string{"a", "b", "c"} {
		s.ok(t, opPut, `{"TableName":"tbl","Item":{"id":{"S":"`+id+`"}}}`)
	}
}

// restoredCounts loads dir into a fresh stack and returns the items and
// stream records of tbl.
func restoredCounts(t *testing.T, dir string) []int {
	t.Helper()

	s, _, _ := pairStack(t, dir)
	scan := s.ok(t, opScan, `{"TableName":"tbl"}`)
	count, _ := scan["Count"].(float64)

	shard := dynamodb.Shard(s.provider, usEast1, "tbl")
	if shard == nil {
		return []int{int(count), -1}
	}

	return []int{int(count), len(shard.Records())}
}

// Test_persist_streamsHoldTheCutsRecords: SQ-363 rows A to C. A DeleteTable,
// a stream disabled and enabled again, or a reset lands after the table
// snapshot's cut and before the streams save of the same flush. Each drops
// the shard's records; the streams file still holds the records of the writes
// the table file holds.
func Test_persist_streamsHoldTheCutsRecords(t *testing.T) {
	for name, between := range map[string]func(*testing.T, *stack){
		"table deleted": func(t *testing.T, s *stack) { s.ok(t, opDelete, `{"TableName":"tbl"}`) },
		"stream disabled and enabled again": func(t *testing.T, s *stack) {
			s.ok(t, opUpdate, `{"TableName":"tbl","StreamSpecification":{"StreamEnabled":false}}`)
			time.Sleep(5 * time.Millisecond) // a new shard's identity is its creation time
			s.ok(t, opUpdate, `{"TableName":"tbl",`+keysOnly+`}`)
		},
		"reset": func(_ *testing.T, s *stack) { s.provider.Registration().Reset("", "") },
		// The byte bound trims the oldest records, zeroing each in place, as
		// retention does.
		"byte bound trims the records": func(t *testing.T, s *stack) {
			shard := dynamodb.Shard(s.provider, usEast1, "tbl")
			shard.SetLimits(stream.Retention, 1)
			s.ok(t, opPut, `{"TableName":"tbl","Item":{"id":{"S":"d"}}}`)
			require.Len(t, shard.Records(), 1, "the bound trimmed every record but the last")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, eng, tables := pairStack(t, dir)
			pairSetup(t, s)

			tables.after = func() { between(t, s) }
			require.NoError(t, eng.Flush(t.Context()))
			require.Equal(t, []int{3, 3}, restoredCounts(t, dir))
		})
	}
}

// Test_persist_failedTableSaveKeepsThePair: SQ-363 rows D and E (finding 7).
// The table save fails after its cut, with the table deleted, or deleted,
// created again and written, since the last good pair. The streams file of
// that pair is kept, so the pair restores as saved.
func Test_persist_failedTableSaveKeepsThePair(t *testing.T) {
	for name, since := range map[string]func(*testing.T, *stack){
		"table deleted": func(t *testing.T, s *stack) { s.ok(t, opDelete, `{"TableName":"tbl"}`) },
		"table created again and written": func(t *testing.T, s *stack) {
			s.ok(t, opDelete, `{"TableName":"tbl"}`)
			time.Sleep(5 * time.Millisecond)
			s.ok(t, opCreate, hashTable("tbl"))
			s.ok(t, opUpdate, `{"TableName":"tbl",`+keysOnly+`}`)
			s.ok(t, opPut, `{"TableName":"tbl","Item":{"id":{"S":"z"}}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, eng, tables := pairStack(t, dir)
			pairSetup(t, s)
			require.NoError(t, eng.Flush(t.Context()))

			since(t, s)
			tables.fail = true
			require.ErrorIs(t, eng.Flush(t.Context()), errTableDisk)
			require.Equal(t, []int{3, 3}, restoredCounts(t, dir))
		})
	}
}

// Test_persist_streamsHoldRecordsRetentionDropped: retention drops the
// shard's records, zeroing each in place, after the table snapshot's cut and
// before the streams save of the same flush. The streams file still holds the
// records of the writes the table file holds.
func Test_persist_streamsHoldRecordsRetentionDropped(t *testing.T) {
	const retention = time.Second

	dir := t.TempDir()
	s, eng, tables := pairStack(t, dir)

	s.ok(t, opCreate, hashTable("tbl"))
	s.ok(t, opUpdate, `{"TableName":"tbl",`+keysOnly+`}`)

	shard := dynamodb.Shard(s.provider, usEast1, "tbl")
	shard.SetLimits(retention, stream.MaxBytes)

	for _, id := range []string{"a", "b", "c"} {
		s.ok(t, opPut, `{"TableName":"tbl","Item":{"id":{"S":"`+id+`"}}}`)
	}

	tables.after = func() {
		require.Len(t, shard.Records(), 3, "the cut is taken before retention passes")
		require.Eventually(t, func() bool { return len(shard.Records()) == 0 }, 10*retention, retention/10,
			"retention drops the records")
	}

	require.NoError(t, eng.Flush(t.Context()))
	require.Equal(t, []int{3, 3}, restoredCounts(t, dir))
}

// Test_persist_cutHoldsRecordsOnlyForAStreamsSave (SQ-367): the table
// snapshot's cut copies the stream records only when a streams save follows
// it in the flush, and the copy is not held once the streams snapshot has
// written it, or once the table save it was for failed.
func Test_persist_cutHoldsRecordsOnlyForAStreamsSave(t *testing.T) {
	t.Run("streams persisted", func(t *testing.T) {
		dir := t.TempDir()
		s, eng, tables := pairStack(t, dir)
		pairSetup(t, s)

		held := -1
		tables.after = func() { held = dynamodb.CutRecords(s.provider) }

		require.NoError(t, eng.Flush(t.Context()))
		require.Equal(t, 3, held, "the cut holds the records for the streams save")
		require.Zero(t, dynamodb.CutRecords(s.provider), "the streams save took the copy")
		require.Equal(t, []int{3, 3}, restoredCounts(t, dir))
	})

	t.Run("streams not persisted", func(t *testing.T) {
		dir := t.TempDir()
		s, eng, tables := tableStack(t, dir, false)
		pairSetup(t, s)

		held := -1
		tables.after = func() { held = dynamodb.CutRecords(s.provider) }

		require.NoError(t, eng.Flush(t.Context()))
		require.Zero(t, held, "no streams save follows, so the cut copies no record")
		require.Zero(t, dynamodb.CutRecords(s.provider))
		require.Equal(t, []int{3, 0}, restoredCounts(t, dir), "the table is saved, the records are not")
	})

	t.Run("table save failed", func(t *testing.T) {
		dir := t.TempDir()
		s, eng, tables := pairStack(t, dir)
		pairSetup(t, s)

		tables.fail = true
		require.ErrorIs(t, eng.Flush(t.Context()), errTableDisk)
		require.Zero(t, dynamodb.CutRecords(s.provider), "no streams save follows a failed table save")
	})
}
