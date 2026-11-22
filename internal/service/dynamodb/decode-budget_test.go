package dynamodb_test

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// numbers is how many {"N":"1"} one item holds at DynamoDB's 400 KB
	// limit: each costs 3 bytes of item size and 10 bytes of body, so the
	// densest legal item is a 1.3 MB body that decodes into 130,000
	// 160-byte values.
	numbers = 130_000
	// nests is how many lists nested to the depth limit one item holds: 30
	// lists and a leaf under the attribute's own list, 125 bytes of item size
	// and 249 bytes of body each.
	nests = 3_000

	// A transaction's payload is 4 MB and a batch's body 16 MiB.
	batchItems     = 25
	batchNumbers   = 58_000
	transactItems  = 100
	transactNumber = 12_000
)

// numberList is n {"N":"1"} elements.
func numberList(n int) string {
	return strings.TrimSuffix(strings.Repeat(`{"N":"1"},`, n), ",")
}

// nestedLists is count lists each nested 31 levels deep with a leaf.
func nestedLists(count int) string {
	nest := strings.Repeat(`{"L":[`, 30) + `{"S":""}` + strings.Repeat(`]}`, 30)

	return strings.TrimSuffix(strings.Repeat(nest+",", count), ",")
}

// denseItem is an item of key id holding list l of elements.
func denseItem(id, elements string) string {
	return `{"id":{"S":"` + id + `"},"l":{"L":[` + elements + `]}}`
}

// roomyMemoryLimit gives the process a Go memory limit far above what any body
// here keeps, as cmd/gostack gives it one from a container's, so that a body
// charged past protocol.DecodeFactor is decoded (protocol.ReserveHeap) and not
// refused for want of room. The old limit is put back after tb.
func roomyMemoryLimit(tb testing.TB) {
	tb.Helper()

	old := debug.SetMemoryLimit(8 << 30)
	tb.Cleanup(func() { debug.SetMemoryLimit(old) })
}

// noMemoryLimit runs tb with no Go memory limit and no memory cap, as gostack
// runs where it reads neither a container's limit nor a machine's memory, so
// that the decode budget weighs a body against the room of
// protocol.UnknownMemory. The old limit and cap are put back after tb.
func noMemoryLimit(tb testing.TB) {
	tb.Helper()

	old, oldCap := debug.SetMemoryLimit(math.MaxInt64), protocol.SetMemoryCap(0)

	tb.Cleanup(func() {
		debug.SetMemoryLimit(old)
		protocol.SetMemoryCap(oldCap)
	})
}

// numberAttributes is an item of key id holding n number attributes.
func numberAttributes(id string, n int) string {
	var b strings.Builder

	b.WriteString(`{"id":{"S":"` + id + `"}`)

	for i := range n {
		fmt.Fprintf(&b, `,"a%d":{"N":"1"}`, i)
	}

	return b.String() + "}"
}

// Test_decodeBudget_noMemoryLimitServesWhatAWSAccepts: with no Go memory limit
// and no memory cap, the default `docker run`, items that AWS and LocalStack
// accept and every other service's budget refuses are served, weighed against
// the room of protocol.UnknownMemory; a body whose heap does not fit that room
// is still refused.
func Test_decodeBudget_noMemoryLimitServesWhatAWSAccepts(t *testing.T) {
	noMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)
	s.ok(t, opCreate, hashTable("open"))

	for name, item := range map[string]string{
		"a list of 10,445 numbers":  denseItem("l10445", numberList(10_445)),
		"22,433 number attributes":  numberAttributes("w22433", 22_433),
		"a list of 50,000 numbers":  denseItem("l50000", numberList(50_000)),
		"a list of 130,000 numbers": denseItem("l130000", numberList(numbers)),
	} {
		got := s.call(t, usEast1, opPut, `{"TableName":"open","Item":`+item+`}`)
		assert.Equal(t, http.StatusOK, got.status, "%s: %s", name, got.body[:min(len(got.body), 300)])
	}

	// No legal body keeps more than the 758 MiB of room the unknown memory
	// leaves: the densest batch, 25 lists of 1,800 maps nested 30 deep, is
	// estimated at 567 MiB, with 16 MiB of body beside it, a margin of 175
	// MiB (Test_decodeBudget_noMemoryLimitServesTheDensestBatches). So the
	// in-flight bound takes all but 64 MiB of it: 1,000,000 numbers in a 10 MB
	// body keep more than that at their peak, and are refused.
	protocol.SetInFlight(protocol.SoftLimit(protocol.UnknownMemory) - protocol.HeapSlack - 64<<20)

	got := s.call(t, usEast1, opPut, `{"TableName":"open","Item":`+denseItem("over", numberList(1_000_000))+`}`)
	assert.Equal(t, http.StatusBadRequest, got.status, got.body[:min(len(got.body), 300)])
	assert.Contains(t, got.body, "more than gostack allows")

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

// Test_decodeBudget_aDenseBodyBesideASparseOneIsServed: a dense BatchWriteItem
// of 15.5 MB and a sparse one of 9.75 MB, both legal, sent at once, round
// after round. The sparse body waits for room under the in-flight bound that
// the dense body holds, and only that; the dense body is served in every
// round, not refused for a waiter its own move lets in.
func Test_decodeBudget_aDenseBodyBesideASparseOneIsServed(t *testing.T) {
	const rounds = 15

	roomyMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)
	s.ok(t, opCreate, hashTable("pair"))

	server := httptest.NewServer(s.handler)
	t.Cleanup(server.Close)

	dense := make([]string, 0, batchItems)
	sparse := make([]string, 0, batchItems)

	for i := range batchItems {
		dense = append(dense, `{"PutRequest":{"Item":`+denseItem(fmt.Sprint("d", i), numberList(62_000))+`}}`)
		sparse = append(sparse, `{"PutRequest":{"Item":{"id":{"S":"`+fmt.Sprint("s", i)+`"},"t":{"S":"`+
			strings.Repeat("x", 390_000)+`"}}}}`)
	}

	bodies := []string{
		`{"RequestItems":{"pair":[` + strings.Join(dense, ",") + `]}}`,
		`{"RequestItems":{"pair":[` + strings.Join(sparse, ",") + `]}}`,
	}

	for round := range rounds {
		var (
			wg      sync.WaitGroup
			answers [2]string
			status  [2]int
		)

		for i, body := range bodies {
			wg.Go(func() { status[i], answers[i] = sendTo(t, server, "BatchWriteItem", body) })
		}

		wg.Wait()

		for i, name := range []string{"dense", "sparse"} {
			assert.Equal(t, http.StatusOK, status[i], "round %d, %s: %s", round, name, answers[i][:min(len(answers[i]), 200)])
		}
	}

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

// mapChain is a map nested 30 levels deep, one entry each, around a number:
// under an item's list, the deepest DynamoDB allows.
var mapChain = strings.Repeat(`{"M":{"a":`, 30) + oneNumber + strings.Repeat(`}}`, 30)

// oneNumber is the smallest number value.
const oneNumber = `{"N":"1"}`

// denseBatch is a BatchWriteItem of 25 items to table, each a list of count
// copies of element: the densest batches DynamoDB accepts, at 16 MiB.
func denseBatch(table, element string, count int) string {
	elements := strings.TrimSuffix(strings.Repeat(element+",", count), ",")

	items := make([]string, 0, batchItems)
	for i := range batchItems {
		items = append(items, `{"PutRequest":{"Item":`+denseItem(fmt.Sprint("i", i), elements)+`}}`)
	}

	return `{"RequestItems":{"` + table + `":[` + strings.Join(items, ",") + `]}}`
}

// Test_decodeBudget_noMemoryLimitServesTheDensestBatches: with no Go memory
// limit and no memory cap, each of the densest batches DynamoDB accepts is
// served against the room of protocol.UnknownMemory, each beside an empty
// store: the table the batch before it filled is deleted, its items garbage.
func Test_decodeBudget_noMemoryLimitServesTheDensestBatches(t *testing.T) {
	noMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)

	for _, tt := range []struct {
		name, element string
		count         int
	}{
		{"25 x 64,000 numbers", oneNumber, 64_000},
		{"25 x 74,000 empty maps", `{"M":{}}`, 74_000},
		{"25 x 1,800 maps nested 30 deep", mapChain, 1_800},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := denseBatch("dense", tt.element, tt.count)
			require.Greater(t, len(body), 15<<20, "the batch is near the cap")
			require.Less(t, int64(len(body)), protocol.DefaultMaxBody)

			s.ok(t, opCreate, hashTable("dense"))
			t.Cleanup(func() { s.ok(t, "DeleteTable", `{"TableName":"dense"}`) })

			got := s.call(t, usEast1, opBatchWrite, body)
			assert.Equal(t, http.StatusOK, got.status, got.body[:min(len(got.body), 300)])
		})
	}

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

// Test_decodeBudget_aBodyAfterOneThatDiedIsServed: a dense batch sent to a
// table that does not exist is decoded and answered ResourceNotFoundException,
// and its value is garbage once it is answered; the same batch sent next to
// a table that does exist is served, though the last collection ran while the
// first one's value was live.
func Test_decodeBudget_aBodyAfterOneThatDiedIsServed(t *testing.T) {
	noMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)
	s.ok(t, opCreate, hashTable("chains"))

	missing := denseBatch("missing", mapChain, 1_800)
	got := s.call(t, usEast1, opBatchWrite, missing)
	require.Equal(t, http.StatusBadRequest, got.status)
	require.Contains(t, got.body, "ResourceNotFoundException", "the first body is decoded")

	got = s.call(t, usEast1, opBatchWrite, strings.Replace(missing, `"missing"`, `"chains"`, 1))
	assert.Equal(t, http.StatusOK, got.status, got.body[:min(len(got.body), 300)])
}

// Test_decodeBudget_twoDenseBatchesAtOnceAreServedOrThrottled: two dense
// batches of 7.5 MB sent at once, round after round. One holds the dense lane
// and is served; the other is served too or answered with a throttle, which
// an SDK retries, and never refused as too large: it fits once the first is
// decoded.
func Test_decodeBudget_twoDenseBatchesAtOnceAreServedOrThrottled(t *testing.T) {
	const rounds = 4

	roomyMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)
	s.ok(t, opCreate, hashTable("pair"))

	server := httptest.NewServer(s.handler)
	t.Cleanup(server.Close)

	body := denseBatch("pair", oneNumber, 30_000)
	require.Greater(t, len(body), 7<<20)

	for round := range rounds {
		var (
			wg      sync.WaitGroup
			answers [2]string
			status  [2]int
		)

		start := make(chan struct{})

		for i := range answers {
			wg.Go(func() {
				<-start

				status[i], answers[i] = sendTo(t, server, opBatchWrite, body)
			})
		}

		close(start)
		wg.Wait()

		served := 0

		for i := range answers {
			switch {
			case status[i] == http.StatusOK:
				served++
			case status[i] == http.StatusBadRequest && strings.Contains(answers[i], "ThrottlingException"):
			default:
				t.Errorf("round %d, body %d: %d %s", round, i, status[i], answers[i][:min(len(answers[i]), 200)])
			}
		}

		assert.Positive(t, served, "round %d: one body is served", round)
	}

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

// Test_decodeBudget_worstLegalBody sends, for each operation that takes
// AttributeValues, the densest body its documented limits allow, and every one
// is served: none is refused for decoding into more than its budget. The
// shapes are the worst measured (docs/architecture.md, Request memory): a list
// of the smallest values at 400 KB of item, and lists nested to the depth
// limit. The batch and the transaction are the largest each admits.
func Test_decodeBudget_worstLegalBody(t *testing.T) {
	roomyMemoryLimit(t) // the batch's items stay live beside the transaction: 758 MiB holds each alone, not both

	s := newStack(t)
	s.ok(t, opCreate, hashTable("dense"))
	s.ok(t, opPut, `{"TableName":"dense","Item":`+denseItem("q", numberList(numbers))+`}`)

	batch := make([]string, 0, batchItems)
	for i := range batchItems {
		batch = append(batch, `{"PutRequest":{"Item":`+denseItem(fmt.Sprint("b", i), numberList(batchNumbers))+`}}`)
	}

	transaction := make([]string, 0, transactItems)
	for i := range transactItems {
		transaction = append(transaction,
			`{"Put":{"TableName":"dense","Item":`+denseItem(fmt.Sprint("t", i), numberList(transactNumber))+`}}`)
	}

	dense := `{"L":[` + numberList(numbers) + `]}`

	tests := []struct {
		name, operation, body string
		wantBytes             int
	}{
		{
			"PutItem 400 KB of numbers", opPut,
			`{"TableName":"dense","Item":` + denseItem("p", numberList(numbers)) + `}`, 1 << 20,
		},
		{
			"PutItem 400 KB of lists nested to the depth limit", opPut,
			`{"TableName":"dense","Item":` + denseItem("n", nestedLists(nests)) + `}`, 700 << 10,
		},
		{
			"BatchWriteItem 25 items", opBatchWrite,
			`{"RequestItems":{"dense":[` + strings.Join(batch, ",") + `]}}`, 13 << 20,
		},
		{
			"TransactWriteItems 100 actions", opTransactWrite,
			`{"TransactItems":[` + strings.Join(transaction, ",") + `]}`, 11 << 20,
		},
		{
			"UpdateItem value", opUpdateItem, `{"TableName":"dense","Key":{"id":{"S":"q"}},` +
				`"UpdateExpression":"SET l = :v","ExpressionAttributeValues":{":v":` + dense + `}}`, 1 << 20,
		},
		{
			"Query filter value", opQuery, `{"TableName":"dense","KeyConditionExpression":"id = :k",` +
				`"FilterExpression":"l = :v","ExpressionAttributeValues":{":k":{"S":"q"},":v":` + dense + `}}`, 1 << 20,
		},
		{
			"Scan filter value", opScan, `{"TableName":"dense","FilterExpression":"l = :v",` +
				`"ExpressionAttributeValues":{":v":` + dense + `}}`, 1 << 20,
		},
		{
			"ExecuteStatement parameter", "ExecuteStatement",
			`{"Statement":"SELECT id FROM dense WHERE id = 'q' AND l = ?","Parameters":[` + dense + `]}`, 1 << 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Greater(t, len(tt.body), tt.wantBytes, "the body is the dense one")
			require.Less(t, int64(len(tt.body)), protocol.DefaultMaxBody, "and under the cap")

			got := s.call(t, usEast1, tt.operation, tt.body)

			assert.Equal(t, http.StatusOK, got.status, got.body[:min(len(got.body), 300)])
		})
	}
}

// Test_decodeBudget_startKeyIsAdmittedAndAnsweredByTheHandler: a dense value in
// ExclusiveStartKey is not a legal key, but decoding admits it: the handler
// answers ValidationException, as it does for any key of the wrong shape, and
// the budget does not.
func Test_decodeBudget_startKeyIsAdmittedAndAnsweredByTheHandler(t *testing.T) {
	noMemoryLimit(t)

	s := newStack(t)
	s.ok(t, opCreate, hashTable("keys"))

	dense := `{"L":[` + numberList(numbers) + `]}`

	for _, operation := range []string{opQuery, opScan} {
		body := `{"TableName":"keys","ExclusiveStartKey":{"id":` + dense + `}}`
		if operation == opQuery {
			body = `{"TableName":"keys","KeyConditionExpression":"id = :k","ExpressionAttributeValues":{":k":{"S":"x"}},` +
				`"ExclusiveStartKey":{"id":` + dense + `}}`
		}

		got := s.call(t, usEast1, operation, body)

		assert.Equal(t, http.StatusBadRequest, got.status, got.body)
		assert.Contains(t, got.body, "ValidationException", operation)
		assert.NotContains(t, got.body, "more than gostack allows", operation)
	}

	longKey := strings.Repeat("k", 2048)
	got := s.call(t, usEast1, opScan, `{"TableName":"keys","ExclusiveStartKey":{"id":{"S":"`+longKey+`"}}}`)
	assert.Equal(t, http.StatusOK, got.status, got.body)
}

// Test_decodeBudget_pastTheLimitsIsRefusedBeforeDecoding: a list of
// attribute values that set no member is no legal item (an AttributeValue
// sets exactly one), and each costs 1,600 bytes charged for 3 bytes of body,
// 533 times, over the 256 the budget allows. It is refused with the json
// protocol's SerializationException before anything is decoded, so the heap
// grows by a small part of what decoding it would allocate. A body one byte over
// the cap is refused as a body over the cap, at any density.
func Test_decodeBudget_pastTheLimitsIsRefusedBeforeDecoding(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("over"))

	const elements = 100_000

	body := `{"TableName":"over","Item":` + denseItem("x", strings.TrimSuffix(strings.Repeat("{},", elements), ",")) + `}`

	var before, after runtime.MemStats

	runtime.GC() //nolint:revive // the baseline is what is live, not garbage
	runtime.ReadMemStats(&before)

	got := s.call(t, usEast1, opPut, body)

	runtime.ReadMemStats(&after)

	assert.Equal(t, http.StatusBadRequest, got.status, got.body)
	assert.Contains(t, got.body, "SerializationException")
	assert.Contains(t, got.body, "more than gostack allows")

	// Decoding the list would allocate at least 160 bytes an element.
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(elements*160/4),
		"the refusal came before the list was decoded")

	protocol.SetMaxBody(protocol.DefaultMaxBody)

	over := `{"TableName":"over","Item":{"id":{"S":"` + strings.Repeat("a", int(protocol.DefaultMaxBody)) + `"}}}`
	require.Greater(t, int64(len(over)), protocol.DefaultMaxBody)

	got = s.call(t, usEast1, opPut, over)

	assert.Equal(t, http.StatusRequestEntityTooLarge, got.status)
	assert.Contains(t, got.body, "RequestEntityTooLarge")
}

// convoyBatch is a BatchWriteItem of 25 items for client, each one string of
// 36,000 bytes and a list of 60 four-attribute records: about 1 MB that
// decodes into about 8.5 MiB charged.
func convoyBatch(client int) string {
	return textBatch(client, 36_000, 36_000)
}

// textBatch is convoyBatch with a string of firstText bytes in the first item
// and of text bytes in the others.
func textBatch(client, firstText, text int) string {
	record := `{"M":{"a":{"S":"alpha"},"b":{"N":"12345"},"c":{"BOOL":true},"d":{"S":"delta delta"}}}`
	requests := make([]string, 0, batchItems)

	for i := range batchItems {
		length := text
		if i == 0 {
			length = firstText
		}

		item := `{"id":{"S":"` + fmt.Sprint("c", client, "-", i) + `"},"text":{"S":"` + strings.Repeat("x", length) +
			`"},"records":{"L":[` + strings.TrimSuffix(strings.Repeat(record+",", 60), ",") + `]}}`
		requests = append(requests, `{"PutRequest":{"Item":`+item+`}}`)
	}

	return `{"RequestItems":{"burst":[` + strings.Join(requests, ",") + `]}}`
}

// Test_decodeBudget_aBurstOfDenseBodiesIsServedOrThrottled: 32 clients at once
// each send a BatchWriteItem that decodes into about 8.5 MiB charged, together
// over the default in-flight bound of 64 MiB. Each is charged within
// protocol.DecodeFactor, so the in-flight bound admits and throttles them as it
// does any body (docs/compat.md, In-flight bound): every client is served or
// throttled, at least one is served, and every request gives back what it held.
func Test_decodeBudget_aBurstOfDenseBodiesIsServedOrThrottled(t *testing.T) {
	const clients = 32

	bodies := make([]string, clients)
	for client := range clients {
		bodies[client] = convoyBatch(client)
		require.Greater(t, len(bodies[client]), 1<<20-100<<10)
	}

	serveBurst(t, bodies)
}

// serveBurst sends bodies to BatchWriteItem at once, one client each, and
// requires every one to be served or throttled, and at least one served.
func serveBurst(t *testing.T, bodies []string) {
	t.Helper()

	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	s := newStack(t)
	s.ok(t, opCreate, hashTable("burst"))

	server := httptest.NewServer(s.handler)
	t.Cleanup(server.Close)

	var (
		wg       sync.WaitGroup
		served   atomic.Int64
		failures sync.Map
	)

	start := make(chan struct{})

	for client, body := range bodies {
		wg.Go(func() {
			<-start

			status, answer := sendTo(t, server, "BatchWriteItem", body)

			switch {
			case status == http.StatusOK:
				served.Add(1)
			case status == http.StatusBadRequest && strings.Contains(answer, "ThrottlingException"):
			default:
				failures.Store(client, fmt.Sprint(status, " ", answer[:min(len(answer), 120)]))
			}
		})
	}

	close(start)
	wg.Wait()

	failures.Range(func(client, answer any) bool {
		t.Errorf("client %v: %v", client, answer)

		return true
	})

	assert.Positive(t, served.Load(), "the burst is served as far as the bound holds it")

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

// csvPut is a PutItem of key id whose attribute t is a string of n bytes of
// comma separated values, a comma every 4 bytes and no json structure at all.
func csvPut(id string, n int) string {
	return `{"TableName":"csv","Item":{"id":{"S":"` + id + `"},"t":{"S":"` + strings.Repeat("123,", n/4) + `"}}}`
}

// stalledBody is a request body that hands out its first bytes, then nothing
// until release is closed, and then the rest.
type stalledBody struct {
	body    string
	stopAt  int
	sent    int
	release chan struct{}
}

func (b *stalledBody) Read(p []byte) (int, error) {
	if b.sent == b.stopAt {
		<-b.release
		b.stopAt = len(b.body)
	}

	if b.sent == len(b.body) {
		return 0, io.EOF
	}

	n := copy(p, b.body[b.sent:b.stopAt])
	b.sent += n

	return n, nil
}

// Test_decodeBudget_aStringOfCommasIsPlannedAsText: a client that states 100 KB
// and sends 64 KiB + 1 of a string of comma separated values holds that and
// twice it for decoding, not what 16,000 list elements would need, so a
// second client's 100 KB string is served while the first has stopped. The
// commas are text inside a string and decode to nothing.
func Test_decodeBudget_aStringOfCommasIsPlannedAsText(t *testing.T) {
	// The second client waits for room for this long at most; it does not wait.
	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(time.Second)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	s := newStack(t)
	s.ok(t, opCreate, hashTable("csv"))

	server := httptest.NewServer(s.handler)
	t.Cleanup(server.Close)

	const size = 100_000

	body := csvPut("first", size)
	stalled := &stalledBody{body: body, stopAt: protocol.FreeBody + 1, release: make(chan struct{})}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, stalled)
	require.NoError(t, err)

	req.ContentLength = int64(len(body))
	signJSON(req, "PutItem")

	first := make(chan reply, 1)

	go func() {
		resp, err := server.Client().Do(req)
		if err != nil {
			first <- reply{body: err.Error()}

			return
		}

		defer func() { _ = resp.Body.Close() }()

		answer, _ := io.ReadAll(resp.Body)
		first <- reply{status: resp.StatusCode, body: string(answer)}
	}()

	// The first client's body has been read as far as it was sent and is
	// charged, when the pool holds it.
	require.Eventually(t, func() bool {
		_, held := protocol.InFlight()

		return held >= int64(len(body))
	}, 30*time.Second, time.Millisecond, "the pool holds the first body")

	status, answer := sendTo(t, server, "PutItem", csvPut("second", size))
	assert.Equal(t, http.StatusOK, status, answer)

	close(stalled.release)

	got := <-first
	assert.Equal(t, http.StatusOK, got.status, got.body)
}

// Test_decodeBudget_whatTheMemoryLimitCannotHoldIsRefused: in a process with a
// Go memory limit (a container's, set at start-up from its own), a body that
// would keep more than it live as it decodes is answered at once with
// the decode budget's answer and nothing is decoded, not decoded until the
// container's OOM killer ends the process. The same body is served with room.
func Test_decodeBudget_whatTheMemoryLimitCannotHoldIsRefused(t *testing.T) {
	protocol.SetInFlight(protocol.DefaultInFlight)

	old := debug.SetMemoryLimit(-1)

	t.Cleanup(func() {
		debug.SetMemoryLimit(old)
		protocol.SetInFlight(0)
	})

	s := newStack(t)
	s.ok(t, opCreate, hashTable("dense"))

	// 130,000 numbers in one list are charged 421 MB and keep about 28 MB live, 44 MB at their peak.
	body := `{"TableName":"dense","Item":` + denseItem("q", numberList(numbers)) + `}`

	debug.SetMemoryLimit(32 << 20)

	got := s.call(t, usEast1, opPut, body)
	assert.Equal(t, http.StatusBadRequest, got.status, got.body)
	assert.Contains(t, got.body, "more than gostack allows")

	debug.SetMemoryLimit(512 << 20)

	got = s.call(t, usEast1, opPut, body)
	assert.Equal(t, http.StatusOK, got.status, got.body[:min(len(got.body), 300)])

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

// signJSON makes req a json call to operation by the test account.
func signJSON(req *http.Request, operation string) {
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20240101/"+usEast1+
		"/dynamodb/aws4_request, SignedHeaders=host, Signature=abc")
}

func sendTo(t *testing.T, server *httptest.Server, operation, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}

	signJSON(req, operation)

	resp, err := server.Client().Do(req)
	if err != nil {
		return 0, err.Error()
	}

	defer func() { _ = resp.Body.Close() }()

	answer, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(answer)
}
