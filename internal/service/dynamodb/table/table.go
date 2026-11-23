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
	count   int
	size    int

	// indexes is copy-on-write. AddIndex and DropIndex publish a new slice
	// while holding mu for writing; a published slice, and the spec of each
	// index in it, never changes. CheckItem reads the list without mu, so a
	// put's check does not contend with writes. Each index's data and counts
	// stay guarded by mu.
	indexes atomic.Pointer[[]*index]

	// observer is told of every write that changes what is stored, under mu,
	// so it sees the writes in the order they applied. Nil when no one
	// listens, which costs a write nothing.
	observer Observer
}

// Observer is told of a write that changed what is stored, under the table's
// write lock, in the order the writes applied. It must not call back into the
// table and must not take a lock that is held while the table's is taken.
type Observer func(Change)

// Observe makes o the table's observer, nil to have none.
func (t *Table) Observe(o Observer) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.observer = o
}

// index is one secondary index: its spec, its own partitions and its counts.
type index struct {
	spec  IndexSpec
	data  store
	count int
	size  int
}

// Change is what a write did: the item stored before and the item stored
// after, either nil when there was none. OldPacked and NewPacked are the same
// two items as the table stores them, "" when there was none. An observer that
// keeps an item keeps these: they are immutable strings that hold no other
// item's bytes, where Old and New can share the bytes of the item they were
// decoded from or built over, and so pin it.
type Change struct {
	Old       value.Item
	New       value.Item
	OldPacked value.Packed
	NewPacked value.Packed
	// Expired marks a removal made through Expire: the service deleting an
	// item whose time to live passed, not a caller.
	Expired bool
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

	list := make([]*index, 0, len(indexes))
	for _, spec := range indexes {
		list = append(list, &index{spec: spec, data: newStore()})
	}

	t.indexes.Store(&list)

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
	return value.Unpack(t.GetPacked(key))
}

// GetPacked returns the item with key as it is stored, or "". key must have
// passed CheckKey.
func (t *Table) GetPacked(key value.Item) value.Packed {
	t.mu.RLock()
	defer t.mu.RUnlock()

	hash, rng := t.primaryKey(key)

	return t.primary.get(hash, rng, "")
}

// IndexesHeld returns the specs of the table's indexes. A caller that holds
// the table's lock through Lock or ReadLock gets the indexes its writes will
// maintain; it needs no lock to call it.
func (t *Table) IndexesHeld() []IndexSpec {
	list := t.indexList()

	specs := make([]IndexSpec, len(list))
	for i, idx := range list {
		specs[i] = idx.spec
	}

	return specs
}

// GetHeld is Get for a caller that holds the table's lock through Lock or
// ReadLock, as a transaction does.
func (t *Table) GetHeld(key value.Item) value.Item {
	return value.Unpack(t.GetHeldPacked(key))
}

// GetHeldPacked is GetHeld for a caller that takes the item as stored.
func (t *Table) GetHeldPacked(key value.Item) value.Packed {
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
	return t.apply(key, decide, false)
}

// Expire is Write for the service's own removal of an expired item: decide
// returns nil to remove it, and the observer sees a Change marked Expired.
func (t *Table) Expire(key value.Item, decide func(old value.Item) (value.Item, error)) (Change, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.apply(key, decide, true)
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
	names := keyNames(t.key, []IndexSpec{spec})

	for _, hash := range t.primary.sortedKeys() {
		for _, e := range t.primary.parts[hash].entries {
			keys := e.item.Attrs(names)
			if ihash, isort, ok := indexKey(&spec, keys); ok {
				idx.data.put(ihash, entry{sort: isort, tie: t.tie(keys), item: e.item})
				idx.count++
				idx.size += e.item.Size()
			}
		}
	}

	list := slices.Concat(t.indexList(), []*index{idx})
	t.indexes.Store(&list)
}

// DropIndex removes the named secondary index, and reports whether there was
// one.
func (t *Table) DropIndex(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	old := t.indexList()

	// DeleteFunc works in place, so it runs on a copy: CheckItem may be
	// reading the published slice.
	list := slices.DeleteFunc(slices.Clone(old), func(idx *index) bool { return idx.spec.Name == name })
	t.indexes.Store(&list)

	return len(list) < len(old)
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
	visit func(value.Packed) bool,
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
func visitRange(entries []entry, lo, hi int, forward bool, visit func(value.Packed) bool) {
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
func (t *Table) Scan(indexName string, start value.Item, visit func(hash string, item value.Packed) bool) error {
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

	first, startSort, startTie := 0, "", ""

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
			if !visit(hash, e.item) {
				return nil
			}
		}
	}

	return nil
}

// apply is Write's body: the caller holds mu for writing.
func (t *Table) apply(key value.Item, decide func(old value.Item) (value.Item, error), expired bool) (Change, error) {
	hash, rng := t.primaryKey(key)
	stored := t.primary.get(hash, rng, "")
	old := value.Unpack(stored)

	updated, err := decide(old)
	if err != nil {
		return Change{Old: old}, err
	}

	// The item is packed once, here, and its entries take their keys from the
	// packed item, whose bytes they share, and not from key or updated, which
	// are the request's.
	var packed value.Packed

	if updated != nil {
		err := t.CheckItem(updated)
		if err != nil {
			return Change{Old: old}, err
		}

		h, s := t.primaryKey(updated)
		if h != hash || (s != "" && s != rng) {
			return Change{Old: old}, errKeyChanged
		}

		packed, err = value.Pack(updated)
		if err != nil {
			return Change{Old: old}, err //nolint:wrapcheck // the encoder's own error
		}
	}

	if old != nil {
		t.unstore(hash, rng, old, stored.Size())
	}

	if packed != "" {
		t.store(hash, packed)
	}

	change := Change{Old: old, New: updated, OldPacked: stored, NewPacked: packed, Expired: expired}
	if t.observer != nil && change.Changed() {
		t.observer(change)
	}

	return change, nil
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
func (t *Table) position(indexName string, key KeySchema, start value.Item) (string, string) {
	sortValue := sortKey(key, start)

	if indexName == "" {
		return sortValue, ""
	}

	return sortValue, t.tie(start)
}

// bounds returns the positions [lo, hi) of the entries whose range value meets
// cond.
func bounds(p *partition, cond SortCondition) (int, int) {
	n := len(p.entries)
	if cond.Op == SortAny || len(cond.Values) == 0 || n == 0 || p.entries[0].sort == "" {
		return 0, n
	}

	v := value.SortKeyUntyped(&cond.Values[0])
	firstGE := func(x string) int {
		return sort.Search(n, func(i int) bool { return p.entries[i].sort >= x })
	}
	firstGT := func(x string) int {
		return sort.Search(n, func(i int) bool { return p.entries[i].sort > x })
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

		return firstGE(v), max(firstGE(v), firstGT(value.SortKeyUntyped(&cond.Values[1])))
	case SortBeginsWith:
		lo := firstGE(v)
		hi := lo

		// A number has no prefix: begins_with takes a string or a binary.
		for hi < n && cond.Values[0].N == nil && strings.HasPrefix(p.entries[hi].sort, v) {
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

// primaryKey returns the encoded hash value of item's table key and its
// encoded range value, empty for a table without one.
func (t *Table) primaryKey(item value.Item) (string, string) {
	h := item[t.key.Hash.Name]

	return value.KeyString(&h), sortKey(t.key, item)
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

// store puts item, packed, in the table's partition hash and in each index it
// is in. Its keys are read from item, so they share its bytes.
func (t *Table) store(hash string, item value.Packed) {
	size := item.Size()
	indexes := t.indexList()
	keys := item.Attrs(keyNames(t.key, specsOf(indexes)))

	t.primary.put(hash, entry{sort: sortKey(t.key, keys), item: item})
	t.count++
	t.size += size

	for _, idx := range indexes {
		if ihash, isort, ok := indexKey(&idx.spec, keys); ok {
			idx.data.put(ihash, entry{sort: isort, tie: t.tie(keys), item: item})
			idx.count++
			idx.size += size
		}
	}
}

// unstore removes the entries of item, stored in partition hash at range
// value rng, whose size is size.
func (t *Table) unstore(hash, rng string, item value.Item, size int) {
	t.primary.remove(hash, rng, "")
	t.count--
	t.size -= size

	for _, idx := range t.indexList() {
		if ihash, isort, ok := indexKey(&idx.spec, item); ok {
			idx.data.remove(ihash, isort, t.tie(item))
			idx.count--
			idx.size -= size
		}
	}
}

// keyNames lists, each once, the key attributes of key and of each index of
// specs: what an entry of the table or an index reads from a stored item.
func keyNames(key KeySchema, specs []IndexSpec) []string {
	const perKey = 2 // a hash and a range attribute

	names := make([]string, 0, perKey+perKey*len(specs))

	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	add(key.Hash.Name)
	add(key.Range.Name)

	for i := range specs {
		add(specs[i].Key.Hash.Name)
		add(specs[i].Key.Range.Name)
	}

	return names
}

// specsOf returns the specs of indexes.
func specsOf(indexes []*index) []IndexSpec {
	specs := make([]IndexSpec, len(indexes))
	for i, idx := range indexes {
		specs[i] = idx.spec
	}

	return specs
}

// indexKey returns the encoded index hash value and the encoded index range
// value of item, and false when the item is not in the index (indexed).
func indexKey(spec *IndexSpec, item value.Item) (string, string, bool) {
	if !indexed(spec, item) {
		return "", "", false
	}

	h := item[spec.Key.Hash.Name]

	return value.KeyString(&h), sortKey(spec.Key, item), true
}

// indexList returns the published index list. The caller must not change it.
func (t *Table) indexList() []*index {
	if p := t.indexes.Load(); p != nil {
		return *p
	}

	return nil
}

func (t *Table) index(name string) *index {
	for _, idx := range t.indexList() {
		if idx.spec.Name == name {
			return idx
		}
	}

	return nil
}
