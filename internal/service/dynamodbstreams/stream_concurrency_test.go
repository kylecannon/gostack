package dynamodbstreams_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests write DynamoDB items from many goroutines and read the stream
// they leave through GetRecords. Every item written is one version: a number
// unique to the test from which every other attribute is derived, and a
// checksum over the rest. A version only a cancelled transaction writes
// carries "poison" and must never appear. The item model is the one
// internal/service/dynamodb's concurrency tests use.

// The request members and event these tests name most.
const (
	ccTableMember    = "TableName"
	ccItemMember     = "Item"
	ccKeyMember      = "Key"
	ccAttrNameMember = "AttributeName"
	ccNamesMember    = "ExpressionAttributeNames"
	ccInsert         = "INSERT"
)

// ccKeyOf is the Key of key.
func ccKeyOf(key string) map[string]any {
	return map[string]any{"pk": ccS(key)}
}

// ccWriterSpan separates the versions of two writers.
const ccWriterSpan = 100_000

// ccAnchor is a key written once and never again: a transaction whose
// condition on it requires it absent always cancels.
const ccAnchor = "anchor"

func ccS(s string) map[string]any { return map[string]any{"S": s} }
func ccN(n int) map[string]any    { return map[string]any{"N": strconv.Itoa(n)} }

// ccMix scatters ver's bits, so the attributes derived from them do not
// follow the sequence a writer counts in.
func ccMix(ver int) int { return int(uint32(ver) * 2654435761 >> 8) }

// ccVersion is the item key holds at version ver.
func ccVersion(key string, ver int) map[string]any {
	mix := ccMix(ver)
	item := map[string]any{
		"pk":  ccS(key),
		"ver": ccN(ver),
		"m":   map[string]any{"M": map[string]any{"v": ccN(ver), "l": map[string]any{"L": []any{ccS(strconv.Itoa(ver))}}}},
	}

	count := 1 + mix>>8%5
	item["n"] = ccN(count)

	for i := range count {
		item["a"+strconv.Itoa(i)] = ccS(strconv.Itoa(ver) + "." + strconv.Itoa(i) + "." +
			strings.Repeat(string(rune('a'+(mix+i)%26)), (mix>>4+i*13)%97))
	}

	// One version in four has already expired, for the tables whose time to
	// live reads exp; never the anchor, which must stay.
	if key != ccAnchor && mix>>5%4 == 0 {
		item["exp"] = ccN(1)
	}

	item["sum"] = ccS(ccSum(item))

	return item
}

// ccSum is the checksum of every attribute of item but sum.
func ccSum(item map[string]any) string {
	h := fnv.New64a()

	for _, name := range slices.Sorted(maps.Keys(item)) {
		if name != "sum" {
			h.Write([]byte(name))
			h.Write(ccJSON(item[name]))
		}
	}

	return strconv.FormatUint(h.Sum64(), 16)
}

func ccJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return b
}

// ccKey is the pk of item.
func ccKey(item map[string]any) string {
	pk, _ := item["pk"].(map[string]any)
	s, _ := pk["S"].(string)

	return s
}

// ccCheck reports how item, read at key, is not one whole version: every
// attribute of the version its ver names, and nothing else, with its checksum.
func ccCheck(item map[string]any, key string) (int, error) {
	if _, ok := item["poison"]; ok {
		return 0, fmt.Errorf("a cancelled transaction's version is visible: %s", ccJSON(item))
	}

	v, _ := item["ver"].(map[string]any)

	ver, err := strconv.Atoi(fmt.Sprint(v["N"]))
	if err != nil {
		return 0, fmt.Errorf("no ver: %s", ccJSON(item))
	}

	if sum, _ := item["sum"].(map[string]any); sum["S"] != ccSum(item) {
		return 0, fmt.Errorf("checksum does not match the attributes: %s", ccJSON(item))
	}

	if want := ccVersion(key, ver); !reflect.DeepEqual(item, want) {
		return 0, fmt.Errorf("not one whole version:\n got %s\nwant %s", ccJSON(item), ccJSON(want))
	}

	return ver, nil
}

// ccTest is one concurrency test: the stack, and its failures, reported from
// any goroutine and capped so a broken run stays readable.
type ccTest struct {
	t      *testing.T
	s      *stack
	failed atomic.Int32
}

func (c *ccTest) errorf(format string, args ...any) {
	if c.failed.Add(1) <= 10 {
		c.t.Errorf(format, args...)
	}
}

// do sends a call to target and returns the decoded answer and its status.
func (c *ccTest) do(target, operation string, body any) (map[string]any, int) {
	got := c.s.call(c.t, target, operation, string(ccJSON(body)))

	var out map[string]any
	err := json.Unmarshal([]byte(got.body), &out)
	if err != nil {
		c.errorf("%s: %d %s", operation, got.status, got.body)

		return nil, 0
	}

	return out, got.status
}

// ok sends a DynamoDB call that must succeed and returns its answer, nil when
// it did not.
func (c *ccTest) ok(operation string, body any) map[string]any {
	out, status := c.do(ddbTarget, operation, body)
	if status != http.StatusOK {
		c.errorf("%s: %d %s", operation, status, ccJSON(out))

		return nil
	}

	return out
}

// refused sends a DynamoDB call that must fail with code.
func (c *ccTest) refused(operation string, body any, code string) {
	out, status := c.do(ddbTarget, operation, body)
	if status != http.StatusBadRequest || out["__type"] != code {
		c.errorf("%s: want %s, got %d %s", operation, code, status, ccJSON(out))
	}
}

// check reports an image or item that is not one whole version; nil is no
// item, version 0.
func (c *ccTest) check(where string, item map[string]any, key string) int {
	if item == nil {
		return 0
	}

	ver, err := ccCheck(item, key)
	if err != nil {
		c.errorf("%s %s: %v", where, key, err)
	}

	return ver
}

// streamTable creates name keyed by pk with a NEW_AND_OLD_IMAGES stream, and
// returns the stream's ARN and its shard's id.
func (c *ccTest) streamTable(name string) (arn, shard string) {
	out := c.ok("CreateTable", map[string]any{
		ccTableMember: name, "BillingMode": "PAY_PER_REQUEST",
		"KeySchema":            []any{map[string]any{ccAttrNameMember: "pk", "KeyType": "HASH"}},
		"AttributeDefinitions": []any{map[string]any{ccAttrNameMember: "pk", "AttributeType": "S"}},
		"StreamSpecification":  map[string]any{"StreamEnabled": true, "StreamViewType": "NEW_AND_OLD_IMAGES"},
	})
	require.NotNil(c.t, out)
	require.NotNil(c.t, c.ok("PutItem", map[string]any{ccTableMember: name, ccItemMember: ccVersion(ccAnchor, 1)}))

	return c.stream(name)
}

// stream is the latest stream of table and its one shard.
func (c *ccTest) stream(table string) (arn, shard string) {
	arn = at[string](c.t, c.s.ddbOK(c.t, "DescribeTable", `{"TableName":"`+table+`"}`), "Table", "LatestStreamArn")
	shards := at[[]any](c.t, c.s.ok(c.t, "DescribeStream", map[string]any{keyStreamArn: arn}),
		"StreamDescription", "Shards")
	require.Len(c.t, shards, 1)

	return arn, at[string](c.t, shards[0], keyShardID)
}

// ccPage is one GetRecords answer.
type ccPage struct {
	Records           []streamsRecord `json:"Records"`
	NextShardIterator *string         `json:"NextShardIterator"`
}

// ccRecord is one stream record as the checks read it.
type ccRecord struct {
	key                string
	event              string
	oldImage, newImage map[string]any
	expired            bool
}

// readAll pages GetRecords from TRIM_HORIZON until a page is empty. It is
// called from any goroutine, so it reports rather than stops.
func (c *ccTest) readAll(arn, shard string) []ccRecord {
	out, status := c.do(streamsTarget, "GetShardIterator",
		map[string]any{keyStreamArn: arn, keyShardID: shard, keyIteratorType: "TRIM_HORIZON"})
	it, _ := out[keyShardIterator].(string)

	if status != http.StatusOK || it == "" {
		c.errorf("GetShardIterator: %d %s", status, ccJSON(out))

		return nil
	}

	var got []ccRecord

	for {
		raw := c.s.call(c.t, streamsTarget, opGetRecords, string(ccJSON(map[string]any{keyShardIterator: it})))

		var p ccPage
		if raw.status != http.StatusOK || json.Unmarshal([]byte(raw.body), &p) != nil || p.NextShardIterator == nil {
			c.errorf("GetRecords: %d %s", raw.status, raw.body)

			return got
		}

		if len(p.Records) == 0 {
			return got
		}

		for _, r := range p.Records {
			got = append(got, ccRecord{
				key: ccKey(r.Dynamodb.Keys), event: r.EventName,
				oldImage: r.Dynamodb.OldImage, newImage: r.Dynamodb.NewImage, expired: r.UserIdentity != nil,
			})
		}

		it = *p.NextShardIterator
	}
}

// chain checks the records of each key, in stream order: each is whole, an
// INSERT follows no item, a MODIFY or REMOVE replaces the item the record
// before it left, and the last one leaves final[key]. It returns each
// record's transition.
func (c *ccTest) chain(records []ccRecord, final map[string]int) []ccTransition {
	prev := map[string]int{}
	transitions := make([]ccTransition, 0, len(records))

	for _, r := range records {
		from := c.check(r.event+" old image", r.oldImage, r.key)
		to := c.check(r.event+" new image", r.newImage, r.key)

		ok := from == prev[r.key]
		switch r.event {
		case ccInsert:
			ok = ok && r.oldImage == nil && r.newImage != nil
		case "MODIFY":
			ok = ok && r.oldImage != nil && r.newImage != nil
		case "REMOVE":
			ok = ok && r.oldImage != nil && r.newImage == nil
		default:
			ok = false
		}

		if !ok {
			c.errorf("%s record of %s from %d to %d follows version %d", r.event, r.key, from, to, prev[r.key])
		}

		prev[r.key] = to

		if !r.expired {
			transitions = append(transitions, ccTransition{key: r.key, from: from, to: to})
		}
	}

	maps.DeleteFunc(prev, func(_ string, ver int) bool { return ver == 0 })
	maps.DeleteFunc(final, func(_ string, ver int) bool { return ver == 0 })

	if !maps.Equal(prev, final) {
		c.errorf("the records leave %v, the table holds %v", prev, final)
	}

	return transitions
}

// ccTransition is one write's effect on its key: the version it replaced, -1
// when its answer does not say, and the version it left, 0 for no item.
type ccTransition struct {
	key      string
	from, to int
}

// ccWith is body with the members of extra added.
func ccWith(body, extra map[string]any) map[string]any {
	maps.Copy(body, extra)

	return body
}

// ccUpdate is an UpdateItem body that makes key version ver whatever it held.
func ccUpdate(table, key string, ver int) map[string]any {
	item := ccVersion(key, ver)
	names := map[string]any{}
	values := map[string]any{}

	var set, remove []string

	for _, name := range slices.Sorted(maps.Keys(item)) {
		if name != "pk" {
			names["#"+name] = name
			values[":"+name] = item[name]
			set = append(set, "#"+name+" = :"+name)
		}
	}

	for _, name := range []string{"a1", "a2", "a3", "a4", "exp"} {
		if _, ok := item[name]; !ok {
			names["#"+name] = name
			remove = append(remove, "#"+name)
		}
	}

	expr := "SET " + strings.Join(set, ", ")
	if len(remove) > 0 {
		expr += " REMOVE " + strings.Join(remove, ", ")
	}

	return map[string]any{
		ccTableMember: table, ccKeyMember: ccKeyOf(key), "UpdateExpression": expr,
		ccNamesMember: names, "ExpressionAttributeValues": values, "ReturnValues": "ALL_OLD",
	}
}

// write is write i of version ver to key, through every write path in turn,
// and the transition its answer reports; nil for a write that must not
// apply.
func (c *ccTest) write(table, key string, ver, i int) *ccTransition {
	allOld := map[string]any{"ReturnValues": "ALL_OLD"}
	old := func(out map[string]any) int {
		item, _ := out["Attributes"].(map[string]any)
		if len(item) == 0 {
			return 0
		}

		return c.check("returned old item", item, key)
	}

	switch i % 6 {
	case 0:
		put := map[string]any{ccTableMember: table, ccItemMember: ccVersion(key, ver)}
		if out := c.ok("PutItem", ccWith(put, allOld)); out != nil {
			return &ccTransition{key: key, from: old(out), to: ver}
		}
	case 1:
		if out := c.ok("UpdateItem", ccUpdate(table, key, ver)); out != nil {
			return &ccTransition{key: key, from: old(out), to: ver}
		}
	case 2:
		del := map[string]any{ccTableMember: table, ccKeyMember: ccKeyOf(key)}
		if out := c.ok("DeleteItem", ccWith(del, allOld)); out != nil {
			return &ccTransition{key: key, from: old(out)}
		}
	case 3:
		if c.ok("TransactWriteItems", map[string]any{"TransactItems": []any{
			map[string]any{"Put": map[string]any{ccTableMember: table, ccItemMember: ccVersion(key, ver)}},
		}}) != nil {
			return &ccTransition{key: key, from: -1, to: ver}
		}
	case 4:
		poisoned := ccVersion(key, ver)
		poisoned["poison"] = map[string]any{"BOOL": true}
		c.refused("TransactWriteItems", map[string]any{"TransactItems": []any{
			map[string]any{"Put": map[string]any{ccTableMember: table, ccItemMember: poisoned}},
			map[string]any{"ConditionCheck": map[string]any{
				ccTableMember: table, ccKeyMember: map[string]any{"pk": ccS(ccAnchor)},
				"ConditionExpression": "attribute_not_exists(#pk)", ccNamesMember: map[string]any{"#pk": "pk"},
			}},
		}}, "TransactionCanceledException")
	default:
		c.refused("PutItem", ccWith(map[string]any{ccTableMember: table, ccItemMember: ccVersion(key, ver)}, map[string]any{
			"ConditionExpression": "#ver = :never", ccNamesMember: map[string]any{"#ver": "ver"},
			"ExpressionAttributeValues": map[string]any{":never": ccN(-1)},
		}), "ConditionalCheckFailedException")
	}

	return nil
}

// writers runs writers goroutines of ops writes each over keys and returns
// the transitions their answers report.
func (c *ccTest) writers(table string, keys []string, writers, ops int) []ccTransition {
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []ccTransition
	)

	for w := 1; w <= writers; w++ {
		wg.Go(func() {
			for i := range ops {
				if tr := c.write(table, keys[(w+i)%len(keys)], w*ccWriterSpan+i+1, i); tr != nil {
					mu.Lock()
					all = append(all, *tr)
					mu.Unlock()
				}
			}
		})
	}

	wg.Wait()

	return all
}

// final is the version each key of table holds, by a Scan.
func (c *ccTest) final(table string) map[string]int {
	final := map[string]int{}

	items, _ := c.ok("Scan", map[string]any{ccTableMember: table})["Items"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		final[ccKey(m)] = c.check("Scan", m, ccKey(m))
	}

	return final
}

// expire runs one time-to-live sweep and returns how many items it removed.
func (c *ccTest) expire() int {
	req := httptest.NewRequestWithContext(c.t.Context(), http.MethodDelete, "/_aws/dynamodb/expired", http.NoBody)
	rec := httptest.NewRecorder()
	c.s.handler.ServeHTTP(rec, req)

	var out struct {
		ExpiredItems int `json:"ExpiredItems"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		c.errorf("expire: %d %s", rec.Code, rec.Body.String())
	}

	return out.ExpiredItems
}

// ccKeys is n keys named k0, k1, ….
func ccKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}

	return keys
}

// Test_streamConcurrency_chain: writers put, update, delete and transact on
// the same keys, beside failed conditions, cancelled transactions,
// time-to-live sweeps and a GetRecords reader. Each key's records are in
// write order: each old image is the previous record's new image, every image
// is one whole version, and the last record leaves what the table holds. The
// records and the writes' ALL_OLD answers agree one to one: every write that
// changed its key left exactly the record its answer describes, and no
// cancelled or refused write left one.
func Test_streamConcurrency_chain(t *testing.T) {
	const (
		writers = 4
		ops     = 120
		table   = "chained"
	)

	c := &ccTest{t: t, s: newStack(t)}
	arn, shard := c.streamTable(table)
	require.NotNil(t, c.ok("UpdateTimeToLive", map[string]any{
		ccTableMember: table, "TimeToLiveSpecification": map[string]any{"Enabled": true, ccAttrNameMember: "exp"},
	}))

	var (
		background sync.WaitGroup
		read       atomic.Int64
		expired    atomic.Int64
	)

	done := make(chan struct{})

	background.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				expired.Add(int64(c.expire()))
			}
		}
	})
	background.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				for _, r := range c.readAll(arn, shard) {
					c.check(r.event+" old image read beside the writes", r.oldImage, r.key)
					c.check(r.event+" new image read beside the writes", r.newImage, r.key)
					read.Add(1)
				}
			}
		}
	})

	// The anchor's put, before the writers, is the first write.
	writes := append([]ccTransition{{key: ccAnchor, to: 1}}, c.writers(table, ccKeys(4), writers, ops)...)

	close(done)
	background.Wait()

	records := c.readAll(arn, shard)
	streamed := c.chain(records, c.final(table))

	require.Empty(t, c.unmatched(writes, streamed), "records no write's answer accounts for")
	require.Positive(t, expired.Load(), "a sweep beside the writers removed an item")
	require.Positive(t, read.Load(), "the reader beside the writers read records")
}

// unmatched matches each record not a sweep's to one write, by the version it
// left (versions are unique) and the version it replaced, and returns the
// records no write accounts for. A delete of no item leaves no record; a
// write that left none is a failure.
func (c *ccTest) unmatched(writes, streamed []ccTransition) map[ccTransition]int {
	left := map[ccTransition]int{}
	for _, tr := range streamed {
		left[tr]++
	}

	for _, tr := range writes {
		if tr.from == 0 && tr.to == 0 {
			continue
		}

		if tr.from == -1 {
			for got := range left {
				if got.key == tr.key && got.to == tr.to && got.to != 0 {
					tr.from = got.from
				}
			}
		}

		if left[tr] == 0 {
			c.errorf("write of %s from %d to %d left no record", tr.key, tr.from, tr.to)

			continue
		}

		left[tr]--
	}

	maps.DeleteFunc(left, func(_ ccTransition, n int) bool { return n == 0 })

	return left
}

// Test_streamConcurrency_snapshot: tables and streams snapshots taken as
// persistence takes them (table snapshot, committed, then the streams
// snapshot of its cut) while writers run restore into a fresh pair of
// providers whose records chain per key and end at exactly the restored
// table's items, every image one whole version.
func Test_streamConcurrency_snapshot(t *testing.T) {
	const (
		writers   = 4
		ops       = 90
		snapshots = 5
		table     = "saved"
	)

	c := &ccTest{t: t, s: newStack(t)}
	c.streamTable(table)

	var wg sync.WaitGroup

	wg.Go(func() { c.writers(table, ccKeys(4), writers, ops) })

	restored := 0

	for range snapshots {
		var tables, streams bytes.Buffer

		c.s.ddb.Followed(true)
		require.NoError(t, c.s.ddb.Snapshot(&tables))
		c.s.ddb.Committed()
		require.NoError(t, c.s.ddb.SnapshotStreams(&streams))
		c.s.ddb.Followed(false)

		r := &ccTest{t: t, s: newStack(t)}
		require.NoError(t, r.s.ddb.Restore(&tables))
		require.NoError(t, r.s.ddb.RestoreStreams(&streams))

		arn, shard := r.stream(table)
		records := r.readAll(arn, shard)
		r.chain(records, r.final(table))
		restored += len(records)
	}

	wg.Wait()
	require.Positive(t, restored, "a restored stream held records")
}
