//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package partiql_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/stretchr/testify/require"
)

// Test_Parse_forms: every statement form parses to its kind, table, index,
// clauses and parameter count.
func Test_Parse_forms(t *testing.T) {
	cases := []struct {
		name, src    string
		kind         partiql.StatementKind
		table, index string
		params       int
		returning    partiql.Returning
	}{
		{"select star", "SELECT * FROM t", partiql.Select, "t", "", 0, partiql.ReturnNone},
		{"select index", `SELECT a, b.c, d[1] FROM "t"."idx" WHERE a = ?`, partiql.Select, "t", "idx", 1, partiql.ReturnNone},
		{
			"select where forms", "select * from t where a between 1 and 2 and b in ['x', ?] or not c is missing " +
				"and d is not null and begins_with(e, 'x') and contains(f, 1) and attribute_type(g, 'S') and size(h) > 2;",
			partiql.Select, "t", "", 1, partiql.ReturnNone,
		},
		{"select order", "SELECT * FROM t WHERE id = 'h' ORDER BY sk DESC", partiql.Select, "t", "", 0, partiql.ReturnNone},
		{"insert", "INSERT INTO t VALUE {'id': ?, 'n': 1.5e3, 'b': true, 'z': null, 'l': [1, 'x'], " +
			"'m': {'k': 'v'}, 'ss': <<'a','b'>>, 'ns': <<1,2>>}", partiql.Insert, "t", "", 1, partiql.ReturnNone},
		{"update", "UPDATE t SET a = a + 1, l = list_append(l, [3]) SET s = set_add(s, <<'z'>>) REMOVE q " +
			"WHERE id = 'a' RETURNING MODIFIED NEW *", partiql.Update, "t", "", 0, partiql.ReturnModifiedNew},
		{"delete", "DELETE FROM t WHERE id = ? RETURNING ALL OLD *", partiql.Delete, "t", "", 1, partiql.ReturnAllOld},
		{"exists", "EXISTS(SELECT * FROM t WHERE id = 'a' AND n = 1)", partiql.Exists, "t", "", 0, partiql.ReturnNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := partiql.Parse(tc.src)
			require.NoError(t, err)
			require.Equal(t, tc.kind, s.Kind)
			require.Equal(t, tc.table, s.Table)
			require.Equal(t, tc.index, s.Index)
			require.Equal(t, tc.params, s.Params)
			require.Equal(t, tc.returning, s.Returning)
		})
	}
}

// Test_Parse_errors: DynamoDB Local's message for each statement it refuses
// to parse (verification/SQ-229 probes 1 and 2).
func Test_Parse_errors(t *testing.T) {
	const malformed = "Statement wasn't well formed, can't be processed: "

	cases := []struct{ name, src, want string }{
		{"misspelled", "SELEC * FROM t", malformed + "SELEC * FROM t"},
		{"no table", "SELECT * FROM", malformed + "SELECT * FROM"},
		{"open string", "SELECT * FROM t WHERE id = 'a", malformed + "SELECT * FROM t WHERE id = 'a"},
		{"empty", "", malformed},
		{"empty where", "SELECT * FROM t WHERE", malformed + "SELECT * FROM t WHERE"},
		{"dangling and", "SELECT * FROM t WHERE id = 'a' AND", malformed + "SELECT * FROM t WHERE id = 'a' AND"},
		{"star and name", "SELECT *, id FROM t", malformed + "SELECT *, id FROM t"},
		{"lone name", "SELEC", "Unsupported operation: VariableReference"},
		{"insert returning", "INSERT INTO t VALUE {'id': 'd'} RETURNING ALL OLD *", "RETURNING is not supported in INSERT"},
		{
			"delete returning new", "DELETE FROM t WHERE id = 'b' RETURNING ALL NEW *",
			"Invalid returning clause: RETURNING ALL NEW *. Only RETURNING ALL OLD * is allowed in DELETE statements.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := partiql.Parse(tc.src)

			var pe *partiql.Error

			require.ErrorAs(t, err, &pe)
			require.Equal(t, tc.want, pe.Message)
		})
	}
}

// Test_Value_errors: the item values DynamoDB Local refuses, with their
// messages and positions (verification/SQ-229 probes 1 and 4).
func Test_Value_errors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"duplicate key", "INSERT INTO t VALUE {'id': 'e', 'id': 'f'}",
			"Duplicate keys in tuple. Key: id under root at 1:33:4",
		},
		{"empty bag", "INSERT INTO t VALUE {'id': 'j', 's': <<>>}", "Empty bags are not supported at 1:38:2"},
		{
			"mixed bag", "INSERT INTO t VALUE {'id': 'j', 's': <<1,'a'>>}",
			"Unsupported data type in Bag. DynamoDB only supports either numbers or strings in bags",
		},
		{"repeated string", "INSERT INTO t VALUE {'id': 'j', 's': <<'a','a'>>}", "Input collection contains duplicates"},
		{
			"missing", "INSERT INTO t VALUE {'id': 'k', 'm': MISSING}",
			"Unsupported data type: MISSING under key root at 1:38:7",
		},
		{
			// A token's length counts characters, as the image's does: 'é' is 3 (SQ-418).
			"key of characters", "INSERT INTO t VALUE {'é': 'e', 'é': 'f'}",
			"Duplicate keys in tuple. Key: é under root at 1:32:3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := partiql.Parse(tc.src)
			require.NoError(t, err)

			_, err = partiql.Value(s.Value, nil, "")

			var pe *partiql.Error

			require.ErrorAs(t, err, &pe)
			require.Equal(t, tc.want, pe.Message)
		})
	}
}

// Test_Parse_orderGaps: the forms the image takes that the parser once refused
// (SQ-418): a REMOVE of one path, a path step in quotes, an alias it parses and
// leaves to the caller to refuse, and a comment in a block.
func Test_Parse_orderGaps(t *testing.T) {
	s, err := partiql.Parse("UPDATE t REMOVE a REMOVE b.c SET d = 1, e = 2 WHERE id = 'a'")
	require.NoError(t, err)
	require.Len(t, s.Actions, 4)

	wantActions := []struct {
		path  partiql.Path
		value string
	}{
		{partiql.Path{{Name: "a"}}, ""},
		{partiql.Path{{Name: "b"}, {Name: "c"}}, ""},
		{partiql.Path{{Name: "d"}}, "1"},
		{partiql.Path{{Name: "e"}}, "2"},
	}
	for i, want := range wantActions {
		require.Equal(t, want.path, s.Actions[i].Path, i)

		if want.value == "" {
			require.Nil(t, s.Actions[i].Value, i)

			continue
		}

		require.Equal(t, partiql.LitNumber, s.Actions[i].Value.Literal.Kind, i)
		require.Equal(t, want.value, s.Actions[i].Value.Literal.Text, i)
	}

	for _, src := range []string{"UPDATE t REMOVE a, b WHERE id = 'a'", "UPDATE t REMOVE a, WHERE id = 'a'"} {
		_, err = partiql.Parse(src)
		require.EqualError(t, err, partiql.MsgNotWellFormed+src)
	}

	for _, step := range []string{"m['x']", `m["x"]`, "m[x]", "m.x"} {
		s, err = partiql.Parse("SELECT " + step + " FROM t")
		require.NoError(t, err, step)
		require.Equal(t, partiql.Path{{Name: "m"}, {Name: "x"}}, s.Projection[0], step)
	}

	s, err = partiql.Parse("SELECT m[1]['x'][2] FROM t")
	require.NoError(t, err)
	require.Equal(t, partiql.Path{{Name: "m"}, {Index: 1, IsIndex: true}, {Name: "x"}, {Index: 2, IsIndex: true}},
		s.Projection[0])

	// A step with no name parses: the image refuses it where the path is used.
	for _, src := range []string{"SELECT m[''] FROM t", `SELECT m[""] FROM t`} {
		s, err = partiql.Parse(src)
		require.NoError(t, err, src)
		require.True(t, s.Projection[0].HasEmptyStep(), src)
		require.Equal(t, partiql.Path{{Name: "m"}, {Name: ""}}, s.Projection[0], src)
	}

	for _, src := range []string{"SELECT m[' FROM t", "SELECT m[''  FROM t", "SELECT m['' FROM t"} {
		_, err = partiql.Parse(src)
		require.EqualError(t, err, partiql.MsgNotWellFormed+src, src)
	}

	aliases := map[string][2]bool{
		"SELECT a AS z FROM t": {true, false}, "SELECT a z, b FROM t": {true, false}, "SELECT a FROM t u": {true, true},
		"SELECT a FROM t AS u WHERE id = 1": {true, true}, "UPDATE t AS u SET a = 1": {true, true},
		"DELETE FROM t u WHERE id = 1": {true, true}, "SELECT a FROM t WHERE id = 1": {false, false},
	}
	for src, want := range aliases {
		s, err = partiql.Parse(src)
		require.NoError(t, err, src)
		require.Equal(t, want, [2]bool{s.Aliased, s.TableAliased}, src)
	}

	for _, src := range []string{"SELECT a AS FROM t", "SELECT * AS z FROM t", "SELECT a AS 'z' FROM t"} {
		_, err = partiql.Parse(src)
		require.EqualError(t, err, partiql.MsgNotWellFormed+src, src)
	}

	s, err = partiql.Parse("SELECT /* a\n*/ a /**/ FROM /*/ b */ t -- c /*\n WHERE /* d */ id = 'x/*y*/'")
	require.NoError(t, err)
	require.Equal(t, "t", s.Table)
	require.Equal(t, partiql.KindCompare, s.Where.Kind)
	require.Equal(t, partiql.LitString, s.Where.Args[1].Literal.Kind)
	require.Equal(t, "x/*y*/", s.Where.Args[1].Literal.Text, "a comment inside a string is the string's")

	for _, src := range []string{"SELECT a FROM t /* open", "SELECT a FROM t /*/", "SELECT /* /* */ */ a FROM t"} {
		_, err = partiql.Parse(src)
		require.EqualError(t, err, partiql.MsgNotWellFormed+src, src)
	}

	_, err = partiql.Parse("SELECT /* é\n😀 */ f(x) FROM t")
	require.EqualError(t, err, "Unexpected path component at 2:6:1")
}

// FuzzParse: no statement panics the parser or the value builder. A table
// name may be empty ("FROM \"\""); the lookup refuses it.
func FuzzParse(f *testing.F) {
	f.Add("SELECT * FROM t WHERE id = 'a'")
	f.Add("INSERT INTO t VALUE {'id': ?, 's': <<1, 2>>}")

	f.Fuzz(func(t *testing.T, src string) {
		s, err := partiql.Parse(src)
		if err != nil {
			return
		}

		require.LessOrEqual(t, s.Kind, partiql.Exists)

		if s.Value != nil {
			_, _ = partiql.Value(s.Value, nil, "")
		}
	})
}
