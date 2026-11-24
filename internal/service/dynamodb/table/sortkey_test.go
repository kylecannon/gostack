package table_test

import (
	"slices"
	"strconv"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b(v string) api.AttributeValue { return api.AttributeValue{B: []byte(v)} }

// sortSamples are range key values of each scalar type, in no order.
var sortSamples = map[string][]api.AttributeValue{
	"N": {
		n("0"), n("1"), n("-1"), n("2.5"), n("-2.5"), n("10"), n("-10"), n("9"), n("100"), n("0.001"), n("-0.001"),
		n("12"), n("123"), n("1234"), n("-12"), n("-123"), n("1E+125"), n("-1E+125"), n("1E-130"),
		n("12345678901234567890123456789012345678"), n("-12345678901234567890123456789012345678"),
		n("99999999999999999999999999999999999999"), n("0.12345678901234567890123456789012345678"),
	},
	"S": {s("a"), s("ab"), s("abc"), s("abd"), s("b"), s("B"), s("é"), s("a\x00"), s("\x01"), s("zz")},
	"B": {b("a"), b("ab"), b("abc"), b("b"), b("\x00"), b("\x00\xff"), b("\xff"), b("\xff\xff")},
}

// sortIndex is the index the sort tables carry.
const sortIndex = "bySk"

// The ops a key condition puts on a range key.
var sortOps = []table.SortOp{
	table.SortEQ, table.SortLT, table.SortLE, table.SortGT, table.SortGE, table.SortBetween, table.SortBeginsWith,
}

// sortTable is a table keyed by pk (S) and sk (of type typ), and a global index
// keyed by g (S) and the same sk, holding one item for each of samples under
// pk "p" and g "g". The samples are validated, so their numbers are canonical.
func sortTable(t *testing.T, typ string, samples []api.AttributeValue) *table.Table {
	t.Helper()

	key := table.KeySchema{
		Hash:  table.KeyAttribute{Name: "pk", Type: "S"},
		Range: table.KeyAttribute{Name: "sk", Type: typ},
	}
	tbl := table.New(key, []table.IndexSpec{{
		Name:       sortIndex,
		Global:     true,
		Key:        table.KeySchema{Hash: table.KeyAttribute{Name: "g", Type: "S"}, Range: key.Range},
		Projection: table.Projection{Type: table.ProjectKeysOnly},
	}})

	for i, sk := range samples {
		put(t, tbl, value.Item{"pk": s("p"), "sk": sk, "g": s("g"), "i": n(strconv.Itoa(i))})
	}

	return tbl
}

// sortOrder is samples in the order of value.Compare.
func sortOrder(samples []api.AttributeValue) []api.AttributeValue {
	want := slices.Clone(samples)
	slices.SortFunc(want, func(x, y api.AttributeValue) int { return value.Compare(&x, &y) })

	return want
}

// sortStart is the exclusive start key at range key value sk, in the table
// or, for index bySk, in the index.
func sortStart(index string, sk api.AttributeValue) value.Item {
	if index == "" {
		return value.Item{"pk": s("p"), "sk": sk}
	}

	return value.Item{"pk": s("p"), "sk": sk, "g": s("g")}
}

// sortHash is the partition the samples are under in the table or the index.
func sortHash(index string) api.AttributeValue {
	if index == "" {
		return s("p")
	}

	return s("g")
}

func reversed(values []api.AttributeValue) []api.AttributeValue {
	out := slices.Clone(values)
	slices.Reverse(out)

	return out
}

// rangeKeys reads the range key values of a Range.
func rangeKeys(
	t *testing.T, tbl *table.Table, index string, cond table.SortCondition, start value.Item, forward bool,
) []api.AttributeValue {
	t.Helper()

	got := []api.AttributeValue{}
	hash := sortHash(index)

	require.NoError(t, tbl.Range(index, &hash, cond, start, forward, func(packed value.Packed) bool {
		item := value.Unpack(packed)

		got = append(got, item["sk"])

		return true
	}))

	return got
}

// Test_Table_Range_sortKeyOrder holds the table, and an index, to the order of
// value.Compare over range keys of each type, read forward and backward.
func Test_Table_Range_sortKeyOrder(t *testing.T) {
	for typ, samples := range sortSamples {
		tbl := sortTable(t, typ, samples)
		want := sortOrder(samples)

		for _, index := range []string{"", sortIndex} {
			assert.Equal(t, want, rangeKeys(t, tbl, index, table.SortCondition{}, nil, true), "%s %q forward", typ, index)
			assert.Equal(t, reversed(want), rangeKeys(t, tbl, index, table.SortCondition{}, nil, false),
				"%s %q backward", typ, index)
		}
	}
}

// Test_Table_Range_sortKeyStart holds a read from an exclusive start key at
// each stored value to what follows it, in either direction.
func Test_Table_Range_sortKeyStart(t *testing.T) {
	for typ, samples := range sortSamples {
		tbl := sortTable(t, typ, samples)
		want := sortOrder(samples)

		for _, index := range []string{"", sortIndex} {
			for i, from := range want {
				start := sortStart(index, from)

				assert.Equal(t, want[i+1:], rangeKeys(t, tbl, index, table.SortCondition{}, start, true),
					"%s %q after %+v", typ, index, from)
				assert.Equal(t, reversed(want[:i]), rangeKeys(t, tbl, index, table.SortCondition{}, start, false),
					"%s %q before %+v", typ, index, from)
			}
		}
	}
}

// sortCondition is the condition op on the i'th of values, with the value three
// after it as the upper bound of a BETWEEN.
func sortCondition(op table.SortOp, values []api.AttributeValue, i int) table.SortCondition {
	cond := table.SortCondition{Op: op, Values: []api.AttributeValue{values[i]}}
	if op == table.SortBetween {
		cond.Values = append(cond.Values, values[(i+3)%len(values)])
	}

	return cond
}

// meeting is the values that meet cond, in order.
func meeting(cond table.SortCondition, values []api.AttributeValue) []api.AttributeValue {
	out := []api.AttributeValue{}

	for _, v := range values {
		if cond.Meets(&v) {
			out = append(out, v)
		}
	}

	return out
}

// Test_Table_Range_sortKeyCondition holds every range key condition on every
// stored value to SortCondition.Meets, which compares the values themselves.
func Test_Table_Range_sortKeyCondition(t *testing.T) {
	for typ, samples := range sortSamples {
		tbl := sortTable(t, typ, samples)
		want := sortOrder(samples)

		for _, index := range []string{"", sortIndex} {
			for _, op := range sortOps {
				if op == table.SortBeginsWith && typ == "N" {
					continue // begins_with takes a string or a binary
				}

				for i := range want {
					cond := sortCondition(op, want, i)

					assert.Equal(t, meeting(cond, want), rangeKeys(t, tbl, index, cond, nil, true),
						"%s %q op %d on %+v", typ, index, op, cond.Values)
				}
			}
		}
	}
}
