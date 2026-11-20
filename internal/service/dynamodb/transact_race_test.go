package dynamodb_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// directTable creates a table keyed by the string id, calling the provider
// directly.
func directTable(tb testing.TB, p *dynamodb.Provider, name string) {
	tb.Helper()

	_, err := p.CreateTable(tb.Context(), &api.CreateTableInput{
		TableName:            new(name),
		KeySchema:            []api.KeySchemaElement{{AttributeName: new("id"), KeyType: new("HASH")}},
		AttributeDefinitions: []api.AttributeDefinition{{AttributeName: new("id"), AttributeType: new("S")}},
		ProvisionedThroughput: &api.ProvisionedThroughput{
			ReadCapacityUnits: new(int64(5)), WriteCapacityUnits: new(int64(5)),
		},
	})
	require.NoError(tb, err)
}

func putAction(tableName, v string) api.TransactWriteItem {
	return api.TransactWriteItem{Put: &api.Put{
		TableName: new(tableName),
		Item:      map[string]api.AttributeValue{"id": {S: new("x")}, "v": {N: new(v)}},
	}}
}

func getAction(tableName, id string) api.TransactGetItem {
	return api.TransactGetItem{Get: &api.Get{
		TableName: new(tableName),
		Key:       map[string]api.AttributeValue{"id": {S: new(id)}},
	}}
}

// Test_transact_oppositeOrder: two writers put one value into an item of each
// of tables A and B, one naming A first and the other B first, for 1,000
// rounds each, while a reader reads both items in one transaction. Nothing
// deadlocks, and the reader never sees the two items differ: no transaction
// is seen half written.
func Test_transact_oppositeOrder(t *testing.T) {
	const rounds = 1000

	p := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
	directTable(t, p, "tabA")
	directTable(t, p, "tabB")

	ctx := t.Context()
	_, err := p.TransactWriteItems(ctx, &api.TransactWriteItemsInput{
		TransactItems: []api.TransactWriteItem{putAction("tabA", "0"), putAction("tabB", "0")},
	})
	require.NoError(t, err)

	var (
		wg     sync.WaitGroup
		failed = make(chan error, 3)
		stop   = make(chan struct{})
	)

	writer := func(first, second string, base int) {
		defer wg.Done()

		for i := range rounds {
			v := strconv.Itoa(base + i)

			_, err := p.TransactWriteItems(ctx, &api.TransactWriteItemsInput{
				TransactItems: []api.TransactWriteItem{putAction(first, v), putAction(second, v)},
			})
			if err != nil {
				failed <- err

				return
			}
		}
	}

	wg.Add(2)

	go writer("tabA", "tabB", 0)
	go writer("tabB", "tabA", rounds)

	reader := make(chan struct{})

	go func() {
		defer close(reader)

		for {
			select {
			case <-stop:
				return
			default:
			}

			out, err := p.TransactGetItems(ctx, &api.TransactGetItemsInput{
				TransactItems: []api.TransactGetItem{getAction("tabB", "x"), getAction("tabA", "x")},
			})
			if err != nil {
				failed <- err

				return
			}

			if a, b := *out.Responses[0].Item["v"].N, *out.Responses[1].Item["v"].N; a != b {
				failed <- errors.New("half-written transaction seen: tabB " + a + ", tabA " + b)

				return
			}
		}
	}()

	writers := make(chan struct{})

	go func() {
		wg.Wait()
		close(writers)
	}()

	select {
	case <-writers:
	case <-time.After(time.Minute):
		t.Fatal("transactions over tabA and tabB in opposite orders deadlocked")
	}

	close(stop)
	<-reader
	close(failed)

	for err := range failed {
		require.NoError(t, err)
	}
}

// Test_transact_tokenWindow: a ClientRequestToken replays its successful
// request for ten minutes and refuses another request; after the window the
// token is free again.
func Test_transact_tokenWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
		directTable(t, p, "tab1")

		write := func(ctx context.Context, v string) error {
			_, err := p.TransactWriteItems(ctx, &api.TransactWriteItemsInput{
				ClientRequestToken: new("tok"),
				TransactItems:      []api.TransactWriteItem{putAction(tab1, v)},
			})

			return err
		}
		stored := func() string {
			out, err := p.GetItem(t.Context(), &api.GetItemInput{
				TableName: new("tab1"), Key: map[string]api.AttributeValue{"id": {S: new("x")}},
			})
			require.NoError(t, err)

			return *out.Item["v"].N
		}
		mismatch := func(err error) bool {
			var e *awserr.Error

			return errors.As(err, &e) && e.Code == "IdempotentParameterMismatchException"
		}

		ctx := t.Context()
		require.NoError(t, write(ctx, "1"))

		_, err := p.PutItem(ctx, &api.PutItemInput{
			TableName: new("tab1"),
			Item:      map[string]api.AttributeValue{"id": {S: new("x")}, "v": {N: new("9")}},
		})
		require.NoError(t, err)

		time.Sleep(10*time.Minute - time.Second)
		require.NoError(t, write(ctx, "1"))
		require.Equal(t, "9", stored(), "a replay inside the window writes nothing")
		require.True(t, mismatch(write(ctx, "2")))

		time.Sleep(time.Second)
		require.NoError(t, write(ctx, "2"))
		require.Equal(t, "2", stored(), "after the window the token is free")
		require.True(t, mismatch(write(ctx, "1")))
	})
}

// Test_write_indexChurn writes items carrying a GSI key while UpdateTable
// creates and deletes that GSI: the check before each write reads the table's
// index list under its lock, so -race finds nothing and no write answers 500.
func Test_write_indexChurn(t *testing.T) {
	const (
		addIndex = `{"TableName":"churn","AttributeDefinitions":[{"AttributeName":"h","AttributeType":"S"}],` +
			`"GlobalSecondaryIndexUpdates":[{"Create":{"IndexName":"hix","KeySchema":[{"AttributeName":"h",` +
			`"KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"},` +
			`"ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}}]}`
		dropIndex = `{"TableName":"churn","GlobalSecondaryIndexUpdates":[{"Delete":{"IndexName":"hix"}}]}`
		item      = `{"id":{"S":"x"},"h":{"S":"v"}}`
	)

	writes := map[string][2]string{
		"PutItem":            {"PutItem", `{"TableName":"churn","Item":` + item + `}`},
		"TransactWriteItems": {"TransactWriteItems", `{"TransactItems":[{"Put":{"TableName":"churn","Item":` + item + `}}]}`},
		opBatchWrite:         {opBatchWrite, `{"RequestItems":{"churn":[{"PutRequest":{"Item":` + item + `}}]}}`},
	}

	for name, w := range writes {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.ok(t, "CreateTable", hashTable("churn"))

			var wg sync.WaitGroup

			wg.Go(func() {
				for range 100 {
					s.call(t, usEast1, "UpdateTable", addIndex)
					s.call(t, usEast1, "UpdateTable", dropIndex)
				}
			})
			wg.Go(func() {
				for range 500 {
					if got := s.call(t, usEast1, w[0], w[1]); got.status != http.StatusOK {
						t.Errorf("%s: %d %s", w[0], got.status, got.body)

						return
					}
				}
			})
			wg.Wait()
		})
	}
}
