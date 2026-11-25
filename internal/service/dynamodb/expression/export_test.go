package expression

import (
	"maps"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Apply is the update of old, an item already decoded, written into a copy
// of its top-level map: the work of an update once ApplyStored has unpacked
// the stored item, for BenchmarkUpdateApply, whose rows main measured with an
// Apply of its own that copied the map the same way.
func (u *UpdateExpr) Apply(key, old value.Item) (value.Item, error) {
	err := u.checkKey(key)
	if err != nil {
		return nil, err
	}

	item, _, err := u.applyTo(key, maps.Clone(old))

	return item, err
}

// ApplyStoredSpliced is ApplyStoredPacked splicing every update into a
// stored item, however much of it the update writes, for the property that a
// splice packs what unpacking, applying and packing does.
func ApplyStoredSpliced(
	key value.Item, stored value.Packed, read func(old value.Item) (*UpdateExpr, error),
) (value.Item, value.Packed, error) {
	return applySplicing(key, stored, read, spliceAlways)
}

// SpliceCosts is the sizes ApplyStoredSpliced settles a splice on: old, the
// size of the old values of the attributes the update writes, and cost, that
// and the size of their new values. Both are 0 when it settles nothing.
func SpliceCosts(
	key value.Item, stored value.Packed, read func(old value.Item) (*UpdateExpr, error),
) (old, cost int, err error) {
	var costs []int

	_, _, err = applySplicing(key, stored, read, func(cost, _ int) bool {
		costs = append(costs, cost)

		return true
	})
	if len(costs) < 2 {
		return 0, 0, err
	}

	return costs[len(costs)-2], costs[len(costs)-1], err
}

// AttrsSize is the size, as ItemSize counts it, of the attributes of item that
// names lists.
func AttrsSize(item value.Item, names []string) int {
	size := 0

	for _, name := range names {
		if v, ok := item[name]; ok {
			size += len(name) + value.Size(&v)
		}
	}

	return size
}

// SpliceChecked is spliceInto: item, the update of stored that wrote names,
// packed, or "" when the splice does not hold as many attributes as item. A
// test hands it names the update did not write.
func SpliceChecked(stored value.Packed, names []string, item value.Item) (value.Packed, error) {
	return spliceInto(stored, names, item)
}

// WrittenNames is the top-level attributes the update writes, as the splice is
// handed them.
func (u *UpdateExpr) WrittenNames() []string {
	return u.written()
}

// spliceAlways says every splice pays.
func spliceAlways(int, int) bool { return true }

// Clash is the pair findPathClash names for the tests of this package: One is
// the path being added, Two the path it names second.
type Clash struct {
	One, Two Path
	Conflict bool
}

// PathsClash is findPathClash for the tests of this package, nil when the paths
// do not clash.
func PathsClash(paths []Path) *Clash {
	c := findPathClash(paths)
	if c == nil {
		return nil
	}

	return &Clash{One: c.one, Two: c.two, Conflict: c.conflict}
}
