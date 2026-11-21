package dynamodb_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// Test_decodeBudget_worstLegalBody sends, for each operation that takes
// AttributeValues, the densest body its documented limits allow, and every one
// is served: none is refused for decoding into more than its budget. The
// shapes are the worst measured (docs/architecture.md, Request memory): a list
// of the smallest values at 400 KB of item, and lists nested to the depth
// limit. The batch and the transaction are the largest each admits.
func Test_decodeBudget_worstLegalBody(t *testing.T) {
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
// decodes into about 15 MiB charged.
func convoyBatch(client int) string {
	record := `{"M":{"a":{"S":"alpha"},"b":{"N":"12345"},"c":{"BOOL":true},"d":{"S":"delta delta"}}}`
	text := strings.Repeat("x", 36_000)
	requests := make([]string, 0, batchItems)

	for i := range batchItems {
		item := `{"id":{"S":"` + fmt.Sprint("c", client, "-", i) + `"},"text":{"S":"` + text +
			`"},"records":{"L":[` + strings.TrimSuffix(strings.Repeat(record+",", 60), ",") + `]}}`
		requests = append(requests, `{"PutRequest":{"Item":`+item+`}}`)
	}

	return `{"RequestItems":{"burst":[` + strings.Join(requests, ",") + `]}}`
}

// Test_decodeBudget_aBurstOfDenseBodiesIsServed: 32 clients at once each send
// a 1 MB BatchWriteItem that decodes into about 8.5 MiB charged, together over
// the default in-flight bound of 64 MiB. A body is admitted with the room its
// elements will need (service.Registration.DecodeUnit), so the bound admits as
// many as fit and the others wait their turn, none part way, and every client
// is served. Admitted with twice its length, as before, each held a few MiB
// and waited for more that none could give, and most were throttled at the
// wait.
func Test_decodeBudget_aBurstOfDenseBodiesIsServed(t *testing.T) {
	const clients = 32

	// The wait is long so that a slow machine, or the race detector, serving
	// the bodies one after another does not throttle the last; a body that
	// waited on the others part way would still wait it out and fail.
	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(20 * time.Second)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	s := newStack(t)
	s.ok(t, opCreate, hashTable("burst"))

	server := httptest.NewServer(s.handler)
	t.Cleanup(server.Close)

	var (
		wg       sync.WaitGroup
		served   atomic.Int64
		failures sync.Map
	)

	bodies := make([]string, clients)
	for client := range clients {
		bodies[client] = convoyBatch(client)
		require.Greater(t, len(bodies[client]), 1<<20-100<<10)
	}

	start := make(chan struct{})

	for client := range clients {
		wg.Go(func() {
			<-start

			status, answer := sendBatch(t, server, bodies[client])
			if status == http.StatusOK {
				served.Add(1)

				return
			}

			failures.Store(client, fmt.Sprint(status, " ", answer[:min(len(answer), 120)]))
		})
	}

	close(start)
	wg.Wait()

	failures.Range(func(client, answer any) bool {
		t.Errorf("client %v: %v", client, answer)

		return true
	})

	assert.Equal(t, int64(clients), served.Load(), "every client of the burst is served")

	_, held := protocol.InFlight()
	assert.Zero(t, held, "every request gives back what it held")
}

func sendBatch(t *testing.T, server *httptest.Server, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}

	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810.BatchWriteItem")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20240101/"+usEast1+
		"/dynamodb/aws4_request, SignedHeaders=host, Signature=abc")

	resp, err := server.Client().Do(req)
	if err != nil {
		return 0, err.Error()
	}

	defer func() { _ = resp.Body.Close() }()

	answer, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(answer)
}
