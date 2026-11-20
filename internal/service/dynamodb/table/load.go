package table

import (
	"errors"
	"maps"
	"slices"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// ErrDuplicateKey is returned by Load for two items with the same key.
var ErrDuplicateKey = errors.New("two items have the same key")

// errNotEmpty is returned by Load for a table that holds items.
var errNotEmpty = errors.New("load into a table that holds items")

// ItemsHeld returns every stored item, partition after partition in the order
// of their encoded hash values and in range key order within each, as Scan
// reads them. The caller holds the table's lock through Lock or ReadLock. It
// copies one reference per item and nothing else: stored items are immutable,
// so the slice is an image of the table that later writes do not change, and
// the caller encodes it after releasing the lock.
func (t *Table) ItemsHeld() []value.Item {
	items := make([]value.Item, 0, t.count)

	// A sorted copy of the keys: the store's own order may be stale, and
	// rebuilding it needs the write lock.
	keys := t.primary.order
	if !t.primary.sorted {
		keys = slices.Sorted(maps.Keys(t.primary.parts))
	}

	for _, hash := range keys {
		for _, e := range t.primary.parts[hash].entries {
			items = append(items, e.item)
		}
	}

	return items
}

// Load fills an empty table with items, as a restore does: each is checked
// with CheckItem and stored with its index entries, then every partition is
// sorted once, so a load costs O(n log n) whatever the order of items. Load
// takes the items as they are, so the caller hands over maps nothing else
// holds. It tells no observer: what a restore puts back is not a write.
func (t *Table) Load(items []value.Item) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.count != 0 {
		return errNotEmpty
	}

	indexes := t.indexList()

	for _, item := range items {
		err := t.CheckItem(item)
		if err != nil {
			return err
		}

		size := value.ItemSize(item)
		hash, rng := t.primaryKey(item)
		appendEntry(&t.primary, hash, entry{sort: rng, item: item})
		t.count++
		t.size += size

		for _, idx := range indexes {
			if ihash, isort, ok := indexKey(&idx.spec, item); ok {
				appendEntry(&idx.data, ihash, entry{sort: isort, tie: t.tie(item), item: item})
				idx.count++
				idx.size += size
			}
		}
	}

	if !sortPartitions(&t.primary) {
		return ErrDuplicateKey
	}

	for _, idx := range indexes {
		sortPartitions(&idx.data)
	}

	return nil
}

// appendEntry adds e to the end of its partition, leaving the order to
// sortPartitions.
func appendEntry(s *store, hash string, e entry) {
	p := s.parts[hash]
	if p == nil {
		p = &partition{}
		s.parts[hash] = p
		s.sorted = false
	}

	p.entries = append(p.entries, e)
}

// sortPartitions sorts every partition of s and reports whether no two
// entries of one partition compare equal. Index entries never do, since the
// tie is the table key, which Load's check on the table makes unique.
func sortPartitions(s *store) bool {
	unique := true

	for _, p := range s.parts {
		slices.SortStableFunc(p.entries, func(a, b entry) int { return a.compare(b.sort, b.tie) })

		for i := 1; i < len(p.entries); i++ {
			if p.entries[i-1].compare(p.entries[i].sort, p.entries[i].tie) == 0 {
				unique = false
			}
		}
	}

	return unique
}
