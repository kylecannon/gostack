package expression

import (
	"slices"
	"strings"
	"unicode/utf16"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// decimalBase is the base DynamoDB numbers are written in.
const decimalBase = 10

// The ReturnValues of UpdateItem.
const (
	ReturnNone       = "NONE"
	ReturnAllOld     = "ALL_OLD"
	ReturnUpdatedOld = "UPDATED_OLD"
	ReturnAllNew     = "ALL_NEW"
	ReturnUpdatedNew = "UPDATED_NEW"
)

// ReturnValues returns the Attributes UpdateItem answers for mode, given the
// paths the update wrote and the item before and after it; nil means no
// Attributes. ALL_OLD and ALL_NEW return the whole item. UPDATED_OLD and
// UPDATED_NEW return, for each path that is there in the old or the new item,
// the whole top-level attribute it starts at: DynamoDB Local answers
// SET a.b = :v with all of a, not a map holding only b, and leaves out a path
// that no longer names an element, such as a list index past the end.
func ReturnValues(mode string, paths []Path, old, updated value.Item) value.Item {
	var item value.Item

	switch mode {
	case ReturnAllOld:
		item = old
	case ReturnAllNew:
		item = updated
	case ReturnUpdatedOld:
		item = updatedAttributes(paths, old)
	case ReturnUpdatedNew:
		item = updatedAttributes(paths, updated)
	default:
	}

	if len(item) == 0 {
		return nil
	}

	return item
}

func updatedAttributes(paths []Path, item value.Item) value.Item {
	var out value.Item

	for _, path := range paths {
		if _, ok := updateGet(item, path); !ok {
			continue
		}

		if out == nil {
			out = make(value.Item, len(paths))
		}

		out[path[0].Name] = item[path[0].Name]
	}

	return out
}

// CheckUnused refuses an ExpressionAttributeNames or ExpressionAttributeValues
// entry that is not in used, the placeholders every expression of the request
// used, with DynamoDB's message: names before values, all the unused keys of
// one kind in the order DynamoDB Local's Java hash set lists them.
func CheckUnused(subs Substitutions, used map[string]struct{}) error {
	for _, check := range []struct {
		kind string
		keys []string
	}{
		{"ExpressionAttributeNames", unusedKeys(subs.Names, used)},
		{"ExpressionAttributeValues", unusedKeys(subs.Values, used)},
	} {
		if len(check.keys) > 0 {
			return value.Invalid("Value provided in " + check.kind + " unused in expressions: keys: {" +
				strings.Join(JavaHashOrder(check.keys), ", ") + "}")
		}
	}

	return nil
}

func unusedKeys[V string | api.AttributeValue](m map[string]V, used map[string]struct{}) []string {
	var keys []string

	for k := range m {
		if _, ok := used[k]; !ok {
			keys = append(keys, k)
		}
	}

	return keys
}

// JavaHashOrder sorts keys into the order a java.util.HashSet of them
// iterates: by bucket of the spread String.hashCode in a table of the default
// size, doubled while the set fills it past three quarters. Keys in one bucket
// keep their insertion order in Java, which the request does not carry, so
// they are sorted by text.
func JavaHashOrder(keys []string) []string {
	const (
		initialBuckets = 16
		loadNum        = 3
		loadDen        = 4
	)

	buckets := initialBuckets
	for len(keys)*loadDen > buckets*loadNum {
		buckets *= 2
	}

	bucket := func(s string) int {
		var h int32
		for _, r := range utf16.Encode([]rune(s)) {
			h = h*31 + int32(r) //nolint:mnd // Java's String.hashCode multiplier over a rune
		}

		spread := uint32(h) ^ uint32(h)>>16 //nolint:mnd // HashMap.hash folds the high half down

		return int(spread) & (buckets - 1)
	}

	sorted := slices.Clone(keys)
	slices.SortFunc(sorted, func(a, b string) int {
		if c := bucket(a) - bucket(b); c != 0 {
			return c
		}

		return strings.Compare(a, b)
	})

	return sorted
}
