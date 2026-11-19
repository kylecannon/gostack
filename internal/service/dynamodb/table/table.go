package table

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// ErrNoIndex is returned for a read of an index the table does not have.
var ErrNoIndex = errors.New("no such index")

// errKeyChanged is returned when a write's new item has a key other than the
// one it was asked to write.
var errKeyChanged = errors.New("write changed the item's key")

// nextID numbers tables, so Lock takes their locks in one order.
var nextID atomic.Uint64

// Table holds one table's items and its secondary indexes.
type Table struct {
	id uint64

	// mu guards everything below. A write holds it for writing across the
	// caller's decision and the index updates, so a reader never sees an item
	// in the table and not in its indexes.
	mu      sync.RWMutex
	key     KeySchema
	primary store
	indexes []*index
	count   int
	size    int
}

// index is one secondary index: its spec, its own partitions and its counts.
type index struct {
	spec  IndexSpec
	data  store
	count int
	size  int
}

// Change is what a write did: the item stored before and the item stored
// after, either nil when there was none.
type Change struct {
	Old value.Item
	New value.Item
}

// Changed reports whether the write changed what is stored: an item appeared,
// disappeared, or was replaced by one that is not Equal.
func (c Change) Changed() bool {
	if (c.Old == nil) != (c.New == nil) {
		return true
	}

	return c.Old != nil && !value.EqualItems(c.Old, c.New)
}

// New returns an empty table with key schema key and the given indexes.
func New(key KeySchema, indexes []IndexSpec) *Table {
	t := &Table{id: nextID.Add(1), key: key, primary: newStore()}
	for _, spec := range indexes {
		t.indexes = append(t.indexes, &index{spec: spec, data: newStore()})
	}

	return t
}

// Key returns the table's key schema.
func (t *Table) Key() KeySchema {
	return t.key
}

// Stats returns the number of items and their total size.
func (t *Table) Stats() (count, size int) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.count, t.size
}

// IndexStats returns the number of items in the named index and their total
// size, and false when there is no such index.
func (t *Table) IndexStats(name string) (count, size int, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	idx := t.index(name)
	if idx == nil {
		return 0, 0, false
	}

	return idx.count, idx.size, true
}

// Get returns the item with key, or nil. key must have passed CheckKey.
func (t *Table) Get(key value.Item) value.Item {
	t.mu.RLock()
	defer t.mu.RUnlock()

	hash, rng := t.primaryKey(key)

	return t.primary.get(hash, rng, "")
}

// GetHeld is Get for a caller that holds the table's lock through Lock or
// ReadLock, as a transaction does.
func (t *Table) GetHeld(key value.Item) value.Item {
	hash, rng := t.primaryKey(key)

	return t.primary.get(hash, rng, "")
}

// Write is the one write path. It calls decide with the item stored at key,
// nil when there is none, and stores what decide returns: a new item, which
// must have the same key, or nil to delete. An error from decide stores
// nothing and is returned with the old image. The new item is checked with
// CheckItem first. decide runs under the table's write lock and must not call
// back into the table. key must have passed CheckKey or CheckItem.
func (t *Table) Write(key value.Item, decide func(old value.Item) (value.Item, error)) (Change, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.WriteHeld(key, decide)
}

// WriteHeld is Write for a caller that holds the table's lock through Lock, as
// a write across several tables does.
func (t *Table) WriteHeld(key value.Item, decide func(old value.Item) (value.Item, error)) (Change, error) {
	hash, rng := t.primaryKey(key)
	old := t.primary.get(hash, rng, "")

	updated, err := decide(old)
	if err != nil {
		return Change{Old: old}, err
	}

	if updated != nil {
		err := t.CheckItem(updated)
		if err != nil {
			return Change{Old: old}, err
		}

		if h, s := t.primaryKey(updated); h != hash || (s != nil && value.Compare(s, rng) != 0) {
			return Change{Old: old}, errKeyChanged
		}
	}

	if old != nil {
		t.unstore(hash, rng, old)
	}

	if updated != nil {
		t.store(hash, rng, updated)
	}

	return Change{Old: old, New: updated}, nil
}

// Lock takes the write locks of tables, each once, in one global order, so
// two writes across overlapping tables cannot deadlock. It returns the
// function that releases them, which the caller defers so that a panic
// between the two releases them too.
func Lock(tables ...*Table) (unlock func()) {
	ordered := lockOrder(tables)

	for _, t := range ordered {
		t.mu.Lock()
	}

	return func() {
		for _, t := range slices.Backward(ordered) {
			t.mu.Unlock()
		}
	}
}

// ReadLock is Lock with the read locks, for a read of several tables that must
// see one state of all of them. It takes them in Lock's order, so it cannot
// deadlock against Lock either.
func ReadLock(tables ...*Table) (unlock func()) {
	ordered := lockOrder(tables)

	for _, t := range ordered {
		t.mu.RLock()
	}

	return func() {
		for _, t := range slices.Backward(ordered) {
			t.mu.RUnlock()
		}
	}
}

// lockOrder is tables, each once, in the one global order every multi-table
// lock follows: the order in which the tables were created.
func lockOrder(tables []*Table) []*Table {
	ordered := slices.Clone(tables)
	slices.SortFunc(ordered, func(a, b *Table) int { return compareIDs(a.id, b.id) })

	return slices.Compact(ordered)
}

func compareIDs(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// AddIndex adds a secondary index and fills it from the items stored now.
func (t *Table) AddIndex(spec IndexSpec) {
	t.mu.Lock()
	defer t.mu.Unlock()

	idx := &index{spec: spec, data: newStore()}
	for _, hash := range t.primary.sortedKeys() {
		for _, e := range t.primary.parts[hash].entries {
			if ihash, isort, ok := indexKey(&spec, e.item); ok {
				idx.data.put(ihash, entry{sort: isort, tie: t.tie(e.item), item: e.item})
				idx.count++
				idx.size += value.ItemSize(e.item)
			}
		}
	}

	t.indexes = append(t.indexes, idx)
}

// DropIndex removes the named secondary index, and reports whether there was
// one.
func (t *Table) DropIndex(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	before := len(t.indexes)
	t.indexes = slices.DeleteFunc(t.indexes, func(idx *index) bool { return idx.spec.Name == name })

	return len(t.indexes) < before
}

// SortOp is the comparison a range read applies to the range key.
type SortOp int

// The range key comparisons. SortAny reads the whole partition.
const (
	SortAny SortOp = iota
	SortEQ
	SortLT
	SortLE
	SortGT
	SortGE
	SortBetween
	SortBeginsWith
)

// betweenOperands is the number of values SortBetween takes.
const betweenOperands = 2

// SortCondition limits a range read to the range key values it accepts:
// Values holds one operand, or two for SortBetween.
type SortCondition struct {
	Op     SortOp
	Values []api.AttributeValue
}

// Range reads the items of one partition of the table, or of the named index
// when indexName is not empty, whose range key meets cond, in range key order
// or its reverse. start, when not nil, is an exclusive start key: the table's
// key attributes and, for an index, the index's, and the read begins after it
// in the direction of the read. visit is called with each stored item, whole,
// under the table's read lock, until it returns false; it must not write to the
// table. The caller applies the index's projection.
//
//nolint:revive // flag-parameter: the direction of the read is part of the API
func (t *Table) Range(
	indexName string, hash *api.AttributeValue, cond SortCondition, start value.Item, forward bool,
	visit func(value.Item) bool,
) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	data, key, err := t.source(indexName)
	if err != nil {
		return err
	}

	p := data.parts[value.KeyString(hash)]
	if p == nil {
		return nil
	}

	lo, hi := bounds(p, cond)

	if start != nil {
		startSort, startTie := t.position(indexName, key, start)

		after := sort.Search(len(p.entries), func(i int) bool { return p.entries[i].compare(startSort, startTie) > 0 })
		if forward {
			lo = max(lo, after)
		} else {
			hi = min(hi, sort.Search(len(p.entries), func(i int) bool { return p.entries[i].compare(startSort, startTie) >= 0 }))
		}
	}

	visitRange(p.entries, lo, hi, forward, visit)

	return nil
}

//nolint:revive // flag-parameter: as Range
func visitRange(entries []entry, lo, hi int, forward bool, visit func(value.Item) bool) {
	if forward {
		for i := lo; i < hi; i++ {
			if !visit(entries[i].item) {
				return
			}
		}

		return
	}

	for i := hi - 1; i >= lo; i-- {
		if !visit(entries[i].item) {
			return
		}
	}
}

// Scan reads every item of the table, or of the named index, partition after
// partition in the order of their encoded hash values and in range key order
// within each, beginning after the exclusive start key start when it is not
// nil. visit is as for Range.
func (t *Table) Scan(indexName string, start value.Item, visit func(value.Item) bool) error {
	if t.needsSort(indexName) {
		t.sortKeys(indexName)
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	data, key, err := t.source(indexName)
	if err != nil {
		return err
	}

	keys := data.order
	if data.needsSort() {
		// A partition was added between the two locks: read the partitions
		// that were there, as a scan running beside writes may.
		keys = slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return data.parts[k] == nil })
	}

	first, startSort, startTie := 0, (*api.AttributeValue)(nil), ""

	var startHash string

	if start != nil {
		hv := start[key.Hash.Name]
		startHash = value.KeyString(&hv)
		startSort, startTie = t.position(indexName, key, start)
		first = sort.SearchStrings(keys, startHash)
	}

	for _, hash := range keys[first:] {
		p := data.parts[hash]
		if p == nil {
			continue
		}

		lo := 0
		if start != nil && hash == startHash {
			lo = sort.Search(len(p.entries), func(i int) bool { return p.entries[i].compare(startSort, startTie) > 0 })
		}

		for _, e := range p.entries[lo:] {
			if !visit(e.item) {
				return nil
			}
		}
	}

	return nil
}

func (t *Table) needsSort(indexName string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	data, _, err := t.source(indexName)

	return err == nil && data.needsSort()
}

// sortKeys sorts the partition keys of the table, or of the named index,
// under the write lock.
func (t *Table) sortKeys(indexName string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, _, err := t.source(indexName)
	if err == nil {
		data.sortedKeys()
	}
}

// source returns the partitions and key schema of the table, or of the named
// index.
func (t *Table) source(indexName string) (*store, KeySchema, error) {
	if indexName == "" {
		return &t.primary, t.key, nil
	}

	idx := t.index(indexName)
	if idx == nil {
		return nil, KeySchema{}, ErrNoIndex
	}

	return &idx.data, idx.spec.Key, nil
}

// position returns the range value and tie of an exclusive start key in the
// table or index read.
func (t *Table) position(indexName string, key KeySchema, start value.Item) (*api.AttributeValue, string) {
	var sortValue *api.AttributeValue

	if key.HasRange() {
		v := start[key.Range.Name]
		sortValue = &v
	}

	if indexName == "" {
		return sortValue, ""
	}

	return sortValue, t.tie(start)
}

// bounds returns the positions [lo, hi) of the entries whose range value meets
// cond.
func bounds(p *partition, cond SortCondition) (int, int) {
	n := len(p.entries)
	if cond.Op == SortAny || len(cond.Values) == 0 || n == 0 || p.entries[0].sort == nil {
		return 0, n
	}

	v := &cond.Values[0]
	firstGE := func(x *api.AttributeValue) int {
		return sort.Search(n, func(i int) bool { return value.Compare(p.entries[i].sort, x) >= 0 })
	}
	firstGT := func(x *api.AttributeValue) int {
		return sort.Search(n, func(i int) bool { return value.Compare(p.entries[i].sort, x) > 0 })
	}

	switch cond.Op {
	case SortEQ:
		return firstGE(v), firstGT(v)
	case SortLT:
		return 0, firstGE(v)
	case SortLE:
		return 0, firstGT(v)
	case SortGT:
		return firstGT(v), n
	case SortGE:
		return firstGE(v), n
	case SortBetween:
		if len(cond.Values) < betweenOperands {
			return 0, 0
		}

		return firstGE(v), max(firstGE(v), firstGT(&cond.Values[1]))
	case SortBeginsWith:
		lo := firstGE(v)
		hi := lo

		for hi < n && hasPrefix(p.entries[hi].sort, v) {
			hi++
		}

		return lo, hi
	default:
		return 0, n
	}
}

// Meets reports whether the range key value v meets c.
func (c SortCondition) Meets(v *api.AttributeValue) bool {
	if c.Op == SortAny || len(c.Values) == 0 {
		return true
	}

	n := value.Compare(v, &c.Values[0])

	switch c.Op {
	case SortEQ:
		return n == 0
	case SortLT:
		return n < 0
	case SortLE:
		return n <= 0
	case SortGT:
		return n > 0
	case SortGE:
		return n >= 0
	case SortBetween:
		return len(c.Values) == betweenOperands && n >= 0 && value.Compare(v, &c.Values[1]) <= 0
	case SortBeginsWith:
		return hasPrefix(v, &c.Values[0])
	default:
		return true
	}
}

func hasPrefix(v, prefix *api.AttributeValue) bool {
	switch {
	case v.S != nil && prefix.S != nil:
		return strings.HasPrefix(*v.S, *prefix.S)
	case v.B != nil && prefix.B != nil:
		return len(v.B) >= len(prefix.B) && string(v.B[:len(prefix.B)]) == string(prefix.B)
	default:
		return false
	}
}

// primaryKey returns the encoded hash value of item's table key and its range
// value, nil for a table without one.
func (t *Table) primaryKey(item value.Item) (string, *api.AttributeValue) {
	h := item[t.key.Hash.Name]
	if !t.key.HasRange() {
		return value.KeyString(&h), nil
	}

	r := item[t.key.Range.Name]

	return value.KeyString(&h), &r
}

// tie is the encoded table key of item, which orders it within an index.
func (t *Table) tie(item value.Item) string {
	h := item[t.key.Hash.Name]
	if !t.key.HasRange() {
		return encodeKey(&h, nil)
	}

	r := item[t.key.Range.Name]

	return encodeKey(&h, &r)
}

func (t *Table) store(hash string, rng *api.AttributeValue, item value.Item) {
	size := value.ItemSize(item)

	t.primary.put(hash, entry{sort: rng, item: item})
	t.count++
	t.size += size

	for _, idx := range t.indexes {
		if ihash, isort, ok := indexKey(&idx.spec, item); ok {
			idx.data.put(ihash, entry{sort: isort, tie: t.tie(item), item: item})
			idx.count++
			idx.size += size
		}
	}
}

func (t *Table) unstore(hash string, rng *api.AttributeValue, item value.Item) {
	size := value.ItemSize(item)

	t.primary.remove(hash, rng, "")
	t.count--
	t.size -= size

	for _, idx := range t.indexes {
		if ihash, isort, ok := indexKey(&idx.spec, item); ok {
			idx.data.remove(ihash, isort, t.tie(item))
			idx.count--
			idx.size -= size
		}
	}
}

// indexKey returns the encoded index hash value and the index range value of
// item, and false when the item lacks one of them and so is not in the index.
func indexKey(spec *IndexSpec, item value.Item) (string, *api.AttributeValue, bool) {
	h, ok := item[spec.Key.Hash.Name]
	if !ok {
		return "", nil, false
	}

	if !spec.Key.HasRange() {
		return value.KeyString(&h), nil, true
	}

	r, ok := item[spec.Key.Range.Name]
	if !ok {
		return "", nil, false
	}

	return value.KeyString(&h), &r, true
}

func (t *Table) index(name string) *index {
	for _, idx := range t.indexes {
		if idx.spec.Name == name {
			return idx
		}
	}

	return nil
}
