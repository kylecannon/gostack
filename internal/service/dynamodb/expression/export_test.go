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

	return u.applyTo(key, maps.Clone(old))
}

// ApplyStoredSpliced is ApplyStoredPacked splicing every update into a
// stored item, however much of it the update writes, for the property that a
// splice packs what unpacking, applying and packing does.
func ApplyStoredSpliced(
	key value.Item, stored value.Packed, read func(old value.Item) (*UpdateExpr, error),
) (value.Item, value.Packed, error) {
	return applySplicing(key, stored, read, func(int, int) bool { return true })
}

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
