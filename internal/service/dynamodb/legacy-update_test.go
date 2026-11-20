package dynamodb_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	auTable    = "legu"
	auKey      = `"Key":{"id":{"S":"1"}}`
	auReturn   = `"ReturnValues":"ALL_NEW"`
	auItem     = `"id":{"S":"1"},"s":{"S":"hello"},"num":{"N":"5"},"ss":{"SS":["x","y"]},"l":{"L":[{"N":"1"}]}`
	msgAuEnum  = "Member must satisfy enum value set: [ADD, DELETE, PUT]"
	msgAuNoVal = legacyInvalid + "Only DELETE action is allowed when no attribute value is specified"
	msgAuMatch = "Type mismatch for attribute to update"
	msgAuKey   = legacyInvalid + "Cannot update attribute id. This attribute is part of the key"
	msgRvocf   = "1 validation error detected: Value 'X' at 'returnValuesOnConditionCheckFailure' failed to " +
		"satisfy constraint: Member must satisfy enum value set: [ALL_OLD, NONE]"
	msgBadCmp = legacyInvalid + "Value or AttributeValueList must be used with ComparisonOperator: XX " +
		"for Attribute: s"
	msgNestDeep    = "Nesting Levels have exceeded supported limits"
	msgAuBadNumber = "A value provided cannot be converted into a number"
	msgAuEmpty     = "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes"
	auPut          = `{"n":{"Action":"PUT","Value":{"N":"1"}}}`
	sX             = `{"S":"x"}`
	fieldTable     = "TableName"
)

// auStack is a stack with the table auTable holding the item auItem.
func auStack(t *testing.T) (*stack, func()) {
	t.Helper()

	s := newStack(t)
	s.ok(t, opCreate, hashTable(auTable))

	reset := func() {
		s.ok(t, opDeleteItem, `{"TableName":"`+auTable+`",`+auKey+`}`)
		s.ok(t, opPut, `{"TableName":"`+auTable+`","Item":{`+auItem+`}}`)
	}
	reset()

	return s, reset
}

// auUpdate is an UpdateItem of the item with key 1, with the extra members.
func auUpdate(updates string, extra ...string) string {
	members := append([]string{`"TableName":"` + auTable + `"`, auKey, `"AttributeUpdates":` + updates}, extra...)

	return "{" + strings.Join(members, ",") + "}"
}

// mustJSON is v as JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()

	b, err := json.Marshal(v)
	require.NoError(t, err)

	return string(b)
}

// auAttrs is the item as an UpdateItem answers ReturnValues ALL_NEW, with the
// attributes of auItem the names drop, and the extra members added.
func auAttrs(drop []string, extra string) string {
	var members []string

	for _, a := range []struct{ name, value string }{
		{"id", `{"S":"1"}`},
		{"s", `{"S":"hello"}`},
		{attrNum, `{"N":"5"}`},
		{"ss", `{"SS":["x","y"]}`},
		{"l", `{"L":[{"N":"1"}]}`},
	} {
		if !slices.Contains(drop, a.name) {
			members = append(members, `"`+a.name+`":`+a.value)
		}
	}

	if extra != "" {
		members = append(members, extra)
	}

	return `{"Attributes":{` + strings.Join(members, ",") + `}}`
}

// Test_legacyUpdate_actions: AttributeUpdates PUT, ADD and DELETE change the
// item as the image does, answering the return values.
func Test_legacyUpdate_actions(t *testing.T) {
	s, reset := auStack(t)

	for _, tt := range []struct {
		name, updates, want string
	}{
		{
			"PUT a new attribute", auPut,
			auAttrs(nil, `"n":{"N":"1"}`),
		},
		{
			"no Action is PUT", `{"n":{"Value":{"N":"1"}}}`,
			auAttrs(nil, `"n":{"N":"1"}`),
		},
		{
			"PUT replaces an attribute of another type", `{"num":{"Action":"PUT","Value":{"S":"x"}}}`,
			auAttrs([]string{attrNum}, `"num":{"S":"x"}`),
		},
		{
			"PUT of an empty string", `{"e":{"Action":"PUT","Value":{"S":""}}}`,
			auAttrs(nil, `"e":{"S":""}`),
		},
		{
			"PUT of a name with a dot is one attribute", `{"a.b":{"Action":"PUT","Value":{"S":"x"}}}`,
			auAttrs(nil, `"a.b":{"S":"x"}`),
		},
		{
			"PUT of a reserved word", `{"name":{"Action":"PUT","Value":{"S":"x"}}}`,
			auAttrs(nil, `"name":{"S":"x"}`),
		},
		{
			"ADD to a number", `{"num":{"Action":"ADD","Value":{"N":"2"}}}`,
			auAttrs([]string{attrNum}, `"num":{"N":"7"}`),
		},
		{
			"ADD a negative number", `{"num":{"Action":"ADD","Value":{"N":"-10.5"}}}`,
			auAttrs([]string{attrNum}, `"num":{"N":"-5.5"}`),
		},
		{
			"ADD sets a missing attribute", `{"zz":{"Action":"ADD","Value":{"N":"2"}}}`,
			auAttrs(nil, `"zz":{"N":"2"}`),
		},
		{
			"ADD to a set", `{"ss":{"Action":"ADD","Value":{"SS":["y","z"]}}}`,
			auAttrs([]string{"ss"}, `"ss":{"SS":["x","y","z"]}`),
		},
		{
			"ADD sets a missing set", `{"zz":{"Action":"ADD","Value":{"NS":["1"]}}}`,
			auAttrs(nil, `"zz":{"NS":["1"]}`),
		},
		{
			"ADD appends to a list", `{"l":{"Action":"ADD","Value":{"L":[{"S":"a"}]}}}`,
			auAttrs([]string{"l"}, `"l":{"L":[{"N":"1"},{"S":"a"}]}`),
		},
		{
			"ADD sets a missing list", `{"zz":{"Action":"ADD","Value":{"L":[{"S":"a"}]}}}`,
			auAttrs(nil, `"zz":{"L":[{"S":"a"}]}`),
		},
		{
			"DELETE with no value removes the attribute", `{"s":{"Action":"DELETE"}}`,
			auAttrs([]string{"s"}, ""),
		},
		{
			"DELETE of a missing attribute", `{"zz":{"Action":"DELETE"}}`,
			auAttrs(nil, ""),
		},
		{
			"DELETE of set members", `{"ss":{"Action":"DELETE","Value":{"SS":["x","q"]}}}`,
			auAttrs([]string{"ss"}, `"ss":{"SS":["y"]}`),
		},
		{
			"DELETE of every member removes the set", `{"ss":{"Action":"DELETE","Value":{"SS":["x","y"]}}}`,
			auAttrs([]string{"ss"}, ""),
		},
		{
			"DELETE of set members from a missing attribute", `{"zz":{"Action":"DELETE","Value":{"SS":["x"]}}}`,
			auAttrs(nil, ""),
		},
		{"no entries", `{}`, auAttrs(nil, "")},
		{
			"several entries",
			`{"s":{"Action":"PUT","Value":{"S":"q"}},"num":{"Action":"ADD","Value":{"N":"1"}},` +
				`"ss":{"Action":"DELETE","Value":{"SS":["y"]}},"l":{"Action":"DELETE"},"n2":{"Value":{"N":"9"}}}`,
			`{"Attributes":{"id":{"S":"1"},"s":{"S":"q"},"num":{"N":"6"},"ss":{"SS":["x"]},"n2":{"N":"9"}}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reset()

			got := s.call(t, usEast1, opUpdateItem, auUpdate(tt.updates, auReturn))
			require.Equal(t, 200, got.status, got.body)
			require.JSONEq(t, tt.want, got.body)

			// What was answered is what is stored.
			var want map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.want), &want))

			stored := s.ok(t, opGet, `{"TableName":"`+auTable+`",`+auKey+`}`)
			require.Equal(t, want["Attributes"], stored["Item"])
		})
	}
}

// Test_legacyUpdate_returnValues: every ReturnValues answers for AttributeUpdates
// as it does for an UpdateExpression, an entry that removed an attribute
// leaving it out of UPDATED_NEW.
func Test_legacyUpdate_returnValues(t *testing.T) {
	s, reset := auStack(t)
	updates := `{"num":{"Action":"ADD","Value":{"N":"2"}},"s":{"Action":"DELETE"},"zz":{"Action":"DELETE"}}`

	for _, tt := range []struct{ mode, want string }{
		{"NONE", `{}`},
		{"ALL_OLD", `{"Attributes":{` + auItem + `}}`},
		{"UPDATED_OLD", `{"Attributes":{"num":{"N":"5"},"s":{"S":"hello"}}}`},
		{"UPDATED_NEW", `{"Attributes":{"num":{"N":"7"}}}`},
		{"ALL_NEW", `{"Attributes":{"id":{"S":"1"},"num":{"N":"7"},"ss":{"SS":["x","y"]},"l":{"L":[{"N":"1"}]}}}`},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			reset()

			got := s.call(t, usEast1, opUpdateItem, auUpdate(updates, `"ReturnValues":"`+tt.mode+`"`))
			require.Equal(t, 200, got.status, got.body)
			require.JSONEq(t, tt.want, got.body)
		})
	}

	reset()
	s.fails(t, opUpdateItem, auUpdate(updates, `"ReturnValues":"X"`), codeValidation, "Return values set to invalid value")

	got := s.ok(t, opUpdateItem, auUpdate(auPut, `"ReturnConsumedCapacity":"TOTAL"`))
	require.Equal(t, map[string]any{fieldTable: auTable, keyUnits: 1.0}, got["ConsumedCapacity"])
}

// Test_legacyUpdate_newItem: AttributeUpdates create the item from its key, as
// an UpdateExpression does, and an update with no entries stores the key.
func Test_legacyUpdate_newItem(t *testing.T) {
	s, _ := auStack(t)

	for id, updates := range map[string]string{
		"p": auPut,
		"a": `{"n":{"Action":"ADD","Value":{"N":"1"}}}`,
		"d": `{"n":{"Action":"DELETE"}}`,
		"e": `{}`,
	} {
		got := s.ok(t, opUpdateItem, `{"TableName":"`+auTable+`","Key":{"id":{"S":"`+id+`"}},`+
			`"AttributeUpdates":`+updates+`,`+auReturn+`}`)

		want := `{"Attributes":{"id":{"S":"` + id + `"}}}`
		if id == "p" || id == "a" {
			want = `{"Attributes":{"id":{"S":"` + id + `"},"n":{"N":"1"}}}`
		}

		require.JSONEq(t, want, mustJSON(t, got))
	}
}

// Test_legacyUpdate_refusals: an entry DynamoDB Local refuses answers its
// message, and stores nothing.
func Test_legacyUpdate_refusals(t *testing.T) {
	s, reset := auStack(t)

	typeMsg := func(action, t string) string {
		return fmt.Sprintf(legacyInvalid+"%s action is not supported for the type %s", action, t)
	}

	for _, tt := range []struct{ name, updates, message string }{
		{"PUT without a value", `{"n":{"Action":"PUT"}}`, msgAuNoVal},
		{"no Action and no value", `{"n":{}}`, msgAuNoVal},
		{"ADD without a value", `{"num":{"Action":"ADD"}}`, msgAuNoVal},
		{"an Action that is none of the three", `{"n":{"Action":"XX","Value":{"N":"1"}}}`, msgAuEnum},
		{"an Action in lower case", `{"n":{"Action":"put","Value":{"N":"1"}}}`, msgAuEnum},
		{"an Action that is none, and no value", `{"n":{"Action":"XX"}}`, msgAuEnum},
		{"a key attribute", `{"id":{"Action":"PUT","Value":{"S":"2"}}}`, msgAuKey},
		{"a key attribute, to the same value", `{"id":{"Action":"PUT","Value":{"S":"1"}}}`, msgAuKey},
		{"a key attribute, removed", `{"id":{"Action":"DELETE"}}`, msgAuKey},
		{"a key attribute, with a bad Action", `{"id":{"Action":"XX"}}`, msgAuKey},
		{"an empty attribute name", `{"":{"Action":"PUT","Value":{"S":"x"}}}`, "Empty attribute name"},
		{"ADD of a string", `{"s":{"Action":"ADD","Value":{"S":"z"}}}`, typeMsg("ADD", "STRING")},
		{"ADD of a string, to a missing attribute", `{"zz":{"Action":"ADD","Value":{"S":"z"}}}`, typeMsg("ADD", "STRING")},
		{"ADD of a map", `{"zz":{"Action":"ADD","Value":{"M":{}}}}`, typeMsg("ADD", "MAP")},
		{"ADD of a boolean", `{"zz":{"Action":"ADD","Value":{"BOOL":true}}}`, typeMsg("ADD", "BOOLEAN")},
		{"ADD of a binary", `{"zz":{"Action":"ADD","Value":{"B":"aGVsbG8="}}}`, typeMsg("ADD", "BINARY")},
		{"ADD of a null", `{"zz":{"Action":"ADD","Value":{"NULL":true}}}`, typeMsg("ADD", "NULL")},
		{"ADD of a number to a string", `{"s":{"Action":"ADD","Value":{"N":"1"}}}`, msgAuMatch},
		{"ADD of a set to a number", `{"num":{"Action":"ADD","Value":{"SS":["a"]}}}`, msgAuMatch},
		{"ADD of a number set to a string set", `{"ss":{"Action":"ADD","Value":{"NS":["1"]}}}`, msgAuMatch},
		{"ADD of a list to a string", `{"s":{"Action":"ADD","Value":{"L":[]}}}`, msgAuMatch},
		{"ADD of a number to a list", `{"l":{"Action":"ADD","Value":{"N":"1"}}}`, msgAuMatch},
		{
			"DELETE of a string", `{"s":{"Action":"DELETE","Value":{"S":"hello"}}}`,
			legacyInvalid + "DELETE action with value is not supported for the type STRING",
		},
		{
			"DELETE of a number", `{"num":{"Action":"DELETE","Value":{"N":"5"}}}`,
			legacyInvalid + "DELETE action with value is not supported for the type NUMBER",
		},
		{
			"DELETE of a string, from a missing attribute", `{"zz":{"Action":"DELETE","Value":{"S":"x"}}}`,
			legacyInvalid + "DELETE action with value is not supported for the type STRING",
		},
		{
			"DELETE of a list", `{"l":{"Action":"DELETE","Value":{"L":[]}}}`,
			legacyInvalid + "DELETE action with value is not supported for the type LIST",
		},
		{"DELETE of a number set from a string set", `{"ss":{"Action":"DELETE","Value":{"NS":["1"]}}}`, msgAuMatch},
		{"DELETE of a set from a number", `{"num":{"Action":"DELETE","Value":{"SS":["x"]}}}`, msgAuMatch},
		{"a value that is not a number", `{"zz":{"Action":"PUT","Value":{"N":"abc"}}}`, msgAuBadNumber},
		{"an empty set", `{"zz":{"Action":"PUT","Value":{"SS":[]}}}`, legacyInvalid + "An string set  may not be empty"},
		{"an empty value", `{"zz":{"Action":"PUT","Value":{}}}`, msgAuEmpty},
		{
			"an entry after one that changes the type",
			`{"a":{"Action":"PUT","Value":{"S":"a"}},"s":{"Action":"ADD","Value":{"N":"1"}}}`, msgAuMatch,
		},
		{
			"the first entry that is refused, in name order",
			`{"b":{"Action":"PUT"},"a":{"Action":"XX","Value":{"S":"a"}}}`, msgAuEnum,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reset()
			s.fails(t, opUpdateItem, auUpdate(tt.updates), codeValidation, tt.message)

			stored := s.ok(t, opGet, `{"TableName":"`+auTable+`",`+auKey+`}`)
			require.JSONEq(t, `{"Item":{`+auItem+`}}`, mustJSON(t, stored))
		})
	}
}

// Test_legacyUpdate_mixing: AttributeUpdates, Expected and ConditionalOperator
// are refused with UpdateExpression and ConditionExpression, naming the
// parameters in DynamoDB Local's order, and with names or values that no
// expression uses.
func Test_legacyUpdate_mixing(t *testing.T) {
	s, _ := auStack(t)

	put := auPut
	hello := `"Expected":{"s":{"Value":{"S":"hello"}}}`
	ue := `"UpdateExpression":"SET n = :n","ExpressionAttributeValues":{":n":{"N":"1"}}`
	ce := `"ConditionExpression":"attribute_exists(id)"`

	for _, tt := range []struct {
		name    string
		updates string
		extra   []string
		message string
	}{
		{"UpdateExpression", put, []string{ue}, legacyMixed + "AttributeUpdates} Expression parameters: {UpdateExpression}"},
		{
			"ConditionExpression", put,
			[]string{ce},
			legacyMixed + "AttributeUpdates} Expression parameters: {ConditionExpression}",
		},
		{
			"both expressions", put,
			[]string{ue, ce},
			legacyMixed + "AttributeUpdates} Expression parameters: {UpdateExpression, ConditionExpression}",
		},
		{
			"Expected and ConditionExpression", put,
			[]string{hello, ce},
			legacyMixed + "AttributeUpdates, Expected} Expression parameters: {ConditionExpression}",
		},
		{
			"ConditionalOperator and ConditionExpression", put,
			[]string{`"ConditionalOperator":"AND"`, ce},
			legacyMixed + "AttributeUpdates, ConditionalOperator} Expression parameters: {ConditionExpression}",
		},
		{
			"Expected and UpdateExpression", put,
			[]string{hello, ue},
			legacyMixed + "AttributeUpdates, Expected} Expression parameters: {UpdateExpression}",
		},
		{"names", put, []string{`"ExpressionAttributeNames":{"#a":"n"}`}, msgNamesOnly},
		{
			"values", put,
			[]string{`"ExpressionAttributeValues":{":a":{"N":"1"}}`},
			"ExpressionAttributeValues can only be specified when using expressions: " +
				"UpdateExpression and ConditionExpression are null",
		},
		{"a lone ConditionalOperator", put, []string{`"ConditionalOperator":"AND"`}, msgCoAlone},
		{"ConditionalOperator and one Expected", put, []string{hello, `"ConditionalOperator":"OR"`}, msgCoSingle},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, opUpdateItem, auUpdate(tt.updates, tt.extra...), codeValidation, tt.message)
		})
	}

	// An AttributeUpdates with no entries is not set, and mixes with nothing.
	got := s.ok(t, opUpdateItem, auUpdate(`{}`, ue, auReturn))
	require.Equal(t, "1", path(t, got, "Attributes", "n", "N"))

	got = s.ok(t, opUpdateItem, auUpdate(`{}`, ce, auReturn))
	require.Equal(t, "hello", path(t, got, "Attributes", "s", "S"))
}

// Test_legacyUpdate_expected: Expected holds an AttributeUpdates back as it
// does an UpdateExpression, and an entry that is refused answers only once the
// condition holds, as the image does.
func Test_legacyUpdate_expected(t *testing.T) {
	s, reset := auStack(t)

	put := auPut
	stored := func() string {
		return mustJSON(t, s.ok(t, opGet, `{"TableName":"`+auTable+`",`+auKey+`}`))
	}

	reset()
	s.ok(t, opUpdateItem, auUpdate(put, `"Expected":{"s":{"Value":{"S":"hello"}}}`))
	require.Contains(t, stored(), `"n":{"N":"1"}`)

	reset()
	s.fails(t, opUpdateItem, auUpdate(put, `"Expected":{"s":{"Value":{"S":"x"}}}`), codeCondFail, msgCondFail)
	require.JSONEq(t, `{"Item":{`+auItem+`}}`, stored())

	reset()
	s.ok(t, opUpdateItem, auUpdate(put, `"ConditionalOperator":"OR"`,
		`"Expected":{"s":{"Value":{"S":"x"}},"num":{"Value":{"N":"5"}}}`))

	// The entry is refused after the condition: a failed condition answers
	// first, and so does the enum of ReturnValuesOnConditionCheckFailure.
	for _, bad := range []string{
		`{"n":{"Action":"PUT"}}`, `{"n":{"Action":"XX","Value":{"N":"1"}}}`, `{"id":{"Action":"PUT","Value":{"S":"2"}}}`,
		`{"s":{"Action":"ADD","Value":{"N":"1"}}}`, `{"n":{"Action":"PUT","Value":{"N":"abc"}}}`, `{"":{"Value":{"S":"x"}}}`,
	} {
		s.fails(t, opUpdateItem, auUpdate(bad, `"Expected":{"s":{"Value":{"S":"x"}}}`), codeCondFail, msgCondFail)
		s.fails(t, opUpdateItem, auUpdate(bad, `"ReturnValuesOnConditionCheckFailure":"X"`), codeValidation, msgRvocf)
	}

	reset()

	got := s.call(t, usEast1, opUpdateItem, auUpdate(put,
		`"Expected":{"s":{"Value":{"S":"x"}}}`, `"ReturnValuesOnConditionCheckFailure":"ALL_OLD"`))
	require.Equal(t, 400, got.status)
	require.JSONEq(t, `{"__type":"ConditionalCheckFailedException","message":"`+msgCondFail+`",`+
		`"Item":{`+auItem+`}}`, got.body)
}

// Test_legacyUpdate_nesting: an AttributeUpdates value passes the same nesting
// bound an UpdateExpression does: an attribute nests at most 32 levels. The
// image takes a PUT of 33 where an UpdateExpression's SET is refused, and gostack
// refuses both.
func Test_legacyUpdate_nesting(t *testing.T) {
	s, _ := auStack(t)

	nest := func(levels int) string {
		v := sX
		for range levels - 1 {
			v = `{"L":[` + v + `]}`
		}

		return v
	}

	put := func(levels int) string {
		return auUpdate(`{"zz":{"Action":"PUT","Value":` + nest(levels) + `}}`)
	}

	s.ok(t, opUpdateItem, put(32))
	s.fails(t, opUpdateItem, put(33), codeValidation, msgNestDeep)
	s.fails(t, opUpdateItem, put(34), codeValidation,
		msgNestDeep+": Attributes in the item have nested levels beyond supported limit")

	// ADD of a list that makes the stored list too deep.
	s.ok(t, opUpdateItem, auUpdate(`{"deep":{"Action":"PUT","Value":`+nest(31)+`}}`))
	s.fails(t, opUpdateItem, auUpdate(`{"deep":{"Action":"ADD","Value":{"L":[`+nest(32)+`]}}}`),
		codeValidation, msgNestDeep)
}
