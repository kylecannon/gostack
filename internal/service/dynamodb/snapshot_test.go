package dynamodb_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// The service names a provider reports changes for.
const (
	ddbName     = "dynamodb"
	streamsName = "dynamodbstreams"
)

// listenAll is the gateway's listen address in the stacks these tests build.
const listenAll = "0.0.0.0:4566"

// changes records the service names a provider reports changed.
type changes struct {
	mu    sync.Mutex
	names []string
}

func (c *changes) changed(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.names = append(c.names, name)
}

func (c *changes) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.names...)
}

// changedStack is newStack with changed told of the changes no call reports.
func changedStack(t *testing.T, changed func(string)) *stack {
	t.Helper()

	s := &stack{}
	s.settings.Store(&dynamodb.Settings{Host: testHost})
	s.provider = dynamodb.New(func() dynamodb.Settings { return *s.settings.Load() }, changed)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{s.provider.Registration()}, nil, nil,
	)
	require.NoError(t, err)

	s.handler = gw.Handler()

	return s
}

// Test_snapshot_marksChanged checks the two changes the gateway does not see
// are reported: a stream record, to dynamodbstreams, which saves records, and
// a time to live removal through the developer endpoint, to dynamodb.
func Test_snapshot_marksChanged(t *testing.T) {
	var got changes

	s := changedStack(t, got.changed)
	s.ok(t, opCreate, `{"TableName":"marked","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST",`+
		`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"}}`)
	require.Empty(t, got.seen())

	s.ok(t, opPut, `{"TableName":"marked","Item":{"id":{"S":"a"},"exp":{"N":"1"}}}`)
	require.Equal(t, []string{streamsName}, got.seen())

	s.ok(t, opPut, `{"TableName":"marked","Item":{"id":{"S":"a"},"exp":{"N":"1"}}}`)
	require.Equal(t, []string{streamsName}, got.seen(), "a put that changes nothing records nothing")

	s.ok(t, "UpdateTimeToLive", `{"TableName":"marked","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"exp"}}`)

	expire := func() string {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/_gostack/services/dynamodb/expired", nil)
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return rec.Body.String()
	}

	require.JSONEq(t, `{"ExpiredItems":1}`, expire())
	require.Equal(t, []string{streamsName, streamsName, ddbName}, got.seen(),
		"the removal's REMOVE record, then the removal")

	require.JSONEq(t, `{"ExpiredItems":0}`, expire())
	require.Len(t, got.seen(), 3, "a sweep that removes nothing changes nothing")
}

// snapshotOf is the DynamoDB snapshot of s.
func snapshotOf(t testing.TB, p *dynamodb.Provider) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, p.Snapshot(&buf))

	return buf.Bytes()
}

// savedOf is a table snapshot as persistence saves it: taken, then reported
// on disk, so the streams snapshot writes the records of its cut.
func savedOf(t testing.TB, p *dynamodb.Provider) []byte {
	t.Helper()

	b := snapshotOf(t, p)
	p.Committed()

	return b
}

const minimalMeta = `{"Name":"tab","Attributes":[{"AttributeName":"id","AttributeType":"S"}],` +
	`"Key":[{"AttributeName":"id","KeyType":"HASH"}]}`

// fileWith is a snapshot of one table in us-east-1 of the test account.
func fileWith(table string) string {
	return `{"v":1,"accounts":[],"states":[{"account":"000000000000","region":"us-east-1","tables":[` +
		table + `]}]}`
}

// Test_snapshot_restoreRefuses checks Restore refuses a file no state of
// the service could have written, and leaves the service empty.
func Test_snapshot_restoreRefuses(t *testing.T) {
	for name, file := range map[string]string{
		"not json":          `{"v":1,`,
		"no version":        `{"states":[]}`,
		"later version":     `{"v":2,"states":[]}`,
		"items first":       fileWith(`{"items":[],"meta":` + minimalMeta + `}`),
		"no meta":           fileWith(`{"items":[]}`),
		"no key schema":     fileWith(`{"meta":{"Name":"tab"},"items":[]}`),
		"undefined key":     fileWith(`{"meta":{"Name":"tab","Key":[{"AttributeName":"id","KeyType":"HASH"}]},"items":[]}`),
		"range first":       fileWith(`{"meta":` + strings.Replace(minimalMeta, "HASH", "RANGE", 1) + `,"items":[]}`),
		"duplicate key":     fileWith(`{"meta":` + minimalMeta + `,"items":[{"id":{"S":"a"}},{"id":{"S":"a"}}]}`),
		"key missing":       fileWith(`{"meta":` + minimalMeta + `,"items":[{"other":{"S":"a"}}]}`),
		"memberless value":  fileWith(`{"meta":` + minimalMeta + `,"items":[{"id":{"S":"a"},"x":{}}]}`),
		"two members":       fileWith(`{"meta":` + minimalMeta + `,"items":[{"id":{"S":"a"},"x":{"S":"a","N":"1"}}]}`),
		"unparsable number": fileWith(`{"meta":` + minimalMeta + `,"items":[{"id":{"S":"a"},"x":{"N":"one"}}]}`),
		"table twice":       fileWith(`{"meta":` + minimalMeta + `,"items":[]},{"meta":` + minimalMeta + `,"items":[]}`),
		"stream no shard": fileWith(`{"meta":` + strings.TrimSuffix(minimalMeta, "}") +
			`,"Stream":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"},"StreamLabel":"x"},"items":[]}`),
		"bad view type": fileWith(`{"meta":` + minimalMeta +
			`,"shard":{"created":"2026-01-01T00:00:00Z","low":1,"next":1,"viewType":"ALL","region":"r"},"items":[]}`),
		"replica no region": `{"v":1,"accounts":[{"account":"a","replicas":[{"table":"t","owner":"us-east-1",` +
			`"replicas":[{}]}]}],"states":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.ok(t, opCreate, hashTable("existing"))

			require.Error(t, s.provider.Restore(strings.NewReader(file)))
			require.Equal(t, map[string]any{"TableNames": []any{}}, s.ok(t, "ListTables", `{}`))
		})
	}
}

// Test_snapshot_restoreReplaces checks a good file replaces every table, and
// a restored table takes writes, with numbers kept in canonical form.
func Test_snapshot_restoreReplaces(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("existing"))

	require.NoError(t, s.provider.Restore(strings.NewReader(
		fileWith(`{"meta":`+minimalMeta+`,"items":[{"id":{"S":"a"},"n":{"N":"1.50"}}]}`))))
	require.Equal(t, map[string]any{"TableNames": []any{"tab"}}, s.ok(t, "ListTables", `{}`))
	require.JSONEq(t, `{"Item":{"id":{"S":"a"},"n":{"N":"1.5"}}}`,
		s.call(t, usEast1, opGet, `{"TableName":"tab","Key":{"id":{"S":"a"}}}`).body)

	s.ok(t, opPut, `{"TableName":"tab","Item":{"id":{"S":"b"}}}`)
	require.Contains(t, s.call(t, usEast1, "DescribeTable", `{"TableName":"tab"}`).body, `"ItemCount":2,`)
}

// Test_snapshot_deepestItem checks an attribute nested as deep as a put takes
// (value.MaxDepth levels) is saved and restored as it was.
func Test_snapshot_deepestItem(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("deep"))

	levels := value.MaxDepth - 1
	deep := strings.Repeat(`{"M":{"a":`, levels) + `{"S":"x"}` + strings.Repeat(`}}`, levels)
	s.ok(t, opPut, `{"TableName":"deep","Item":{"id":{"S":"a"},"d":`+deep+`}}`)

	restored := newStack(t)
	require.NoError(t, restored.provider.Restore(bytes.NewReader(snapshotOf(t, s.provider))))
	require.JSONEq(t, `{"Item":{"id":{"S":"a"},"d":`+deep+`}}`,
		restored.call(t, usEast1, opGet, `{"TableName":"deep","Key":{"id":{"S":"a"}}}`).body)
}

// escapedNames are attribute names whose json forms differ only by escapes
// and quotes: a\"b and a"b, "x" and x. A decoder that shares names between
// items must tell each pair apart.
var escapedNames = []string{`a\"b`, `a"b`, `"x"`, `x`}

// Test_snapshot_escapedNames saves items whose attribute names are
// escapedNames, one name to an item and all in one, through the persistence
// engine in each format, and checks every name reads back as it was written.
func Test_snapshot_escapedNames(t *testing.T) {
	for _, format := range []persist.Format{persist.FormatJSON, persist.FormatBinary} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			before, eng := formatStack(t, dir, format)
			before.ok(t, opCreate, hashTable("names"))

			all := value.Item{}
			want := map[string]value.Item{}

			for i, name := range escapedNames {
				v := strconv.Itoa(i)
				item := value.Item{"id": {S: &v}, name: {N: &v}}
				all[name] = api.AttributeValue{S: &v}
				want[v] = item
			}

			all["id"] = api.AttributeValue{S: new("all")}
			want["all"] = all

			for _, item := range want {
				before.ok(t, opPut, `{"TableName":"names","Item":`+mustJSON(t, item)+`}`)
			}

			require.NoError(t, eng.Flush(t.Context()))

			after, _ := formatStack(t, dir, format)

			for id, item := range want {
				got := after.call(t, usEast1, opGet, `{"TableName":"names","Key":{"id":{"S":"`+id+`"}}}`).body
				require.JSONEq(t, `{"Item":`+mustJSON(t, item)+`}`, got, id)
			}
		})
	}
}

// formatStack is newStack with DynamoDB persisted under dir in format.
func formatStack(t *testing.T, dir string, format persist.Format) (*stack, *persist.Engine) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Formats: []persist.Format{format},
		MountDetector: func(string) bool { return false },
	})

	s := &stack{}
	s.settings.Store(&dynamodb.Settings{Host: testHost})
	s.provider = dynamodb.New(func() dynamodb.Settings { return *s.settings.Load() }, eng.MarkDirty)
	reg := s.provider.Registration()
	eng.Register(ddbName, reg.Persist)
	require.NoError(t, eng.Load(t.Context()))

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{reg}, nil, eng,
	)
	require.NoError(t, err)

	s.handler = gw.Handler()

	return s, eng
}

// Test_snapshot_deterministic checks one state gives one file, so a restored
// state snapshots to the bytes it was restored from.
func Test_snapshot_deterministic(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, `{"TableName":"det","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST",`+
		`"Tags":[{"Key":"a","Value":"b"}]}`)

	for i := range 50 {
		s.ok(t, opPut, `{"TableName":"det","Item":{"id":{"S":"k`+strconv.Itoa(i)+
			`"},"m":{"M":{"z":{"N":"1"},"a":{"SS":["y","x"]}}}}}`)
	}

	saved := snapshotOf(t, s.provider)
	require.Equal(t, saved, snapshotOf(t, s.provider))

	restored := newStack(t)
	require.NoError(t, restored.provider.Restore(bytes.NewReader(saved)))
	require.Equal(t, string(saved), string(snapshotOf(t, restored.provider)))
}

// Test_snapshot_writesDuringSnapshot runs snapshots beside writers: every
// snapshot restores, and holds each table's items as one moment left them.
func Test_snapshot_writesDuringSnapshot(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("busy"))

	stop := make(chan struct{})

	var wg sync.WaitGroup

	for worker := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}

				s.call(t, usEast1, opPut, `{"TableName":"busy","Item":{"id":{"S":"w`+strconv.Itoa(worker)+`-`+
					strconv.Itoa(i%100)+`"}}}`)
			}
		})
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		restored := newStack(t)
		require.NoError(t, restored.provider.Restore(bytes.NewReader(snapshotOf(t, s.provider))))
	}

	close(stop)
	wg.Wait()
}
