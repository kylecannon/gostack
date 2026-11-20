package table

import (
	"errors"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// ErrDuplicateKey is returned by Load for two items with the same key.
var ErrDuplicateKey = errors.New("two items have the same key")

// errNotEmpty is returned by Load for a table that holds items.
var errNotEmpty = errors.New("load into a table that holds items")

// Image is a table's items as one moment left them. ImageHeld takes it under
// the table's lock; Items puts it in order after the lock is released.
type Image struct {
	parts  []imagePart
	items  []value.Item
	sorted bool
}

// imagePart is one partition of an Image: its encoded hash value and where
// its items are in Image.items.
type imagePart struct {
	hash       string
	start, end int
}

// ImageHeld copies one reference per item, partition by partition, and the
// partitions' keys. The caller holds the table's lock through Lock or
// ReadLock, for this copy and no longer: stored items are immutable, so the
// image is one that later writes do not change, and putting it in order,
// which a table with new partitions needs, waits for Image.Items.
func (t *Table) ImageHeld() Image {
	img := Image{items: make([]value.Item, 0, t.count), sorted: t.primary.sorted}

	visit := func(hash string, p *partition) {
		start := len(img.items)

		for _, e := range p.entries {
			img.items = append(img.items, e.item)
		}

		if !img.sorted {
			img.parts = append(img.parts, imagePart{hash: hash, start: start, end: len(img.items)})
		}
	}

	if img.sorted {
		// A delete drops an emptied partition from parts but leaves its key
		// in order until the next rebuild.
		for _, hash := range t.primary.order {
			if p := t.primary.parts[hash]; p != nil {
				visit(hash, p)
			}
		}
	} else {
		img.parts = make([]imagePart, 0, len(t.primary.parts))

		for hash, p := range t.primary.parts {
			visit(hash, p)
		}
	}

	return img
}

// Items returns the image's items as Scan reads them: partition after
// partition in the order of their encoded hash values, in range key order
// within each. The image is left empty.
func (img *Image) Items() []value.Item {
	if img.sorted {
		items := img.items
		*img = Image{}

		return items
	}

	slices.SortFunc(img.parts, func(a, b imagePart) int { return strings.Compare(a.hash, b.hash) })

	items := make([]value.Item, 0, len(img.items))
	for _, part := range img.parts {
		items = append(items, img.items[part.start:part.end]...)
	}

	*img = Image{}

	return items
}

// Load fills an empty table with items, as a restore does: each is checked
// as every stored item is (checkStored) and stored with the index entries of
// the indexes it is in (indexed), then every partition is
// sorted once, so a load costs O(n log n) whatever the order of items. Load
// takes the items as they are, so the caller hands over maps nothing else
// holds. It tells no observer: what a restore puts back is not a write.
func (t *Table) Load(items []value.Item) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.count != 0 {
		return errNotEmpty
	}

	for _, item := range items {
		err := t.checkStored(item)
		if err != nil {
			return err
		}
	}

	err := t.loadPrimary(items)
	if err != nil {
		return err
	}

	for _, idx := range t.indexList() {
		key := idx.spec.Key

		fill(&idx.data, items, func(dst []byte, item value.Item) ([]byte, bool) {
			if !indexed(&idx.spec, item) {
				return dst, false
			}

			h := item[key.Hash.Name]

			return value.AppendKeyString(dst, &h), true
		}, func(item value.Item) entry {
			idx.count++
			idx.size += value.ItemSize(item)

			return entry{sort: sortKey(key, item), tie: t.tie(item), item: item}
		})
		sortPartitions(&idx.data)
	}

	return nil
}

// loadPrimary puts items in the table's own store. A table without a range
// key has one item per partition, so its partitions are made in one pass;
// with one, fill sizes them first.
func (t *Table) loadPrimary(items []value.Item) error {
	add := func(item value.Item) entry {
		t.count++
		t.size += value.ItemSize(item)

		return entry{sort: sortKey(t.key, item), item: item}
	}

	if t.key.HasRange() {
		fill(&t.primary, items, func(dst []byte, item value.Item) ([]byte, bool) {
			h := item[t.key.Hash.Name]

			return value.AppendKeyString(dst, &h), true
		}, add)

		if !sortPartitions(&t.primary) {
			return ErrDuplicateKey
		}

		return nil
	}

	t.primary.parts = make(map[string]*partition, len(items))
	t.primary.sorted = false

	for _, item := range items {
		h := item[t.key.Hash.Name]
		hash := value.KeyString(&h)

		if t.primary.parts[hash] != nil {
			return ErrDuplicateKey
		}

		t.primary.parts[hash] = &partition{entries: []entry{add(item)}}
	}

	return nil
}

// sortKey is the range key value of item under key, nil without one.
func sortKey(key KeySchema, item value.Item) *api.AttributeValue {
	if !key.HasRange() {
		return nil
	}

	r := item[key.Range.Name]

	return &r
}

// fill puts in the empty store s an entry, made by add, for each item that
// hash appends a partition key for. It counts the entries of each partition
// first and makes each at its size, so a partition of many items is allocated
// once rather than grown, and its key string once. The order is left to
// sortPartitions.
func fill(s *store, items []value.Item, hash func([]byte, value.Item) ([]byte, bool),
	add func(value.Item) entry,
) {
	var (
		key    []byte
		counts = map[*partition]int{}
	)

	s.sorted = false

	for _, item := range items {
		var ok bool

		key, ok = hash(key[:0], item)
		if !ok {
			continue
		}

		p := s.parts[string(key)]
		if p == nil {
			p = &partition{}
			s.parts[string(key)] = p
		}

		counts[p]++
	}

	for p, n := range counts {
		p.entries = make([]entry, 0, n)
	}

	for _, item := range items {
		var ok bool

		key, ok = hash(key[:0], item)
		if ok {
			p := s.parts[string(key)]
			p.entries = append(p.entries, add(item))
		}
	}
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
