package expression_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// updateApply parses expr and applies it to old under the test key, checking
// old is unchanged.
func updateApply(t *testing.T, old value.Item, expr string, subs expression.Substitutions) (
	*expression.UpdateExpr, value.Item, error,
) {
	t.Helper()

	u, err := expression.ParseUpdate(expr, subs, nil)
	require.NoError(t, err)

	before := updateJSON(t, old)

	updated, applyErr := u.Apply(updateItem(t, updateKey), old)

	require.Equal(t, before, updateJSON(t, old), "Apply changed the old item")

	return u, updated, applyErr //nolint:wrapcheck // the test checks the error itself
}

// Test_UpdateExpr_Apply_CanonicalValues: a value placeholder is read in
// canonical form, as the image reads it (SQ-268): ADD of "1.0" to [1, 2, 3.5]
// adds nothing, and DELETE of "1.0" removes 1. The substitutions are decoded
// without validation, as the request carries them.
func Test_UpdateExpr_Apply_CanonicalValues(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		expr string
		want []string
	}{
		{"ADD ns :x", []string{"1", "2", "3.5"}},
		{"DELETE ns :x", []string{"2", "3.5"}},
	} {
		t.Run(tt.expr, func(t *testing.T) {
			t.Parallel()

			var values value.Item

			require.NoError(t, json.Unmarshal([]byte(`{":x":{"NS":["1.0"]}}`), &values))

			old := updateItem(t, `{"pk":{"S":"k"},"ns":{"NS":["1","2","3.5"]}}`)
			_, updated, err := updateApply(t, old, tt.expr, expression.Substitutions{Values: values})
			require.NoError(t, err)

			ns := updated["ns"]
			require.ElementsMatch(t, tt.want, ns.NS)
		})
	}
}

// Each row's new item and UPDATED_OLD and UPDATED_NEW Attributes are what
// DynamoDB Local in the LocalStack image answered for the same update, except
// where a row says otherwise.
func Test_UpdateExpr_Apply(t *testing.T) {
	t.Parallel()

	const k = `"pk":{"S":"k"},`

	tests := []struct {
		name       string
		giveItem   string
		giveExpr   string
		giveValues string
		wantItem   string
		wantOld    string
		wantNew    string
	}{
		{
			"right-hand sides read the old item", `{` + k + `"a":{"N":"1"},"b":{"N":"2"}}`, "SET a = b, b = a", "",
			`{` + k + `"a":{"N":"2"},"b":{"N":"1"}}`, `{"a":{"N":"1"},"b":{"N":"2"}}`, `{"a":{"N":"2"},"b":{"N":"1"}}`,
		},
		{
			"nested set returns the whole attribute",
			`{` + k + `"a":{"M":{"b":{"S":"1"},"c":{"S":"2"}}},"d":{"S":"3"}}`, "SET a.b = :v, d = :v", updateX,
			`{` + k + `"a":{"M":{"b":{"S":"x"},"c":{"S":"2"}}},"d":{"S":"x"}}`,
			`{"a":{"M":{"b":{"S":"1"},"c":{"S":"2"}}},"d":{"S":"3"}}`,
			`{"a":{"M":{"b":{"S":"x"},"c":{"S":"2"}}},"d":{"S":"x"}}`,
		},
		{
			"set past the end appends", `{` + k + `"a":{"L":[{"S":"0"},{"S":"1"}]}}`, "SET a[5] = :v", updateX,
			`{` + k + `"a":{"L":[{"S":"0"},{"S":"1"},{"S":"x"}]}}`, "", "",
		},
		{
			"appends go in index order", `{` + k + `"a":{"L":[{"S":"0"}]}}`, "SET a[7] = :v, a[5] = :w",
			updateXY, `{` + k + `"a":{"L":[{"S":"0"},{"S":"y"},{"S":"x"}]}}`, "", "",
		},
		{
			"set list elements", `{` + k + `"a":{"L":[{"S":"0"},{"S":"1"},{"S":"2"}]}}`, "SET a[1] = :v, a[2] = :w",
			updateXY, `{` + k + `"a":{"L":[{"S":"0"},{"S":"x"},{"S":"y"}]}}`,
			updateList012, `{"a":{"L":[{"S":"0"},{"S":"x"},{"S":"y"}]}}`,
		},
		{
			"remove list elements of the old list", `{` + k + `"a":{"L":[{"S":"0"},{"S":"1"},{"S":"2"},{"S":"3"}]}}`,
			"REMOVE a[1], a[2]", "", `{` + k + `"a":{"L":[{"S":"0"},{"S":"3"}]}}`,
			`{"a":{"L":[{"S":"0"},{"S":"1"},{"S":"2"},{"S":"3"}]}}`, `{"a":{"L":[{"S":"0"},{"S":"3"}]}}`,
		},
		{
			"remove in any order", `{` + k + `"a":{"L":[{"S":"0"},{"S":"1"},{"S":"2"}]}}`, "REMOVE a[2], a[0]", "",
			`{` + k + `"a":{"L":[{"S":"1"}]}}`, updateList012, `{"a":{"L":[{"S":"1"}]}}`,
		},
		{
			"set acts before remove", `{` + k + `"a":{"L":[{"S":"0"},{"S":"1"},{"S":"2"}]}}`,
			"REMOVE a[0] SET a[1] = :v", updateX, `{` + k + `"a":{"L":[{"S":"x"},{"S":"2"}]}}`,
			updateList012, `{"a":{"L":[{"S":"x"},{"S":"2"}]}}`,
		},
		{
			"append and remove", `{` + k + `"a":{"L":[{"S":"0"},{"S":"1"}]}}`, "SET a[5] = :v, a[3] = :w REMOVE a[0]",
			updateXY, `{` + k + `"a":{"L":[{"S":"1"},{"S":"y"},{"S":"x"}]}}`,
			`{"a":{"L":[{"S":"0"},{"S":"1"}]}}`, `{"a":{"L":[{"S":"1"},{"S":"y"},{"S":"x"}]}}`,
		},
		{
			"add acts before remove", `{` + k + `"a":{"L":[{"N":"1"},{"N":"2"},{"N":"3"}]}}`, "ADD a[1] :n REMOVE a[0]",
			`{":n":{"N":"10"}}`, `{` + k + `"a":{"L":[{"N":"12"},{"N":"3"}]}}`,
			`{"a":{"L":[{"N":"1"},{"N":"2"},{"N":"3"}]}}`, `{"a":{"L":[{"N":"12"},{"N":"3"}]}}`,
		},
		{
			"remove past the end", `{` + k + `"a":{"L":[{"S":"0"}]}}`, "REMOVE a[4]", "",
			`{` + k + `"a":{"L":[{"S":"0"}]}}`, "", "",
		},
		{"remove missing", `{` + k + `"a":{"S":"0"}}`, "REMOVE b", "", `{` + k + `"a":{"S":"0"}}`, "", ""},
		{
			"remove nested", `{` + k + `"a":{"M":{"b":{"S":"1"},"c":{"S":"2"}}}}`, "REMOVE a.b", "",
			`{` + k + `"a":{"M":{"c":{"S":"2"}}}}`, `{"a":{"M":{"b":{"S":"1"},"c":{"S":"2"}}}}`, "",
		},
		{"remove on no item", "", "REMOVE b", "", `{` + k[:len(k)-1] + `}`, "", ""},
		{
			"add a new number", `{` + k[:len(k)-1] + `}`, updateAddN, `{":v":{"N":"3"}}`,
			`{` + k + `"n":{"N":"3"}}`, "", `{"n":{"N":"3"}}`,
		},
		{
			"add to a number", `{` + k + `"n":{"N":"2"}}`, updateAddN, `{":v":{"N":"-3.5"}}`,
			`{` + k + `"n":{"N":"-1.5"}}`, `{"n":{"N":"2"}}`, `{"n":{"N":"-1.5"}}`,
		},
		{
			"add to a set", `{` + k + `"s":{"SS":["a","b"]}}`, "ADD s :v", `{":v":{"SS":["b","c"]}}`,
			`{` + k + `"s":{"SS":["a","b","c"]}}`, `{"s":{"SS":["a","b"]}}`, `{"s":{"SS":["a","b","c"]}}`,
		},
		{
			"add nested", `{` + k + `"a":{"M":{}}}`, "ADD a.n :v", updateV1,
			`{` + k + `"a":{"M":{"n":{"N":"1"}}}}`, "", `{"a":{"M":{"n":{"N":"1"}}}}`,
		},
		{
			"delete from a set", `{` + k + `"s":{"SS":["a","b"]}}`, updateDeleteS, `{":v":{"SS":["b","c"]}}`,
			`{` + k + `"s":{"SS":["a"]}}`, `{"s":{"SS":["a","b"]}}`, updateSA,
		},
		{
			"delete empties the set", `{` + k + `"s":{"NS":["1","2"]}}`, updateDeleteS, `{":v":{"NS":["2","1"]}}`,
			`{` + k[:len(k)-1] + `}`, `{"s":{"NS":["1","2"]}}`, "",
		},
		{
			"delete missing", `{` + k + `"b":{"S":"x"}}`, updateDeleteS, `{":v":{"SS":["a"]}}`,
			`{` + k + `"b":{"S":"x"}}`, "", "",
		},
		{
			"if_not_exists", `{` + k + `"a":{"S":"old"}}`, "SET a = if_not_exists(a, :v), b = if_not_exists(b, :v)",
			updateX, `{` + k + `"a":{"S":"old"},"b":{"S":"x"}}`, `{"a":{"S":"old"}}`, `{"a":{"S":"old"},"b":{"S":"x"}}`,
		},
		{
			"list_append", `{` + k + `"b":{"L":[{"S":"x"}]}}`,
			"SET a = list_append(b, :w), c = list_append(if_not_exists(c, :e), :w)",
			`{":w":{"L":[{"S":"y"}]},":e":{"L":[]}}`,
			`{` + k + `"a":{"L":[{"S":"x"},{"S":"y"}]},"b":{"L":[{"S":"x"}]},"c":{"L":[{"S":"y"}]}}`, "",
			`{"a":{"L":[{"S":"x"},{"S":"y"}]},"c":{"L":[{"S":"y"}]}}`,
		},
		{
			"arithmetic", `{` + k + `"b":{"N":"5"}}`, "SET a = b + :n, c = b - :n, d = (:n + :n)", `{":n":{"N":"1.5"}}`,
			`{` + k + `"a":{"N":"6.5"},"b":{"N":"5"},"c":{"N":"3.5"},"d":{"N":"3"}}`, "",
			`{"a":{"N":"6.5"},"c":{"N":"3.5"},"d":{"N":"3"}}`,
		},
		{
			// DynamoDB Local answers 0.0 here, the scale its BigDecimal keeps;
			// gostack keeps every number canonical, as DynamoDB does.
			"arithmetic result is canonical", `{` + k + `"n":{"N":"2.5"}}`, updateAddN, `{":v":{"N":"-2.5"}}`,
			`{` + k + `"n":{"N":"0"}}`, `{"n":{"N":"2.5"}}`, `{"n":{"N":"0"}}`,
		},
		{
			"every clause", `{` + k + `"a":{"N":"1"},"r":{"S":"x"},"s":{"SS":["a"]}}`,
			"SET a = a + :one REMOVE r ADD n :one DELETE s :s", `{":one":{"N":"1"},":s":{"SS":["a"]}}`,
			`{` + k + `"a":{"N":"2"},"n":{"N":"1"}}`, `{"a":{"N":"1"},"r":{"S":"x"},"s":{"SS":["a"]}}`,
			`{"a":{"N":"2"},"n":{"N":"1"}}`,
		},
		{
			"list in a list", `{` + k + `"a":{"L":[{"L":[{"S":"0"}]}]}}`, "SET a[0][0] = :v", updateX,
			`{` + k + `"a":{"L":[{"L":[{"S":"x"}]}]}}`, `{"a":{"L":[{"L":[{"S":"0"}]}]}}`, `{"a":{"L":[{"L":[{"S":"x"}]}]}}`,
		},
		{
			"unchanged value is still updated", `{` + k + `"a":{"S":"x"}}`, "SET a = :v", updateX,
			`{` + k + `"a":{"S":"x"}}`, updateAX, updateAX,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			old := updateItem(t, tt.giveItem)
			u, updated, err := updateApply(t, old, tt.giveExpr, updateSubs(t, "", tt.giveValues))
			require.NoError(t, err)
			require.JSONEq(t, tt.wantItem, updateJSON(t, updated))

			for _, rv := range []struct{ mode, want string }{
				{expression.ReturnUpdatedOld, tt.wantOld},
				{expression.ReturnUpdatedNew, tt.wantNew},
				{expression.ReturnAllOld, updateJSON(t, old)},
				{expression.ReturnAllNew, tt.wantItem},
				{expression.ReturnNone, ""},
			} {
				got := updateJSON(t, expression.ReturnValues(rv.mode, u.Paths(), old, updated))
				if rv.want == "" {
					require.Empty(t, got, rv.mode)
				} else {
					require.JSONEq(t, rv.want, got, rv.mode)
				}
			}
		})
	}
}

// Every message below was observed from DynamoDB Local in the LocalStack image;
// rows that hold two problems pin which one DynamoDB reports first.
func Test_UpdateExpr_Apply_Errors(t *testing.T) {
	t.Parallel()

	const (
		key      = "One or more parameter values were invalid: Cannot update attribute pk. This attribute is part of the key"
		path     = "The document path provided in the update expression is invalid for update"
		missing  = "The provided expression refers to an attribute that does not exist in the item"
		badType  = "An operand in the update expression has an incorrect data type"
		precise  = "DynamoDB only supports precision up to 38 digits"
		listS    = `{"x":{"S":"s"}}`
		listNums = `{":n":{"N":"1"},":l":{"L":[]}}`
	)

	big := `{":v":{"S":"` + strings.Repeat("x", 210_000) + `"}}`

	tests := []struct {
		name       string
		giveItem   string
		giveExpr   string
		giveNames  string
		giveValues string
		wantMsg    string
	}{
		{"set key", "", "SET pk = :v", "", updateX, key},
		{"remove inside key", "", "REMOVE pk[0]", "", "", key},
		{"key through a name", "", "SET #n = :v", `{"#n":"pk"}`, updateX, key},
		{"key before operands", "", "SET a = b + :n, pk = :n", "", updateN, key},
		{"set under missing parent", "", "SET a.b = :v", "", updateX, path},
		{"set under a string", updateAQ, "SET a.b = :v", "", updateX, path},
		{"index into a map", `{"a":{"M":{}}}`, "SET a[0] = :v", "", updateX, path},
		{"remove under missing parent", `{"a":{"S":"0"}}`, "REMOVE b.c", "", "", path},
		{"remove index of a string", `{"a":{"S":"0"}}`, "REMOVE a[0]", "", "", path},
		{"add under missing parent", "", "ADD a.n :v", "", updateV1, path},
		{"list_append a string", `{"b":{"S":"x"}}`, "SET a = list_append(b, :l)", "", `{":l":{"L":[]}}`, badType},
		{"list_append missing", "", "SET a = list_append(b, :l)", "", `{":l":{"L":[]}}`, missing},
		{"plus a string", `{"b":{"S":"x"}}`, "SET a = b + :n", "", updateN, badType},
		{"plus missing", "", "SET a = b + :n", "", updateN, missing},
		{"index past the end", `{"l":{"L":[{"S":"x"}]}}`, "SET a = l[0], b = l[9]", "", "", missing},
		{"missing nested", `{"b":{"M":{}}}`, "SET a = b.c", "", "", missing},
		{"if_not_exists of two missing", "", "SET a = if_not_exists(a, b)", "", "", missing},
		{"if_not_exists reads its default", updateAQ, "SET c = if_not_exists(a, b)", "", "", missing},
		{"missing before type", listS, "SET a = list_append(x, :l), c = z + :n", "", listNums, missing},
		{"type before path", listS, "SET q.r = :n, a = list_append(x, :l)", "", listNums, badType},
		{
			"if_not_exists result type", updateAQ, "SET a = if_not_exists(a, :s) + :n", "",
			`{":s":{"S":"x"},":n":{"N":"1"}}`, badType,
		},
		{"add number set to string set", updateSA, updateAddS, "", `{":v":{"NS":["1"]}}`, badType},
		{"add number to string", `{"s":{"S":"a"}}`, updateAddS, "", updateV1, badType},
		{"delete number set from string set", updateSA, updateDeleteS, "", `{":v":{"NS":["1"]}}`, badType},
		{"delete from a string", `{"s":{"S":"a"}}`, updateDeleteS, "", `{":v":{"SS":["a"]}}`, badType},
		{
			"sum past 38 digits", "", "SET a = :x + :y", "",
			`{":x":{"N":"10000000000000000000000000000000000000"},":y":{"N":"0.1"}}`, precise,
		},
		{
			"sum past the range", "", "SET a = :x + :x", "", `{":x":{"N":"9E+125"}}`,
			"Number overflow. Attempting to store a number with magnitude larger than supported range",
		},
		{
			"item too large", updateAX, "SET b = :v, c = :v", "", big,
			"Item size to update has exceeded the maximum allowed size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := updateApply(t, updateItem(t, tt.giveItem), tt.giveExpr, updateSubs(t, tt.giveNames, tt.giveValues))
			requireUpdateError(t, err, tt.wantMsg)
		})
	}
}
