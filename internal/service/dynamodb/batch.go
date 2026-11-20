package dynamodb

import (
	"maps"
	"slices"
	"strconv"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The limits of the batch and transaction operations.
const (
	maxBatchGetKeys     = 100
	maxBatchWrites      = 25
	maxTransactItems    = 100
	maxBatchGetBytes    = 16 << 20
	maxTransactBytes    = 4 << 20
	maxClientTokenRunes = 36
)

// requestTables returns the table names of a batch request's map in the order
// DynamoDB Local reads them.
func requestTables[V any](m map[string]V) []string {
	return expression.JavaHashOrder(slices.Collect(maps.Keys(m)))
}

// itemKey is the identity of an item within one table: its key values, in the
// table's key order. Two keys that name one item have one itemKey. The values
// must have passed value.Validate.
func itemKey(e *tableEntry, key value.Item) string {
	schema := e.data.Key()
	h := key[schema.Hash.Name]
	out := value.KeyString(&h)

	if schema.HasRange() {
		r := key[schema.Range.Name]
		out = strconv.Itoa(len(out)) + ":" + out + value.KeyString(&r)
	}

	return out
}

// add adds what o consumed to c.
func (c *consumption) add(o consumption) {
	c.table += o.table

	for name, u := range o.local {
		c.addUnits(&c.local, name, u)
	}

	for name, u := range o.global {
		c.addUnits(&c.global, name, u)
	}
}

func (*consumption) addUnits(m *map[string]float64, name string, units float64) {
	if *m == nil {
		*m = make(map[string]float64)
	}

	(*m)[name] += units
}

// tableConsumption is what one call consumed on each table it touched.
type tableConsumption map[string]*consumption

// on returns the consumption of the named table, adding it when it is new.
func (t tableConsumption) on(name string) *consumption {
	c := t[name]
	if c == nil {
		c = &consumption{}
		t[name] = c
	}

	return c
}

// report is the ConsumedCapacity list ReturnConsumedCapacity asks for, a
// table at a time in DynamoDB Local's order, or nil.
func (t tableConsumption) report(mode *string) []api.ConsumedCapacity {
	if mode == nil || (*mode != consumedTotal && *mode != consumedIndexes) {
		return nil
	}

	out := make([]api.ConsumedCapacity, 0, len(t))
	for _, name := range requestTables(t) {
		out = append(out, *t[name].report(mode, name))
	}

	return out
}
