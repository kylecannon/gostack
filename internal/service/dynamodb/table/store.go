package table

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// entry is one item in a partition: its range key value as value.SortKeyUntyped
// encodes it, which the type of the table's or index's range key fixes, empty
// for a key without one, and, in an index, the encoded table key, which orders
// items with the same index key and tells them apart.
type entry struct {
	sort string
	tie  string
	item value.Item
}

// compare orders entries by range key value, then by tie.
func (e *entry) compare(sortKey, tie string) int {
	if e.sort != "" && sortKey != "" {
		if c := strings.Compare(e.sort, sortKey); c != 0 {
			return c
		}
	}

	return strings.Compare(e.tie, tie)
}

// partition holds the entries that share a hash key value, in order.
type partition struct {
	entries []entry
}

// searchFirst returns the position of the first entry of p not before
// (sortKey, tie).
func searchFirst(p *partition, sortKey, tie string) int {
	return sort.Search(len(p.entries), func(i int) bool { return p.entries[i].compare(sortKey, tie) >= 0 })
}

// store holds partitions by encoded hash key value. Partitions are kept in a
// map; order lists their keys sorted, and is rebuilt when a partition has been
// added since, for a read across partitions.
type store struct {
	parts  map[string]*partition
	order  []string
	sorted bool
}

func newStore() store {
	return store{parts: make(map[string]*partition), sorted: true}
}

func (s *store) get(hash, sortKey, tie string) value.Item {
	p := s.parts[hash]
	if p == nil {
		return nil
	}

	if i := searchFirst(p, sortKey, tie); i < len(p.entries) && p.entries[i].compare(sortKey, tie) == 0 {
		return p.entries[i].item
	}

	return nil
}

// put stores e under hash, replacing an entry with the same range value and
// tie.
func (s *store) put(hash string, e entry) {
	p := s.parts[hash]
	if p == nil {
		p = &partition{}
		s.parts[hash] = p
		s.sorted = false
	}

	i := searchFirst(p, e.sort, e.tie)
	if i < len(p.entries) && p.entries[i].compare(e.sort, e.tie) == 0 {
		p.entries[i] = e

		return
	}

	p.entries = slices.Insert(p.entries, i, e)
}

// remove drops the entry with this range value and tie, and its partition when
// it was the last.
func (s *store) remove(hash, sortKey, tie string) {
	p := s.parts[hash]
	if p == nil {
		return
	}

	i := searchFirst(p, sortKey, tie)
	if i >= len(p.entries) || p.entries[i].compare(sortKey, tie) != 0 {
		return
	}

	p.entries = slices.Delete(p.entries, i, i+1)
	if len(p.entries) == 0 {
		delete(s.parts, hash)
	}
}

// sortedKeys returns the partition keys in order. It rebuilds the order when a
// partition was added since the last call, so the caller holds the write lock
// when needsSort reports true.
func (s *store) sortedKeys() []string {
	if !s.sorted {
		s.order = slices.Sorted(maps.Keys(s.parts))
		s.sorted = true
	}

	return s.order
}

func (s *store) needsSort() bool {
	return !s.sorted
}
