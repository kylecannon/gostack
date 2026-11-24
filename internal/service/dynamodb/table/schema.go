// Package table is DynamoDB's table engine: one table's items under its key
// schema, ordered range reads within a partition in both directions from an
// exclusive start key, local and global secondary indexes kept in step with
// every write, and one write path that reports the old and new image.
//
// Conditions, return values, stream records and transactions all hang off that
// report: Write hands the caller the stored item and takes back the item to
// store, so the caller decides under the table's lock. Lock takes several
// tables' locks in one order for a write that spans them.
//
// Each Table has its own lock; there is no lock across tables. Stored items are
// never changed in place, so an item a read returns may be kept.
package table

import (
	"strconv"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The projection types of a secondary index.
const (
	ProjectAll      = "ALL"
	ProjectKeysOnly = "KEYS_ONLY"
	ProjectInclude  = "INCLUDE"
)

// KeyAttribute is one key attribute: its name and its scalar type, S, N or B.
type KeyAttribute struct {
	Name string
	Type string
}

// KeySchema is a table's or an index's key: a hash attribute and, for a
// composite key, a range attribute. Range.Name is empty when there is none.
type KeySchema struct {
	Hash  KeyAttribute
	Range KeyAttribute
}

// HasRange reports whether the key has a range attribute.
func (k KeySchema) HasRange() bool {
	return k.Range.Name != ""
}

// Projection is the attributes an index copies from the table: ProjectAll,
// ProjectKeysOnly, or ProjectInclude with NonKeyAttributes.
type Projection struct {
	Type             string
	NonKeyAttributes []string
}

// IndexSpec describes a secondary index.
type IndexSpec struct {
	Name string
	// Global marks a global secondary index; a local one shares the table's
	// hash attribute.
	Global     bool
	Key        KeySchema
	Projection Projection
}

// HoldsAll reports whether the index holds every attribute of an item.
func (s *IndexSpec) HoldsAll() bool {
	return s.Projection.Type == ProjectAll || s.Projection.Type == ""
}

// Held is the names of the attributes a projecting index holds: the table's
// and the index's key attributes and, for ProjectInclude, the listed ones.
// The names may repeat and may be empty. NonKeyAttributes count only for
// ProjectInclude. It is meaningless when HoldsAll. tableKey is the key schema
// of the table.
func (s *IndexSpec) Held(tableKey KeySchema) []string {
	names := []string{tableKey.Hash.Name, tableKey.Range.Name, s.Key.Hash.Name, s.Key.Range.Name}
	if s.Projection.Type == ProjectInclude {
		names = append(names, s.Projection.NonKeyAttributes...)
	}

	return names
}

// Project returns the attributes of item that the index holds: all of them,
// or those Held. tableKey is the key schema of the table.
func (s *IndexSpec) Project(tableKey KeySchema, item value.Item) value.Item {
	if s.HoldsAll() {
		return item
	}

	names := s.Held(tableKey)
	out := make(value.Item, len(names))

	for _, name := range names {
		if v, ok := item[name]; ok && name != "" {
			out[name] = v
		}
	}

	return out
}

// encodeKey is the string that identifies the item with these key values in a
// map: the hash value, length prefixed, then the range value.
func encodeKey(hash, rng *api.AttributeValue) string {
	h := value.KeyString(hash)
	if rng == nil {
		return h
	}

	return strconv.Itoa(len(h)) + ":" + h + value.KeyString(rng)
}
