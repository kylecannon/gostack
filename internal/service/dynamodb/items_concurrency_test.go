package dynamodb_test

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

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// These tests drive the provider through the gateway while many goroutines
// read and write the same keys, so they hold whichever way an item is stored.
// Every item written is one version: a number unique to the test (writer
// times ccWriterSpan plus a sequence) from which every other attribute is
// derived, and a checksum over the rest. A read that mixes two versions, or
// a version only a cancelled transaction wrote (it carries "poison"), fails.

// The attribute and request member names these tests write most.
const (
	ccVerName         = "ver"
	ccSumName         = "sum"
	ccKeyMember       = "Key"
	ccNamesMember     = "ExpressionAttributeNames"
	ccAttrNameMember  = "AttributeName"
	ccAttrTypeMember  = "AttributeType"
	ccConditionMember = "ConditionExpression"
	ccAllOld          = "ALL_OLD"
	ccTransactItems   = "TransactItems"
	ccPutAction       = "Put"
	ccConsistentRead  = "ConsistentRead"
	ccKeyCondition    = "KeyConditionExpression"
	ccReturnValues    = "ReturnValues"
	ccUpdateMember    = "UpdateExpression"
	ccValuesMember    = "ExpressionAttributeValues"
)

// ccWriterSpan separates the versions of two writers.
const ccWriterSpan = 100_000

// ccMaxFill is one more than the most aN attributes a version holds, so
// versions differ in attribute count as well as in their values' lengths.
const ccMaxFill = 5

// ccGSI is the global secondary index on g the index tests create.
const ccGSI = "byg"

// ccAnchor is a key written once and never again: a transaction whose
// condition on it requires it absent always cancels.
const ccAnchor = "anchor"

// ccS, ccN and ccBool are DynamoDB JSON values as encoding/json decodes them.
func ccS(s string) map[string]any { return map[string]any{"S": s} }
func ccN(n int) map[string]any    { return map[string]any{"N": strconv.Itoa(n)} }
func ccBool() map[string]any      { return map[string]any{"BOOL": true} }

// ccMix scatters ver's bits, so the attributes derived from them do not
// follow the sequence a writer counts in.
func ccMix(ver int) int { return int(uint32(ver) * 2654435761 >> 8) }

// ccGKey is the GSI key of version ver.
func ccGKey(ver int) string { return "g" + strconv.Itoa(ccMix(ver)>>3%2) }

// ccOtherGKey is the GSI key version ver does not have.
func ccOtherGKey(ver int) string { return "g" + strconv.Itoa(1-ccMix(ver)>>3%2) }

// ccVersion is the item key holds at version ver.
func ccVersion(key string, ver int) map[string]any {
	mix := ccMix(ver)
	item := map[string]any{
		"pk":      ccS(key),
		ccVerName: ccN(ver),
		"g":       ccS(ccGKey(ver)),
		"m": map[string]any{"M": map[string]any{
			"v": ccN(ver),
			"l": map[string]any{"L": []any{ccS(strconv.Itoa(ver)), ccN(mix % 97)}},
		}},
	}

	count := 1 + mix>>8%ccMaxFill
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

	item[ccSumName] = ccS(ccSum(item))

	return item
}

// ccPoisoned is version ver of key as only a cancelled transaction writes it.
func ccPoisoned(key string, ver int) map[string]any {
	item := ccVersion(key, ver)
	item["poison"] = ccBool()
	item[ccSumName] = ccS(ccSum(item))

	return item
}

// ccSum is the checksum of every attribute of item but sum.
func ccSum(item map[string]any) string {
	h := fnv.New64a()

	for _, name := range slices.Sorted(maps.Keys(item)) {
		if name == ccSumName {
			continue
		}

		h.Write([]byte(name))
		h.Write(ccJSON(item[name]))
	}

	return strconv.FormatUint(h.Sum64(), 16)
}

// ccJSON is v as JSON; json.Marshal sorts a map's members.
func ccJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return b
}

// ccVer is the version item carries.
func ccVer(item map[string]any) (int, error) {
	v, ok := item[ccVerName].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("no ver: %s", ccJSON(item))
	}

	n, ok := v["N"].(string)
	if !ok {
		return 0, fmt.Errorf("ver is not a number: %s", ccJSON(item))
	}

	ver, err := strconv.Atoi(n)
	if err != nil {
		return 0, fmt.Errorf("ver: %w", err)
	}

	return ver, nil
}

// ccKey is the pk of item.
func ccKey(item map[string]any) string {
	pk, _ := item["pk"].(map[string]any)
	s, _ := pk["S"].(string)

	return s
}

// ccExpected is the version item names at key, and that version.
func ccExpected(item map[string]any, key string) (map[string]any, int, error) {
	if _, ok := item["poison"]; ok {
		return nil, 0, fmt.Errorf("a cancelled transaction's version is visible: %s", ccJSON(item))
	}

	ver, err := ccVer(item)
	if err != nil {
		return nil, 0, err
	}

	return ccVersion(key, ver), ver, nil
}

// ccCheck reports how item, read at key, is not one whole version: every
// attribute of the version its ver names, and nothing else, with its checksum.
func ccCheck(item map[string]any, key string) (int, error) {
	want, ver, err := ccExpected(item, key)
	if err != nil {
		return 0, err
	}

	sum, _ := item[ccSumName].(map[string]any)
	if sum["S"] != ccSum(item) {
		return 0, fmt.Errorf("checksum does not match the attributes: %s", ccJSON(item))
	}

	if !reflect.DeepEqual(item, want) {
		return 0, fmt.Errorf("not one whole version:\n got %s\nwant %s", ccJSON(item), ccJSON(want))
	}

	return ver, nil
}

// ccCheckProjected is ccCheck for a read projected to names: exactly the
// version's attributes among names.
func ccCheckProjected(item map[string]any, key string, names []string) error {
	want, _, err := ccExpected(item, key)
	if err != nil {
		return err
	}

	maps.DeleteFunc(want, func(name string, _ any) bool { return !slices.Contains(names, name) })

	if !reflect.DeepEqual(item, want) {
		return fmt.Errorf("projection not of one whole version:\n got %s\nwant %s", ccJSON(item), ccJSON(want))
	}

	return nil
}

// ccKeyOf is the Key of key.
func ccKeyOf(key string) map[string]any {
	return map[string]any{"pk": ccS(key)}
}

// ccCheckPartial is ccCheck for UPDATED_OLD: some attributes, ver among them,
// each as the version has it.
func ccCheckPartial(item map[string]any, key string) (int, error) {
	want, ver, err := ccExpected(item, key)
	if err != nil {
		return 0, err
	}

	for name, v := range item {
		if !reflect.DeepEqual(v, want[name]) {
			return 0, fmt.Errorf("attribute %s not of version %d: %s", name, ver, ccJSON(item))
		}
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

func newCCTest(t *testing.T) *ccTest {
	t.Helper()

	return &ccTest{t: t, s: newStack(t)}
}

func (c *ccTest) errorf(format string, args ...any) {
	if c.failed.Add(1) <= 10 {
		c.t.Errorf(format, args...)
	}
}

// do sends operation with body and returns the decoded answer and its status;
// an answer that is not JSON is a failure.
func (c *ccTest) do(operation string, body any) (map[string]any, int) {
	text, ok := body.(string)
	if !ok {
		text = string(ccJSON(body))
	}

	got := c.s.call(c.t, usEast1, operation, text)

	var out map[string]any
	err := json.Unmarshal([]byte(got.body), &out)
	if err != nil {
		c.errorf("%s: %d %s", operation, got.status, got.body)

		return nil, 0
	}

	return out, got.status
}

// ok sends a call that must succeed and returns its answer, nil when it did
// not.
func (c *ccTest) ok(operation string, body any) map[string]any {
	out, status := c.do(operation, body)
	if status != http.StatusOK {
		c.errorf("%s: %d %s", operation, status, ccJSON(out))

		return nil
	}

	return out
}

// refused sends a call that must fail with code and returns its answer.
func (c *ccTest) refused(operation string, body any, code string) map[string]any {
	out, status := c.do(operation, body)
	if status != http.StatusBadRequest || out["__type"] != code {
		c.errorf("%s: want %s, got %d %s", operation, code, status, ccJSON(out))

		return nil
	}

	return out
}

// check reports a read item that is not one whole version.
func (c *ccTest) check(where string, item any, key string) int {
	m, ok := item.(map[string]any)
	if !ok {
		c.errorf("%s: not an item: %v", where, item)

		return 0
	}

	ver, err := ccCheck(m, key)
	if err != nil {
		c.errorf("%s %s: %v", where, key, err)
	}

	return ver
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

// ccTableBody is a CreateTable body for name keyed by pk.
func ccTableBody(name string) map[string]any {
	return map[string]any{
		fieldTable:             name,
		"BillingMode":          "PAY_PER_REQUEST",
		"KeySchema":            []any{map[string]any{ccAttrNameMember: "pk", "KeyType": "HASH"}},
		"AttributeDefinitions": []any{map[string]any{ccAttrNameMember: "pk", ccAttrTypeMember: "S"}},
	}
}

// table creates name keyed by pk.
func (c *ccTest) table(name string) {
	require.NotNil(c.t, c.ok(opCreate, ccTableBody(name)))
}

// indexedTable creates name keyed by pk, with the GSI on g.
func (c *ccTest) indexedTable(name string) {
	body := ccTableBody(name)
	body["AttributeDefinitions"] = []any{
		map[string]any{ccAttrNameMember: "pk", ccAttrTypeMember: "S"},
		map[string]any{ccAttrNameMember: "g", ccAttrTypeMember: "S"},
	}
	body["GlobalSecondaryIndexes"] = []any{map[string]any{
		"IndexName":  ccGSI,
		"KeySchema":  []any{map[string]any{ccAttrNameMember: "g", "KeyType": "HASH"}},
		"Projection": map[string]any{"ProjectionType": "ALL"},
	}}

	require.NotNil(c.t, c.ok(opCreate, body))
}

// ccWith is body with the members of extra added.
func ccWith(body map[string]any, extra map[string]any) map[string]any {
	maps.Copy(body, extra)

	return body
}

// ccPut is a PutItem body writing version ver of key.
func ccPut(table, key string, ver int, extra map[string]any) map[string]any {
	return ccWith(map[string]any{fieldTable: table, keyItem: ccVersion(key, ver)}, extra)
}

// ccUpdate is an UpdateItem body, or a transaction's Update, that makes key
// version ver whatever it held: it sets every attribute of the version and
// removes the ones it lacks, in one expression.
func ccUpdate(table, key string, ver int, extra map[string]any) map[string]any {
	item := ccVersion(key, ver)
	names := map[string]any{}
	values := map[string]any{}

	var set, remove []string

	for _, name := range slices.Sorted(maps.Keys(item)) {
		if name == "pk" {
			continue
		}

		names["#"+name] = name
		values[":"+name] = item[name]
		set = append(set, "#"+name+" = :"+name)
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

	return ccWith(map[string]any{
		fieldTable: table, ccKeyMember: ccKeyOf(key), ccUpdateMember: expr,
		ccNamesMember: names, ccValuesMember: values,
	}, extra)
}

// ccDelete is a DeleteItem body for key.
func ccDelete(table, key string, extra map[string]any) map[string]any {
	return ccWith(map[string]any{fieldTable: table, ccKeyMember: ccKeyOf(key)}, extra)
}

// ccNestedUpdate is ccUpdate with m written member by member, SET m.v and
// m.l, in place of whole, so the update writes below an attribute of the item
// it unpacked. Every version's m holds exactly v and l, so the item written is
// still one whole version. With no item stored m is missing, and the
// condition refuses the update rather than the path.
func ccNestedUpdate(table, key string, ver int) map[string]any {
	body := ccUpdate(table, key, ver, nil)

	m, _ := ccVersion(key, ver)["m"].(map[string]any)
	members, _ := m["M"].(map[string]any)
	names, _ := body[ccNamesMember].(map[string]any)
	values, _ := body[ccValuesMember].(map[string]any)
	expr, _ := body[ccUpdateMember].(string)

	delete(values, ":m")
	names["#v"], names["#l"] = "v", "l"
	values[":mv"], values[":ml"] = members["v"], members["l"]
	body[ccUpdateMember] = strings.Replace(expr, "#m = :m", "#m.#v = :mv, #m.#l = :ml", 1)
	body[ccConditionMember] = "attribute_exists(#m)"

	return body
}

// nestedWrites makes ops nested updates over keys of table, as writer w, and
// counts the ones that applied: a key may hold no item when one runs.
func (c *ccTest) nestedWrites(table string, keys []string, w, ops int, applied *atomic.Int32) {
	for i := range ops {
		ver := w*ccWriterSpan + 2*i + 1

		out, status := c.do(opUpdateItem, ccNestedUpdate(table, keys[i%len(keys)], ver))

		switch {
		case status == http.StatusOK:
			applied.Add(1)
		case status == http.StatusBadRequest && out["__type"] == codeConditional:
		default:
			c.errorf("nested update: %d %s", status, ccJSON(out))
		}
	}
}

// readStream checks the images of the shard's records from pos on, each a
// whole version of the record's key, and returns the position to read next.
func (c *ccTest) readStream(shard dynamodb.StreamShard, pos int, images *atomic.Int32) int {
	page, err := shard.Read(pos, 1000, 1<<20)
	if err != nil {
		c.errorf("stream read at %d: %v", pos, err)

		return pos
	}

	for _, rec := range page.Records {
		key := ""
		if pk := rec.Keys["pk"].S; pk != nil {
			key = *pk
		}

		for _, img := range []value.Packed{rec.Old, rec.New} {
			if img == "" {
				continue
			}

			var item map[string]any
			if json.Unmarshal(ccJSON(value.Unpack(img)), &item) != nil {
				c.errorf("stream image at %d: not json", rec.Seq)

				continue
			}

			c.check("stream image", item, key)
			images.Add(1)
		}
	}

	return page.Next
}

// ccFailingPut is a PutItem of version ver whose condition no stored version
// meets, asking for the stored item when it fails.
func ccFailingPut(table, key string, ver int) map[string]any {
	return ccPut(table, key, ver, map[string]any{
		ccConditionMember:                     "#ver = :never",
		ccNamesMember:                         map[string]any{"#ver": ccVerName},
		ccValuesMember:                        map[string]any{":never": ccN(-1)},
		"ReturnValuesOnConditionCheckFailure": ccAllOld,
	})
}

// ccCancelled is a TransactWriteItems that writes the poisoned version ver of
// key and cancels: its check requires the anchor absent.
func ccCancelled(table, key string, ver int) map[string]any {
	return map[string]any{ccTransactItems: []any{
		map[string]any{ccPutAction: map[string]any{fieldTable: table, keyItem: ccPoisoned(key, ver)}},
		map[string]any{"ConditionCheck": map[string]any{
			fieldTable: table, ccKeyMember: map[string]any{"pk": ccS(ccAnchor)},
			ccConditionMember:                     "attribute_not_exists(#pk)",
			ccNamesMember:                         map[string]any{"#pk": "pk"},
			"ReturnValuesOnConditionCheckFailure": ccAllOld,
		}},
	}}
}

// ccTransactPair is a TransactWriteItems that puts version ver of one key and
// updates another to version ver+1.
func ccTransactPair(table, putKey, updateKey string, ver int) map[string]any {
	update := ccUpdate(table, updateKey, ver+1, nil)

	return map[string]any{ccTransactItems: []any{
		map[string]any{ccPutAction: map[string]any{fieldTable: table, keyItem: ccVersion(putKey, ver)}},
		map[string]any{"Update": update},
	}}
}

// ccKeys is n keys named k0, k1, ….
func ccKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}

	return keys
}

// cancelled checks a cancelled transaction's answer: the anchor's stored
// item, as its check returned it, is whole.
func (c *ccTest) cancelled(out map[string]any) {
	reasons, _ := out["CancellationReasons"].([]any)
	if len(reasons) != 2 {
		c.errorf("cancellation reasons: %s", ccJSON(out))

		return
	}

	reason, _ := reasons[1].(map[string]any)
	if reason["Code"] != "ConditionalCheckFailed" {
		c.errorf("cancellation reason: %s", ccJSON(out))

		return
	}

	c.check("cancelled transaction's anchor", reason[keyItem], ccAnchor)
}

// ccSeen counts the distinct versions readers saw of each key.
type ccSeen struct {
	mu   sync.Mutex
	vers map[string]map[int]bool
}

func (s *ccSeen) add(key string, ver int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.vers == nil {
		s.vers = map[string]map[int]bool{}
	}

	if s.vers[key] == nil {
		s.vers[key] = map[int]bool{}
	}

	s.vers[key][ver] = true
}

func (s *ccSeen) most() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	most := 0
	for _, vers := range s.vers {
		most = max(most, len(vers))
	}

	return most
}

// Test_packedConcurrency_checker: the whole-version check refuses a torn
// item, one with an attribute missing or added, a checksum that does not
// match, and a poisoned version, so the concurrency tests below can fail.
func Test_packedConcurrency_checker(t *testing.T) {
	whole := ccVersion("k0", 100_003)
	_, err := ccCheck(whole, "k0")
	require.NoError(t, err)

	other := ccVersion("k0", 200_004)

	for name, mutate := range map[string]func(map[string]any){
		"torn":     func(m map[string]any) { m["a0"] = other["a0"] },
		"dropped":  func(m map[string]any) { delete(m, "a1") },
		"extra":    func(m map[string]any) { m["a4"] = other["a4"] },
		"checksum": func(m map[string]any) { m[ccSumName] = ccS("0") },
		"nested":   func(m map[string]any) { m["m"] = other["m"] },
		"poison":   func(m map[string]any) { m["poison"] = ccBool() },
		"other key": func(m map[string]any) {
			m["pk"] = ccS("k1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			item := ccVersion("k0", 100_003)
			mutate(item)

			_, got := ccCheck(item, "k0")
			require.Error(t, got)
		})
	}

	err = ccCheckProjected(map[string]any{"pk": ccS("k0"), ccVerName: ccN(100_003), "a0": other["a0"]},
		"k0", []string{"pk", ccVerName, "a0"})
	require.Error(t, err)

	_, err = ccCheckPartial(map[string]any{ccVerName: ccN(100_003), "a0": other["a0"]}, "k0")
	require.Error(t, err)
}

// ccProjection is the projection the projected reads ask for.
var ccProjection = []string{"pk", ccVerName, "a0", "a3", "m"}

// ccProjectionBody is the ProjectionExpression and names of ccProjection.
func ccProjectionBody() map[string]any {
	names := map[string]any{}
	parts := make([]string, 0, len(ccProjection))

	for _, name := range ccProjection {
		names["#"+name] = name
		parts = append(parts, "#"+name)
	}

	return map[string]any{"ProjectionExpression": strings.Join(parts, ", "), ccNamesMember: names}
}

// readAll runs every kind of read once over keys and checks each item read.
func (c *ccTest) readAll(table string, keys []string, seen *ccSeen, filtered *atomic.Int32) {
	note := func(where string, item any) {
		m, _ := item.(map[string]any)
		key := ccKey(m)
		seen.add(key, c.check(where, item, key))
	}

	for _, key := range keys {
		get := c.ok(opGet, map[string]any{
			fieldTable: table, ccKeyMember: ccKeyOf(key), ccConsistentRead: true,
		})
		if item, ok := get[keyItem]; ok {
			note("GetItem", item)
		}

		query := c.ok(opQuery, map[string]any{
			fieldTable: table, ccKeyCondition: "#pk = :k", ccConsistentRead: true,
			ccNamesMember: map[string]any{"#pk": "pk"}, ccValuesMember: map[string]any{":k": ccS(key)},
		})
		for _, item := range ccItems(query) {
			note("Query", item)
		}

		projected := c.ok(opQuery, ccWith(map[string]any{
			fieldTable: table, ccKeyCondition: "#pk = :k",
			ccValuesMember: map[string]any{":k": ccS(key)},
		}, ccProjectionBody()))
		for _, item := range ccItems(projected) {
			c.projected("projected Query", item, key)
		}
	}

	for _, item := range ccItems(c.ok(opScan, map[string]any{fieldTable: table})) {
		note("Scan", item)
	}

	filter := c.ok(opScan, map[string]any{
		fieldTable: table, "FilterExpression": "attribute_exists(#sum) AND #n >= :one",
		ccNamesMember: map[string]any{"#sum": ccSumName, "#n": "n"}, ccValuesMember: map[string]any{":one": ccN(1)},
	})
	for _, item := range ccItems(filter) {
		filtered.Add(1)
		note("filtered Scan", item)
	}

	for _, item := range ccItems(c.ok(opScan, ccWith(map[string]any{fieldTable: table}, ccProjectionBody()))) {
		m, _ := item.(map[string]any)
		c.projected("projected Scan", item, ccKey(m))
	}

	keyList := make([]any, 0, len(keys))
	gets := make([]any, 0, len(keys))

	for _, key := range keys {
		keyList = append(keyList, map[string]any{"pk": ccS(key)})
		gets = append(gets, map[string]any{"Get": map[string]any{fieldTable: table, ccKeyMember: ccKeyOf(key)}})
	}

	batch := c.ok(opBatchGet, map[string]any{"RequestItems": map[string]any{table: map[string]any{"Keys": keyList}}})
	if unprocessed, _ := batch["UnprocessedKeys"].(map[string]any); len(unprocessed) > 0 {
		c.errorf("BatchGetItem left keys unprocessed: %s", ccJSON(batch))
	}

	responses, _ := batch["Responses"].(map[string]any)
	for _, item := range ccList(responses[table]) {
		note("BatchGetItem", item)
	}

	transact := c.ok(opTransactGet, map[string]any{ccTransactItems: gets})
	for _, response := range ccList(transact["Responses"]) {
		if item, ok := response.(map[string]any)[keyItem]; ok {
			note("TransactGetItems", item)
		}
	}
}

// projected checks an item of a projected read.
func (c *ccTest) projected(where string, item any, key string) {
	m, _ := item.(map[string]any)
	err := ccCheckProjected(m, key, ccProjection)
	if err != nil {
		c.errorf("%s %s: %v", where, key, err)
	}
}

// ccItems is a Query or Scan answer's items.
func ccItems(out map[string]any) []any {
	return ccList(out["Items"])
}

func ccList(v any) []any {
	list, _ := v.([]any)

	return list
}

// writeMix makes writer w's ops writes over keys of table, cycling through
// every write path: put, update, delete, a transaction over two keys, a
// cancelled transaction, and a put whose condition fails.
func (c *ccTest) writeMix(table string, keys []string, w, ops int, cancels, conditionItems *atomic.Int32) {
	for i := range ops {
		ver := w*ccWriterSpan + 2*i + 1
		key := keys[(w+i)%len(keys)]

		switch i % 6 {
		case 0:
			c.ok(opPut, ccPut(table, key, ver, nil))
		case 1:
			c.ok(opUpdateItem, ccUpdate(table, key, ver, nil))
		case 2:
			c.ok(opDeleteItem, ccDelete(table, key, nil))
		case 3:
			c.ok(opTransactWrite, ccTransactPair(table, key, keys[(w+i+1)%len(keys)], ver))
		case 4:
			if out := c.refused(opTransactWrite, ccCancelled(table, key, ver), "TransactionCanceledException"); out != nil {
				cancels.Add(1)
				c.cancelled(out)
			}
		default:
			if out := c.refused(opPut, ccFailingPut(table, key, ver), codeConditional); out != nil {
				if item, ok := out[keyItem]; ok {
					conditionItems.Add(1)
					c.check("failed condition's item", item, key)
				}
			}
		}
	}
}

// Test_packedConcurrency_wholeVersionReads: GetItem, Query, Scan with and
// without a filter or projection, BatchGetItem and TransactGetItems read keys
// that PutItem, UpdateItem, DeleteItem, TransactWriteItems (some cancelled)
// and time-to-live sweeps write at the same time, with updates that write
// below an attribute, SET m.v. Every item read is one whole version, no
// cancelled transaction's version is ever seen, and a failed condition's and a
// cancelled check's returned item are whole too, as is every old and new image
// of the stream, read while the writes run.
func Test_packedConcurrency_wholeVersionReads(t *testing.T) {
	const (
		writers = 4
		readers = 4
		ops     = 120
		table   = "whole"
	)

	c := newCCTest(t)
	require.NotNil(t, c.ok(opCreate, ccWith(ccTableBody(table), map[string]any{
		"StreamSpecification": map[string]any{"StreamEnabled": true, "StreamViewType": "NEW_AND_OLD_IMAGES"},
	})))
	require.NotNil(t, c.ok("UpdateTimeToLive", map[string]any{
		fieldTable: table, "TimeToLiveSpecification": map[string]any{"Enabled": true, ccAttrNameMember: "exp"},
	}))
	require.NotNil(t, c.ok(opPut, ccPut(table, ccAnchor, 1, nil)))

	info, ok := c.s.provider.StreamByARN(streamCtx(t), c.s.latestStreamARN(t, table))
	require.True(t, ok)

	keys := ccKeys(4)
	readKeys := append(ccKeys(4), ccAnchor)

	var (
		writing                                       sync.WaitGroup
		reading                                       sync.WaitGroup
		seen                                          ccSeen
		cancels, conditionItems, filtered, expiredAll atomic.Int32
		nested, images                                atomic.Int32
		streamPos                                     int
	)

	done := make(chan struct{})

	for w := 1; w <= writers; w++ {
		writing.Go(func() { c.writeMix(table, keys, w, ops, &cancels, &conditionItems) })
	}

	writing.Go(func() { c.nestedWrites(table, keys, writers+1, ops, &nested) })

	reading.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				streamPos = c.readStream(info.Shard, streamPos, &images)
			}
		}
	})

	reading.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				expiredAll.Add(int32(c.expire()))
			}
		}
	})

	for range readers {
		reading.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					c.readAll(table, readKeys, &seen, &filtered)
				}
			}
		})
	}

	writing.Wait()
	close(done)
	reading.Wait()

	// One more pass once the writes have stopped, so every read path ran at
	// least once whatever the scheduling.
	c.readAll(table, readKeys, &seen, &filtered)
	c.readStream(info.Shard, streamPos, &images)

	require.Positive(t, cancels.Load(), "a transaction was cancelled")
	require.Positive(t, conditionItems.Load(), "a failed condition returned the stored item")
	require.Positive(t, filtered.Load(), "the filtered scan returned items")
	require.Greater(t, seen.most(), 1, "readers saw more than one version of a key")
	require.Positive(t, expiredAll.Load(), "a sweep beside the writers removed an expired version")
	require.Positive(t, nested.Load(), "a nested update applied")
	require.Positive(t, images.Load(), "the stream reader checked images")
}

// Test_packedConcurrency_noLostUpdate: workers × rounds ADDs of one to one
// counter, through UpdateItem and through TransactWriteItems, end at exactly
// workers × rounds, and UpdateItem's UPDATED_NEW answers are 1 to that total,
// each once.
func Test_packedConcurrency_noLostUpdate(t *testing.T) {
	const (
		workers = 8
		rounds  = 50
		table   = "counter"
	)

	add := func(key string) map[string]any {
		return map[string]any{
			fieldTable: table, ccKeyMember: ccKeyOf(key), ccUpdateMember: "ADD #c :one",
			ccNamesMember: map[string]any{"#c": "counter"}, ccValuesMember: map[string]any{":one": ccN(1)},
		}
	}

	c := newCCTest(t)
	c.table(table)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		saw   = map[int]int{}
		total = workers * rounds
	)

	for range workers {
		wg.Go(func() {
			for range rounds {
				out := c.ok(opUpdateItem, ccWith(add("update"), map[string]any{ccReturnValues: "UPDATED_NEW"}))
				attributes, _ := out["Attributes"].(map[string]any)
				counter, _ := attributes["counter"].(map[string]any)
				n, err := strconv.Atoi(fmt.Sprint(counter["N"]))
				if err != nil {
					c.errorf("UPDATED_NEW: %s", ccJSON(out))

					continue
				}

				mu.Lock()
				saw[n]++
				mu.Unlock()
			}
		})
		wg.Go(func() {
			for range rounds {
				c.transactAdd(map[string]any{ccTransactItems: []any{map[string]any{"Update": add("transact")}}})
			}
		})
	}

	wg.Wait()

	want := map[int]int{}
	for i := 1; i <= total; i++ {
		want[i] = 1
	}

	require.Equal(t, want, saw, "every UPDATED_NEW from 1 to the total, once")

	for _, key := range []string{"update", "transact"} {
		got := c.ok(opGet, map[string]any{fieldTable: table, ccKeyMember: ccKeyOf(key), ccConsistentRead: true})
		require.Equal(t, strconv.Itoa(total), path(t, got, "Item", "counter", "N"), key)
	}
}

// transactAdd runs a TransactWriteItems that must apply, retrying it while a
// concurrent transaction on the same item cancels it, as AWS may.
func (c *ccTest) transactAdd(body map[string]any) {
	for {
		out, status := c.do(opTransactWrite, body)
		if status == http.StatusOK {
			return
		}

		if status == http.StatusBadRequest && strings.Contains(string(ccJSON(out)), "TransactionConflict") {
			continue
		}

		c.errorf("TransactWriteItems: %d %s", status, ccJSON(out))

		return
	}
}

// gsiQuery is the items of the GSI under g.
func (c *ccTest) gsiQuery(table, g string) []any {
	return ccItems(c.ok(opQuery, map[string]any{
		fieldTable: table, "IndexName": ccGSI, ccKeyCondition: "#g = :g",
		ccNamesMember: map[string]any{"#g": "g"}, ccValuesMember: map[string]any{":g": ccS(g)},
	}))
}

// gsiCheck checks every item of the GSI query under g: whole, and keyed g.
// It returns the version each key holds under g.
func (c *ccTest) gsiCheck(table, g string) map[string]int {
	got := map[string]int{}

	for _, item := range c.gsiQuery(table, g) {
		m, _ := item.(map[string]any)
		key := ccKey(m)
		ver := c.check("GSI Query", item, key)

		if gk, _ := m["g"].(map[string]any); gk["S"] != g {
			c.errorf("GSI Query under %s returned %s", g, ccJSON(m))
		}

		got[key] = ver
	}

	return got
}

// Test_packedConcurrency_indexReplacement: writers overwrite items so their
// GSI key flips between g0 and g1, through PutItem, UpdateItem,
// TransactWriteItems and DeleteItem. A GSI Query never returns an item whose
// GSI key is not the one queried, or a torn one. Each owner writer is the
// only writer of its key: once its write returns, the GSI holds exactly that
// version under the new key and nothing of the key under the other. Once the
// writes stop, the GSI holds exactly the base table's items.
func Test_packedConcurrency_indexReplacement(t *testing.T) {
	const (
		owners  = 3
		sharers = 2
		ops     = 60
		table   = "indexed"
	)

	c := newCCTest(t)
	c.indexedTable(table)

	shared := ccKeys(3)

	var writing, reading sync.WaitGroup

	done := make(chan struct{})

	for w := 1; w <= owners; w++ {
		writing.Go(func() { c.ownIndexed(table, w, ops) })
	}

	for w := owners + 1; w <= owners+sharers; w++ {
		writing.Go(func() {
			for i := range ops {
				ver := w*ccWriterSpan + i + 1
				key := shared[(w+i)%len(shared)]

				switch i % 4 {
				case 0, 3:
					c.ok(opPut, ccPut(table, key, ver, nil))
				case 1:
					c.ok(opUpdateItem, ccUpdate(table, key, ver, nil))
				default:
					c.ok(opTransactWrite, ccTransactPair(table, key, shared[(w+i+1)%len(shared)], ver))
				}
			}
		})
	}

	for range 2 {
		reading.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					c.gsiCheck(table, "g0")
					c.gsiCheck(table, "g1")
				}
			}
		})
	}

	writing.Wait()
	close(done)
	reading.Wait()

	base := map[string]map[string]int{"g0": {}, "g1": {}}

	for _, item := range ccItems(c.ok(opScan, map[string]any{fieldTable: table, ccConsistentRead: true})) {
		m, _ := item.(map[string]any)
		key := ccKey(m)
		ver := c.check("Scan", item, key)
		base[ccGKey(ver)][key] = ver
	}

	for g, want := range base {
		require.Equal(t, want, c.gsiCheck(table, g), "the GSI under %s holds the table's items", g)
	}
}

// ownIndexed is owner w's writes of its own key, each followed by GSI queries
// under both keys.
func (c *ccTest) ownIndexed(table string, w, ops int) {
	key := "own" + strconv.Itoa(w)

	for i := range ops {
		ver := w*ccWriterSpan + i + 1

		switch i % 4 {
		case 0:
			c.ok(opPut, ccPut(table, key, ver, nil))
		case 1:
			c.ok(opUpdateItem, ccUpdate(table, key, ver, nil))
		case 2:
			c.ok(opTransactWrite, map[string]any{ccTransactItems: []any{
				map[string]any{ccPutAction: map[string]any{fieldTable: table, keyItem: ccVersion(key, ver)}},
			}})
		default:
			c.ok(opDeleteItem, ccDelete(table, key, nil))
		}

		now, other := ccGKey(ver), ccOtherGKey(ver)
		if got, held := c.gsiCheck(table, now)[key]; i%4 == 3 && held {
			c.errorf("GSI under %s holds deleted %s at %d", now, key, got)
		} else if i%4 != 3 && got != ver {
			c.errorf("GSI under %s holds %s at %d after the write of %d returned", now, key, got, ver)
		}

		if got, held := c.gsiCheck(table, other)[key]; held {
			c.errorf("GSI under %s still holds %s at %d after the write of %d returned", other, key, got, ver)
		}
	}
}

// Test_packedConcurrency_snapshot: snapshots taken and committed while every
// write path runs restore into a fresh provider whose items are each one
// whole version, with the GSI holding exactly the restored table's items.
func Test_packedConcurrency_snapshot(t *testing.T) {
	const (
		writers   = 4
		ops       = 90
		snapshots = 6
		table     = "snapped"
	)

	c := newCCTest(t)
	c.indexedTable(table)
	require.NotNil(t, c.ok(opPut, ccPut(table, ccAnchor, 1, nil)))

	keys := ccKeys(4)

	var (
		writing                 sync.WaitGroup
		cancels, conditionItems atomic.Int32
	)

	for w := 1; w <= writers; w++ {
		writing.Go(func() { c.writeMix(table, keys, w, ops, &cancels, &conditionItems) })
	}

	restoredItems := 0

	for range snapshots {
		c.s.provider.Followed(true)

		var saved bytes.Buffer
		require.NoError(t, c.s.provider.Snapshot(&saved))
		c.s.provider.Committed()
		c.s.provider.Followed(false)

		r := &ccTest{t: t, s: newStack(t)}
		require.NoError(t, r.s.provider.Restore(&saved))

		base := map[string]map[string]int{"g0": {}, "g1": {}}

		for _, item := range ccItems(r.ok(opScan, map[string]any{fieldTable: table})) {
			m, _ := item.(map[string]any)
			key := ccKey(m)
			ver := r.check("restored Scan", item, key)
			base[ccGKey(ver)][key] = ver
			restoredItems++
		}

		for g, want := range base {
			require.Equal(t, want, r.gsiCheck(table, g), "the restored GSI under %s holds the table's items", g)
		}
	}

	writing.Wait()
	require.Positive(t, restoredItems)
}

// ccTransition is one write's effect on its key as its answer reports it:
// the version it replaced and the version it left, 0 for no item.
type ccTransition struct {
	key      string
	from, to int
}

// Test_packedConcurrency_oldItemReaders: PutItem and DeleteItem with
// ReturnValues ALL_OLD and UpdateItem with ALL_OLD and UPDATED_OLD write the
// same keys concurrently, beside puts whose condition fails and transactions
// that cancel. Every old item returned is one whole version, and together the
// answers chain: every version a write left is replaced by exactly one later
// write, but the one the key holds at the end. A failed condition returns a
// version some write left, and no cancelled version is ever returned.
func Test_packedConcurrency_oldItemReaders(t *testing.T) {
	const (
		writers = 4
		ops     = 120
		table   = "olds"
	)

	c := newCCTest(t)
	c.table(table)
	require.NotNil(t, c.ok(opPut, ccPut(table, ccAnchor, 1, nil)))

	keys := ccKeys(3)

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		transitions []ccTransition
		failedSaw   []ccTransition
	)

	for w := 1; w <= writers; w++ {
		wg.Go(func() {
			for i := range ops {
				ver := w*ccWriterSpan + i + 1
				key := keys[(w+i)%len(keys)]

				tr, isWrite := c.oldItemWrite(table, key, ver, i)
				if tr == nil {
					continue
				}

				mu.Lock()
				if isWrite {
					transitions = append(transitions, *tr)
				} else {
					failedSaw = append(failedSaw, *tr)
				}
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	for _, key := range keys {
		left := map[int]int{0: 1}
		replaced := map[int]int{}
		wrote := map[int]bool{}

		for _, tr := range transitions {
			if tr.key == key {
				left[tr.to]++
				replaced[tr.from]++
				wrote[tr.to] = true
			}
		}

		final := 0
		get := c.ok(opGet, map[string]any{
			fieldTable: table, ccKeyMember: ccKeyOf(key), ccConsistentRead: true,
		})

		if item, ok := get[keyItem]; ok {
			final = c.check("final GetItem", item, key)
		}

		// What the writes left, less what later writes replaced, is the
		// version the key holds now: each version replaced exactly once.
		for ver, n := range replaced {
			left[ver] -= n
		}

		left[final]--
		maps.DeleteFunc(left, func(_ int, n int) bool { return n == 0 })
		require.Empty(t, left, "%s: versions left, less replaced, less the final one", key)

		for _, tr := range failedSaw {
			if tr.key == key {
				require.True(t, wrote[tr.from], "%s: a failed condition returned %d, a version no write left",
					key, tr.from)
			}
		}
	}

	require.NotEmpty(t, failedSaw, "a failed condition returned the stored item")
}

// oldItemWrite is write i of version ver to key; it returns the transition
// the answer reports, and whether the write applied (a failed condition's
// returned item is a transition to nothing).
func (c *ccTest) oldItemWrite(table, key string, ver, i int) (*ccTransition, bool) {
	allOld := map[string]any{ccReturnValues: ccAllOld}
	old := func(out map[string]any, check func(map[string]any, string) (int, error)) int {
		item, _ := out["Attributes"].(map[string]any)
		if len(item) == 0 {
			return 0
		}

		got, err := check(item, key)
		if err != nil {
			c.errorf("returned old item of %s: %v", key, err)
		}

		return got
	}

	switch i % 6 {
	case 0:
		if out := c.ok(opPut, ccPut(table, key, ver, allOld)); out != nil {
			return &ccTransition{key: key, from: old(out, ccCheck), to: ver}, true
		}
	case 1:
		if out := c.ok(opUpdateItem, ccUpdate(table, key, ver, allOld)); out != nil {
			return &ccTransition{key: key, from: old(out, ccCheck), to: ver}, true
		}
	case 2:
		if out := c.ok(opUpdateItem, ccUpdate(table, key, ver, map[string]any{ccReturnValues: "UPDATED_OLD"})); out != nil {
			return &ccTransition{key: key, from: old(out, ccCheckPartial), to: ver}, true
		}
	case 3:
		if out := c.ok(opDeleteItem, ccDelete(table, key, allOld)); out != nil {
			return &ccTransition{key: key, from: old(out, ccCheck)}, true
		}
	case 4:
		if out := c.refused(opTransactWrite, ccCancelled(table, key, ver), "TransactionCanceledException"); out != nil {
			c.cancelled(out)
		}
	default:
		if out := c.refused(opPut, ccFailingPut(table, key, ver), codeConditional); out != nil {
			if item, ok := out[keyItem]; ok {
				return &ccTransition{key: key, from: c.check("failed condition's item", item, key)}, false
			}
		}
	}

	return nil, false
}
