// Package expression declares the parsed forms of DynamoDB's four expression
// languages and the entry points that evaluate them against an item: condition
// and filter, key condition, projection, and update. Each language has one
// parsed form, built once per request with the request's
// ExpressionAttributeNames and ExpressionAttributeValues already substituted,
// so evaluation reads no placeholder.
//
// This file is the contract the parsers are written against; the parsers and
// the evaluators live beside it. A parse error is a ValidationException with
// DynamoDB's message (value.Invalid), reported for the first problem in the
// expression's text.
package expression

import (
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Substitutions are a request's ExpressionAttributeNames, by "#name"
// placeholder, and ExpressionAttributeValues, by ":value" placeholder. A
// parser records which ones the expression used, so the caller can refuse the
// unused ones as DynamoDB does across all of a request's expressions.
type Substitutions struct {
	Names  map[string]string
	Values map[string]api.AttributeValue
}

// PathElement is one step of a document path: an attribute or map key name,
// or a list index when Name is empty.
type PathElement struct {
	Name  string
	Index int
}

// Path is a document path, such as a.b[2].c, with its names substituted.
type Path []PathElement

// Condition is a parsed ConditionExpression or FilterExpression: a boolean
// over one item.
type Condition interface {
	// Eval reports whether item meets the condition. item is nil for a write
	// to a key that holds no item, which every comparison treats as missing
	// attributes. An error is a ValidationException DynamoDB raises only on
	// evaluation, such as an operand of the wrong type for size().
	Eval(item value.Item) (bool, error)
}

// KeyCondition is a parsed KeyConditionExpression: an equality on the hash
// attribute and at most one comparison on the range attribute, checked against
// the key schema of the table or index the query reads.
type KeyCondition interface {
	// Hash returns the hash attribute's name and the value it must equal.
	Hash() (name string, v api.AttributeValue)
	// Range returns the range attribute's name and the comparison on it, and
	// false when the expression has none.
	Range() (name string, cond table.SortCondition, ok bool)
}

// Projection is a parsed ProjectionExpression: the document paths a read
// returns.
type Projection interface {
	// Paths returns the paths in the order the expression lists them.
	Paths() []Path
	// Apply returns a new item holding only the projected parts of item; a
	// path to an attribute that is not there is left out. item is not changed.
	Apply(item value.Item) value.Item
}

// Update is a parsed UpdateExpression: its SET, REMOVE, ADD and DELETE
// clauses. ApplyStored writes one into the item a table stores.
type Update interface {
	// Paths returns every path the update writes, so the caller can report the
	// UPDATED_OLD and UPDATED_NEW return values.
	Paths() []Path
}
