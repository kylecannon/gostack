package dynamodb

import (
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for placeholders a request gives with no
// expression to use them.
const (
	msgNamesUnusable  = "ExpressionAttributeNames can only be specified when using expressions"
	msgValuesUnusable = "ExpressionAttributeValues can only be specified when using expressions: "
)

// exprs is the expressions of one request: its placeholders, and the ones its
// expressions have used so far. Every expression a handler parses goes through
// it, so CheckSubstitutions runs first and CheckUnused runs once across all of
// them, and every expression kind passes its parser's size check before it is
// lexed.
type exprs struct {
	subs expression.Substitutions
	used map[string]struct{}
}

// newExprs checks a request's placeholders. hasExpr is whether the request
// gives any expression; valuesUsable whether it gives one that can name a
// :value, and nullMembers the members DynamoDB Local's message names when it
// does not.
//
//nolint:revive // flag-parameter: two facts about the request, not modes
func newExprs(
	names map[string]string, values map[string]api.AttributeValue, hasExpr, valuesUsable bool, nullMembers string,
) (*exprs, error) {
	if names != nil && !hasExpr {
		return nil, value.Invalid(msgNamesUnusable)
	}

	if values != nil && !valuesUsable {
		return nil, value.Invalid(msgValuesUnusable + nullMembers)
	}

	x := &exprs{
		subs: expression.Substitutions{Names: names, Values: values},
		used: make(map[string]struct{}, len(names)+len(values)),
	}

	err := expression.CheckSubstitutions(x.subs)
	if err != nil {
		return nil, fmt.Errorf("placeholders: %w", err)
	}

	return x, nil
}

// condition parses a ConditionExpression or FilterExpression, or returns nil
// when src is nil.
//
//nolint:ireturn // the parsed forms are the expression package's interfaces
func (x *exprs) condition(kind expression.Kind, src *string) (expression.Condition, error) {
	if src == nil {
		return nil, nil //nolint:nilnil // no expression
	}

	c, err := expression.ParseCondition(kind, *src, x.subs, x.used)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}

	return c, nil
}

// projection parses a ProjectionExpression, or returns nil when src is nil.
//
//nolint:ireturn // as condition
func (x *exprs) projection(src *string) (expression.Projection, error) {
	if src == nil {
		return nil, nil //nolint:nilnil // no expression
	}

	p, err := expression.ParseProjection(*src, x.subs, x.used)
	if err != nil {
		return nil, fmt.Errorf("projection: %w", err)
	}

	return p, nil
}

// update parses an UpdateExpression, or returns nil when src is nil.
func (x *exprs) update(src *string) (*expression.UpdateExpr, error) {
	if src == nil {
		return nil, nil //nolint:nilnil // no expression
	}

	u, err := expression.ParseUpdate(*src, x.subs, x.used)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}

	return u, nil
}

// keyCondition parses a KeyConditionExpression against key.
//
//nolint:ireturn // as condition
func (x *exprs) keyCondition(src string, key table.KeySchema) (expression.KeyCondition, error) {
	kc, err := expression.ParseKeyCondition(src, key, x.subs, x.used)
	if err != nil {
		return nil, fmt.Errorf("key condition: %w", err)
	}

	return kc, nil
}

// done refuses the placeholders no expression of the request used.
func (x *exprs) done() error {
	err := expression.CheckUnused(x.subs, x.used)
	if err != nil {
		return fmt.Errorf("placeholders: %w", err)
	}

	return nil
}

// enumError is the generated-validation message for one enum member.
func enumError(member, got, allowed string) error {
	var r constraint.Report
	r.Value(nil, member, got, "Member must satisfy enum value set: ["+allowed+"]")

	return value.Invalid(r.Message())
}

// consumption is what one call consumed: on the table, and on each index it
// read or wrote.
type consumption struct {
	table  float64
	local  map[string]float64
	global map[string]float64
}

// scale multiplies every amount in c by f.
func (c *consumption) scale(f float64) {
	c.table *= f

	for name := range c.local {
		c.local[name] *= f
	}

	for name := range c.global {
		c.global[name] *= f
	}
}

// addIndex adds units on the index spec names.
func (c *consumption) addIndex(spec *table.IndexSpec, units float64) {
	m := &c.local
	if spec.Global {
		m = &c.global
	}

	if *m == nil {
		*m = make(map[string]float64)
	}

	(*m)[spec.Name] += units
}

// report is the ConsumedCapacity ReturnConsumedCapacity asks for, or nil.
func (c *consumption) report(mode *string, tableName string) *api.ConsumedCapacity {
	if mode == nil || (*mode != consumedTotal && *mode != consumedIndexes) {
		return nil
	}

	total := c.table
	for _, u := range c.local {
		total += u
	}

	for _, u := range c.global {
		total += u
	}

	out := &api.ConsumedCapacity{TableName: new(tableName), CapacityUnits: new(total)}
	if *mode == consumedTotal {
		return out
	}

	out.Table = &api.Capacity{CapacityUnits: new(c.table)}
	out.LocalSecondaryIndexes = capacities(c.local)
	out.GlobalSecondaryIndexes = capacities(c.global)

	return out
}

func capacities(m map[string]float64) map[string]api.Capacity {
	if len(m) == 0 {
		return nil
	}

	out := make(map[string]api.Capacity, len(m))
	for name, u := range m {
		out[name] = api.Capacity{CapacityUnits: new(u)}
	}

	return out
}

// writeCapacity is what a write from old to updated consumed, as DynamoDB
// Local counts it: the table's units on the larger image, and on each index
// whose entry the write changed, the larger projected image when the item
// keeps its index key, both when it moves, and the one there is when it
// enters or leaves the index.
func writeCapacity(e *tableEntry, old, updated value.Item) consumption {
	return indexedWriteCapacity(e.data.Key(), e.indexSpecs(), old, updated)
}

// indexedWriteCapacity is writeCapacity on a table with key schema key and
// the indexes specs, for a caller that holds the table's lock and so cannot
// take the entry's.
func indexedWriteCapacity(key table.KeySchema, specs []table.IndexSpec, old, updated value.Item) consumption {
	c := consumption{table: writeUnits(old, updated)}

	for _, spec := range specs {
		oldIn, newIn := inIndex(&spec, old), inIndex(&spec, updated)

		var oldP, newP value.Item
		if oldIn {
			oldP = spec.Project(key, old)
		}

		if newIn {
			newP = spec.Project(key, updated)
		}

		switch {
		case !oldIn && !newIn:
			continue
		case oldIn && newIn && sameIndexKey(&spec, old, updated):
			if value.EqualItems(oldP, newP) {
				continue
			}

			c.addIndex(&spec, writeUnits(oldP, newP))
		default:
			units := 0.0
			if oldIn {
				units += writeUnits(oldP, nil)
			}

			if newIn {
				units += writeUnits(newP, nil)
			}

			c.addIndex(&spec, units)
		}
	}

	return c
}

// inIndex reports whether item carries every key attribute of the index.
func inIndex(spec *table.IndexSpec, item value.Item) bool {
	if item == nil {
		return false
	}

	if _, ok := item[spec.Key.Hash.Name]; !ok {
		return false
	}

	if spec.Key.HasRange() {
		if _, ok := item[spec.Key.Range.Name]; !ok {
			return false
		}
	}

	return true
}

// sameIndexKey reports whether a and b, both in the index, have one index key.
func sameIndexKey(spec *table.IndexSpec, a, b value.Item) bool {
	for _, name := range []string{spec.Key.Hash.Name, spec.Key.Range.Name} {
		if name == "" {
			continue
		}

		av, bv := a[name], b[name]
		if !value.Equal(&av, &bv) {
			return false
		}
	}

	return true
}

// indexSpecs returns the table's secondary indexes, local then global.
func (e *tableEntry) indexSpecs() []table.IndexSpec {
	e.mu.RLock()
	defer e.mu.RUnlock()

	specs := make([]table.IndexSpec, 0, len(e.meta.Local)+len(e.meta.Global))
	for i := range e.meta.Local {
		specs = append(specs, e.meta.indexSpec(&e.meta.Local[i], false))
	}

	for i := range e.meta.Global {
		specs = append(specs, e.meta.indexSpec(&e.meta.Global[i], true))
	}

	return specs
}
