package dynamodb_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	codeConditional = "ConditionalCheckFailedException"
	legacyMixed     = "Can not use both expression and non-expression parameters in the same request: " +
		"Non-expression parameters: {"
	legacyInvalid = "One or more parameter values were invalid: "
	msgNamesOnly  = "ExpressionAttributeNames can only be specified when using expressions"
	msgNoComparer = "Comparison type does not exist in DynamoDB"
	msgNoKey      = "Either the KeyConditions or KeyConditionExpression parameter must be specified in the request."
	msgCoAlone    = "ConditionalOperator cannot be used without Filter or Expected"
	msgCoSingle   = "ConditionalOperator can only be used when Filter or Expected has two or more elements"
	msgBadArgs    = legacyInvalid + "Invalid number of argument(s) for the %s ComparisonOperator"
	msgBadType    = legacyInvalid + "ComparisonOperator %s is not valid for %s AttributeValue type"
	sHello        = `{"S":"hello"}`
	n5            = `{"N":"5"}`
	n6            = `{"N":"6"}`
	attrNum       = "num"
	opNull        = "NULL"
	opContains    = "CONTAINS"
	opNotContains = "NOT_CONTAINS"
	opBeginsWith  = "BEGINS_WITH"
	opBetween     = "BETWEEN"
)

// legacyItems puts three items in a table keyed by id: i1 holds every type
// the operators meet, i2 a string and a number, i3 nothing else.
func legacyItems(t *testing.T, s *stack) {
	t.Helper()

	s.ok(t, opCreate, hashTable("legt"))

	for _, item := range []string{
		`{"id":{"S":"i1"},"s":{"S":"hello"},"num":{"N":"5"},"ss":{"SS":["x","y"]},"l":{"L":[{"N":"1"},{"S":"a"}]},` +
			`"m":{"M":{"k":{"N":"1"}}},"bo":{"BOOL":true},"b":{"B":"aGVsbG8="}}`,
		`{"id":{"S":"i2"},"s":{"S":"world"},"num":{"N":"7"}}`,
		`{"id":{"S":"i3"}}`,
	} {
		s.ok(t, opPut, `{"TableName":"legt","Item":`+item+`}`)
	}
}

// ids are the sorted ids of a reply's items.
func ids(t *testing.T, got map[string]any) []string {
	t.Helper()

	items, ok := got["Items"].([]any)
	require.True(t, ok, "no Items: %v", got)

	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, path(t, items, i, "id", "S").(string)) //nolint:forcetypeassert // the id is a string
	}

	slices.Sort(out)

	return out
}

// cmp is a legacy condition: an operator over values, which are JSON.
func cmp(op string, values ...string) string {
	list := ""
	if len(values) > 0 {
		list = `,"AttributeValueList":[` + strings.Join(values, ",") + `]`
	}

	return `{"ComparisonOperator":"` + op + `"` + list + `}`
}

// conds is a map of legacy conditions: name, condition, name, condition...
func conds(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		parts = append(parts, `"`+pairs[i]+`":`+pairs[i+1])
	}

	return `{` + strings.Join(parts, ",") + `}`
}

// Test_legacy_scanFilter: every ComparisonOperator of a ScanFilter selects
// what the LocalStack image selects (verification/SQ-311 r1), through the
// expression evaluator.
func Test_legacy_scanFilter(t *testing.T) {
	s := newStack(t)
	legacyItems(t, s)

	for _, tt := range []struct {
		name, attr, op string
		values         []string
		want           []string
	}{
		{"EQ", "s", "EQ", []string{sHello}, []string{"i1"}},
		{"EQ of another type", "s", "EQ", []string{n5}, nil},
		{"EQ sets in any order", "ss", "EQ", []string{`{"SS":["y","x"]}`}, []string{"i1"}},
		{"EQ number forms", attrNum, "EQ", []string{`{"N":"5.00"}`}, []string{"i1"}},
		{"NE takes the item without it", "s", "NE", []string{sHello}, []string{"i2", "i3"}},
		{"NE of another type", "s", "NE", []string{n5}, []string{"i1", "i2", "i3"}},
		{"LE", "s", "LE", []string{sHello}, []string{"i1"}},
		{"LT", "s", "LT", []string{sHello}, nil},
		{"GE", "s", "GE", []string{sHello}, []string{"i1", "i2"}},
		{"GT number", attrNum, "GT", []string{n5}, []string{"i2"}},
		{"GT of another type", "s", "GT", []string{n5}, nil},
		{opNull, "s", opNull, nil, []string{"i3"}},
		{"NOT_NULL", "s", "NOT_NULL", nil, []string{"i1", "i2"}},
		{"CONTAINS string", "s", opContains, []string{`{"S":"ell"}`}, []string{"i1"}},
		{"CONTAINS set", "ss", opContains, []string{`{"S":"x"}`}, []string{"i1"}},
		{"CONTAINS list", "l", opContains, []string{`{"N":"1"}`}, []string{"i1"}},
		{"CONTAINS binary", "b", opContains, []string{`{"B":"ZWxs"}`}, []string{"i1"}},
		{"CONTAINS in a map", "m", opContains, []string{`{"S":"k"}`}, nil},
		{opNotContains, "s", opNotContains, []string{`{"S":"ell"}`}, []string{"i2"}},
		{"NOT_CONTAINS of another type", "s", opNotContains, []string{n5}, []string{"i1", "i2"}},
		{"NOT_CONTAINS in a map", "m", opNotContains, []string{`{"S":"k"}`}, []string{"i1"}},
		{"NOT_CONTAINS of a missing attribute", "zz", opNotContains, []string{sHello}, nil},
		{opBeginsWith, "s", opBeginsWith, []string{`{"S":"he"}`}, []string{"i1"}},
		{"BEGINS_WITH on a number", attrNum, opBeginsWith, []string{`{"S":"5"}`}, nil},
		{"IN", "s", "IN", []string{sHello, `{"S":"zz"}`}, []string{"i1"}},
		{"IN numbers", attrNum, "IN", []string{`{"N":"7"}`, n6}, []string{"i2"}},
		{opBetween, "num", opBetween, []string{`{"N":"4"}`, n6}, []string{"i1"}},
		{"BETWEEN of another type", "s", opBetween, []string{`{"N":"4"}`, n6}, nil},
		{"a missing attribute", "zz", "EQ", []string{sHello}, nil},
		{"the item's own key", "id", opBeginsWith, []string{`{"S":"i"}`}, []string{"i1", "i2", "i3"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.ok(t, opScan, `{"TableName":"legt","ScanFilter":`+conds(tt.attr, cmp(tt.op, tt.values...))+`}`)
			require.Equal(t, append([]string{}, tt.want...), append([]string{}, ids(t, got)...))
			require.InDelta(t, 3, got["ScannedCount"], 0)
			require.InDelta(t, float64(len(tt.want)), got["Count"], 0)
		})
	}

	got := s.ok(t, opScan, `{"TableName":"legt","ScanFilter":{}}`)
	require.Len(t, ids(t, got), 3, "an empty filter selects everything")
}

// Test_legacy_conditionalOperator: AND is the default and OR any one; the
// operator needs a filter of two or more conditions.
func Test_legacy_conditionalOperator(t *testing.T) {
	s := newStack(t)
	legacyItems(t, s)

	two := conds("s", cmp("EQ", sHello), "num", cmp("EQ", `{"N":"7"}`))

	for _, tt := range []struct {
		extra string
		want  []string
	}{
		{``, nil},
		{`,"ConditionalOperator":"AND"`, nil},
		{`,"ConditionalOperator":"OR"`, []string{"i1", "i2"}},
	} {
		got := s.ok(t, opScan, `{"TableName":"legt","ScanFilter":`+two+tt.extra+`}`)
		require.Equal(t, append([]string{}, tt.want...), append([]string{}, ids(t, got)...), tt.extra)
	}

	one := conds("s", cmp("EQ", sHello))

	s.fails(t, opScan, `{"TableName":"legt","ScanFilter":`+one+`,"ConditionalOperator":"OR"}`, codeValidation,
		msgCoSingle)
	s.fails(t, opScan, `{"TableName":"legt","ConditionalOperator":"OR"}`, codeValidation, msgCoAlone)
	s.fails(t, opScan, `{"TableName":"legt","ScanFilter":{},"ConditionalOperator":"AND"}`, codeValidation, msgCoAlone)
}

// Test_legacy_scanRefusals: the refusals of a ScanFilter, in the image's
// words, and how it mixes with the expressions.
func Test_legacy_scanRefusals(t *testing.T) {
	s := newStack(t)
	legacyItems(t, s)

	for _, tt := range []struct {
		name, condition, message string
	}{
		{"EQ with two values", cmp("EQ", sHello, sHello), strings.Replace(msgBadArgs, "%s", "EQ", 1)},
		{"EQ with none", cmp("EQ"), strings.Replace(msgBadArgs, "%s", "EQ", 1)},
		{"NULL with one", cmp(opNull, sHello), strings.Replace(msgBadArgs, "%s", opNull, 1)},
		{"BETWEEN with one", cmp(opBetween, sHello), strings.Replace(msgBadArgs, "%s", opBetween, 1)},
		{"LE on a set", cmp("LE", `{"SS":["a"]}`), fmt.Sprintf(msgBadType, "LE", "SS")},
		{
			"BEGINS_WITH a number", cmp(opBeginsWith, n5),
			legacyInvalid + "ComparisonOperator BEGINS_WITH is not valid for N AttributeValue type",
		},
		{
			"IN of two types", cmp("IN", sHello, n5),
			legacyInvalid + "AttributeValues inside AttributeValueList must be of same type",
		},
		{
			"BETWEEN reversed", cmp(opBetween, n6, `{"N":"4"}`),
			"The BETWEEN condition was provided a range where the lower bound is greater than the upper bound",
		},
		{"no operator", `{"AttributeValueList":[` + sHello + `]}`, msgNoComparer},
		{"unknown operator", cmp("XX", sHello), msgNoComparer},
		{"empty condition", `{}`, msgNoComparer},
		{
			"empty value", `{"ComparisonOperator":"EQ","AttributeValueList":[{}]}`,
			"Supplied AttributeValue is empty, must contain exactly one of the supported datatypes",
		},
		{"bad number", cmp("EQ", `{"N":"abc"}`), "A value provided cannot be converted into a number"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, opScan, `{"TableName":"legt","ScanFilter":{"s":`+tt.condition+`}}`, codeValidation, tt.message)
		})
	}

	s.fails(t, opScan, `{"TableName":"legt","ScanFilter":{"":`+cmp(opNull)+`}}`, codeValidation, "Empty attribute name")

	const placeholder = `"ExpressionAttributeValues":{":s":{"S":"x"}}`

	both := conds("s", cmp(opNull), "num", cmp(opNull))

	for _, tt := range []struct {
		name, extra, message string
	}{
		{
			"FilterExpression", `"FilterExpression":"s = :s",` + placeholder,
			"ScanFilter} Expression parameters: {FilterExpression}",
		},
		{"ProjectionExpression", `"ProjectionExpression":"s"`, "ScanFilter} Expression parameters: {ProjectionExpression}"},
		{
			"all of them", `"AttributesToGet":["s"],"ProjectionExpression":"s","FilterExpression":"s = :s",` + placeholder,
			"AttributesToGet, ScanFilter} Expression parameters: {ProjectionExpression, FilterExpression}",
		},
		{
			"ConditionalOperator", `"ConditionalOperator":"OR","FilterExpression":"s = :s",` + placeholder,
			"ScanFilter, ConditionalOperator} Expression parameters: {FilterExpression}",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, opScan, `{"TableName":"legt","ScanFilter":`+both+`,`+tt.extra+`}`, codeValidation,
				legacyMixed+tt.message)
		})
	}

	eq := conds("s", cmp("EQ", sHello))

	s.fails(t, opScan, `{"TableName":"legt","ScanFilter":`+eq+`,"ExpressionAttributeNames":{"#a":"a"}}`, codeValidation,
		msgNamesOnly)
	s.fails(t, opScan, `{"TableName":"legt","ScanFilter":`+eq+`,"ExpressionAttributeValues":{":a":{"S":"a"}}}`,
		codeValidation, "ExpressionAttributeValues can only be specified when using expressions: FilterExpression is null")

	// The filter is checked after the table and the index, and before the
	// segments and Limit.
	bad := conds("s", cmp("XX"))

	s.fails(t, opScan, `{"TableName":"nope","ScanFilter":`+bad+`}`,
		"ResourceNotFoundException", "Cannot do operations on a non-existent table")
	s.fails(t, opScan, `{"TableName":"legt","ScanFilter":`+bad+`,"Limit":0,"Segment":3}`, codeValidation, msgNoComparer)
	s.fails(t, opScan, `{"TableName":"legt","IndexName":"zz","ScanFilter":`+bad+`}`, codeValidation,
		"The table does not have the specified index: zz")

	got := s.ok(t, opScan, `{"TableName":"legt","ScanFilter":`+eq+`,"AttributesToGet":["s"]}`)
	require.Equal(t, map[string]any{"s": map[string]any{"S": "hello"}}, path(t, got, "Items", 0))

	got = s.ok(t, opScan, `{"TableName":"legt","ScanFilter":`+eq+`,"Select":"COUNT"}`)
	require.InDelta(t, 1, got["Count"], 0)
	require.NotContains(t, got, "Items")
}

// Test_legacy_query: KeyConditions, QueryFilter and ConditionalOperator read
// as their expressions do, and refuse as the LocalStack image does
// (verification/SQ-311 r2, r3).
func Test_legacy_query(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeTable)
	fill(t, s, 5, 1)

	pk := `"pk":` + cmp("EQ", `{"S":"a"}`)
	sk := func(op string, values ...string) string { return pk + `,"sk":` + cmp(op, values...) }
	one, two := `{"N":"1"}`, `{"N":"2"}`

	for _, tt := range []struct {
		name, kc, extra string
		want            []string
	}{
		{"the partition", pk, ``, []string{"0", "1", "2", "3", "4"}},
		{"EQ", sk("EQ", two), ``, []string{"2"}},
		{"GT", sk("GT", two), ``, []string{"3", "4"}},
		{"LE", sk("LE", one), ``, []string{"0", "1"}},
		{opBetween, sk(opBetween, one, two), ``, []string{"1", "2"}},
		{"backwards", pk, `,"ScanIndexForward":false,"Limit":2`, []string{"4", "3"}},
		{"QueryFilter", pk, `,"QueryFilter":` + conds("x", cmp("GE", `{"N":"3"}`)), []string{"3", "4"}},
		{
			"QueryFilter OR", pk, `,"ConditionalOperator":"OR","QueryFilter":` +
				conds("x", cmp("EQ", `{"N":"0"}`), "y", cmp(opNull)), []string{"0"},
		},
		{
			"a FilterExpression", pk, `,"FilterExpression":"x > :x","ExpressionAttributeValues":{":x":{"N":"3"}}`,
			[]string{"4"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.ok(t, opQuery, `{"TableName":"tab2","KeyConditions":{`+tt.kc+`}`+tt.extra+`}`)
			items, _ := got["Items"].([]any)
			keys := make([]string, 0, len(items))

			for i := range items {
				keys = append(keys, path(t, items, i, "sk", "N").(string)) //nolint:forcetypeassert // a number
			}

			require.Equal(t, tt.want, keys)
		})
	}

	atg := s.ok(t, opQuery, `{"TableName":"tab2","KeyConditions":{`+pk+`},"AttributesToGet":["x"],"Limit":1}`)
	require.Equal(t, map[string]any{"x": map[string]any{"N": "0"}}, path(t, atg, "Items", 0))

	gsi := s.ok(t, opQuery, `{"TableName":"tab2","IndexName":"gkey","KeyConditions":{"g":`+cmp("EQ", `{"S":"G"}`)+
		`},"QueryFilter":`+conds("sk", cmp("GT", two))+`}`)
	require.InDelta(t, 2, gsi["Count"], 0, "a table key attribute is no key of the index")

	const (
		notSupported = "Query key condition not supported"
		missedKey    = "Query condition missed key schema element"
		onlyFilter   = "QueryFilter can only contain non-primary key attributes: Primary key attribute: "
		kce          = `,"KeyConditionExpression":"pk = :a","ExpressionAttributeValues":{":a":{"S":"a"}}`
	)

	for _, tt := range []struct {
		name, body, message string
	}{
		{"no conditions", `"KeyConditions":{}`, "Conditions must be at least length 1"},
		{
			"three conditions", `"KeyConditions":{` + sk("EQ", one) + `,"x":` + cmp("EQ", one) + `}`,
			"Conditions can be of length 1 or 2 only",
		},
		{"no hash", `"KeyConditions":{"sk":` + cmp("EQ", one) + `}`, missedKey},
		{"a non-key attribute", `"KeyConditions":{` + pk + `,"x":` + cmp("EQ", one) + `}`, missedKey},
		{"hash not EQ", `"KeyConditions":{"pk":` + cmp("LE", `{"S":"a"}`) + `}`, notSupported},
		{"hash NULL", `"KeyConditions":{"pk":` + cmp(opNull) + `}`, "Only scalar types can be used as Hash Key values"},
		{
			"range NOT_NULL", `"KeyConditions":{` + sk("NOT_NULL") + `}`,
			"Only scalar types can be used as Range Key values",
		},
		{
			"range IN", `"KeyConditions":{` + sk("IN", one) + `}`,
			"Attempted conditional constraint is not an indexable operation",
		},
		{"range NE", `"KeyConditions":{` + sk("NE", one) + `}`, notSupported},
		{
			"range of the wrong type", `"KeyConditions":{` + sk("EQ", `{"S":"1"}`) + `}`,
			legacyInvalid + "Condition parameter type does not match schema type",
		},
		{"unknown operator", `"KeyConditions":{"pk":` + cmp("XX", `{"S":"a"}`) + `}`, msgNoComparer},
		{
			"QueryFilter on the hash", `"KeyConditions":{` + pk + `},"QueryFilter":` + conds("pk", cmp(opNull)),
			onlyFilter + "pk",
		},
		{
			"QueryFilter on the index range", `"IndexName":"lidx","KeyConditions":{` + pk + `},"QueryFilter":` +
				conds("l", cmp(opNull)), onlyFilter + "l",
		},
		{"ConditionalOperator alone", `"KeyConditions":{` + pk + `},"ConditionalOperator":"OR"`, msgCoAlone},
		{
			"Limit before the key", `"KeyConditions":{"pk":` + cmp("EQ") + `},"Limit":0`,
			"Limit must be greater than or equal to 1",
		},
		{
			"the key before Select", `"KeyConditions":{"pk":` + cmp("EQ") + `},"Select":"ALL_PROJECTED_ATTRIBUTES"`,
			strings.Replace(msgBadArgs, "%s", "EQ", 1),
		},
		{
			"KeyConditionExpression", `"KeyConditions":{` + pk + `}` + kce,
			legacyMixed + "KeyConditions} Expression parameters: {KeyConditionExpression}",
		},
		{
			"QueryFilter and a FilterExpression", `"KeyConditions":{` + pk + `},"QueryFilter":` + conds("x", cmp(opNull)) +
				`,"FilterExpression":"x > :x","ExpressionAttributeValues":{":x":{"N":"3"}}`,
			legacyMixed + "QueryFilter} Expression parameters: {FilterExpression}",
		},
		{
			"AttributesToGet and a ProjectionExpression",
			`"KeyConditions":{` + pk + `},"AttributesToGet":["x"],"ProjectionExpression":"x"`,
			legacyMixed + "AttributesToGet} Expression parameters: {ProjectionExpression}",
		},
		{
			"placeholders with no expression", `"KeyConditions":{` + pk + `},"ExpressionAttributeValues":{":a":{"S":"a"}}`,
			"ExpressionAttributeValues can only be specified when using expressions: " +
				"FilterExpression and KeyConditionExpression are null",
		},
		{"a QueryFilter alone", `"QueryFilter":` + conds("x", cmp(opNull)), msgNoKey},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, opQuery, `{"TableName":"tab2",`+tt.body+`}`, codeValidation, tt.message)
		})
	}
}

// Test_legacy_expected: Expected and ConditionalOperator guard PutItem,
// DeleteItem and UpdateItem as a ConditionExpression does.
func Test_legacy_expected(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("legw"))

	const (
		key    = `"Key":{"id":{"S":"1"}}`
		update = "UpdateItem"
		failed = "The conditional request failed"
		hello  = `"s":{"Value":{"S":"hello"}}`
		nine   = `"num":{"Value":{"N":"9"}}`
	)

	reset := func() {
		s.ok(t, opDeleteI, `{"TableName":"legw",`+key+`}`)
		s.ok(t, opPut, `{"TableName":"legw","Item":{"id":{"S":"1"},"s":{"S":"hello"},"num":{"N":"5"}}}`)
	}

	put := func(expected string) string {
		return `{"TableName":"legw","Item":{"id":{"S":"1"},"n":{"N":"1"}},` + expected + `}`
	}

	for _, tt := range []struct {
		name, expected string
		ok             bool
	}{
		{"Value", `"Expected":{` + hello + `}`, true},
		{"Value that differs", `"Expected":{"s":{"Value":{"S":"x"}}}`, false},
		{"Exists true with Value", `"Expected":{"s":{"Exists":true,"Value":{"S":"hello"}}}`, true},
		{"Exists false", `"Expected":{"s":{"Exists":false}}`, false},
		{"Exists false of a missing attribute", `"Expected":{"zz":{"Exists":false}}`, true},
		{"an empty Expected", `"Expected":{}`, true},
		{"ComparisonOperator", `"Expected":{"num":` + cmp("GT", `{"N":"4"}`) + `}`, true},
		{"ComparisonOperator with Value", `"Expected":{"num":{"ComparisonOperator":"GT","Value":{"N":"5"}}}`, false},
		{opNull, `"Expected":{"zz":` + cmp(opNull) + `}`, true},
		{"AND", `"Expected":{` + hello + `,` + nine + `}`, false},
		{"OR", `"ConditionalOperator":"OR","Expected":{` + hello + `,` + nine + `}`, true},
		{"OR with none true", `"ConditionalOperator":"OR","Expected":{"s":{"Value":{"S":"x"}},` + nine + `}`, false},
	} {
		t.Run("PutItem "+tt.name, func(t *testing.T) {
			reset()

			if tt.ok {
				s.ok(t, opPut, put(tt.expected))

				return
			}

			s.fails(t, opPut, put(tt.expected), codeConditional, failed)
			require.NotContains(t, path(t, s.ok(t, opGet, `{"TableName":"legw",`+key+`}`), "Item"), "n")
		})
	}

	reset()
	s.fails(t, opDeleteI, `{"TableName":"legw",`+key+`,"Expected":{"s":{"Value":{"S":"x"}}}}`, codeConditional, failed)
	s.ok(t, opDeleteI, `{"TableName":"legw",`+key+`,"Expected":{`+hello+`}}`)
	require.NotContains(t, s.ok(t, opGet, `{"TableName":"legw",`+key+`}`), "Item")

	reset()
	s.fails(t, update, `{"TableName":"legw",`+key+`,"Expected":{"s":{"Value":{"S":"x"}}}}`, codeConditional, failed)
	s.ok(t, update, `{"TableName":"legw",`+key+`,"Expected":{`+hello+`}}`)

	for _, tt := range []struct{ name, expected, message string }{
		{"empty entry", `"Expected":{"s":{}}`, "Value must be provided when Exists is null for Attribute: s"},
		{
			"Exists true alone", `"Expected":{"s":{"Exists":true}}`,
			"Value must be provided when Exists is true for Attribute: s",
		},
		{
			"Exists false with Value", `"Expected":{"s":{"Exists":false,"Value":{"S":"x"}}}`,
			"Value cannot be used when Exists is false for Attribute: s",
		},
		{
			"Value and list", `"Expected":{"s":{"Value":{"S":"x"},"AttributeValueList":[{"S":"x"}]}}`,
			"Value and AttributeValueList cannot be used together for Attribute: s",
		},
		{
			"list alone", `"Expected":{"s":{"AttributeValueList":[{"S":"x"}]}}`,
			"AttributeValueList can only be used with a ComparisonOperator for Attribute: s",
		},
		{
			"Exists and operator", `"Expected":{"s":{"Exists":true,"ComparisonOperator":"NULL"}}`,
			"Exists and ComparisonOperator cannot be used together for Attribute: s",
		},
		{
			"operator without values", `"Expected":{"s":` + cmp("EQ") + `}`,
			"Value or AttributeValueList must be used with ComparisonOperator: EQ for Attribute: s",
		},
	} {
		t.Run("PutItem refuses "+tt.name, func(t *testing.T) {
			s.fails(t, opPut, put(tt.expected), codeValidation, legacyInvalid+tt.message)
		})
	}

	for _, tt := range []struct{ name, expected, message string }{
		{"one condition with an operator", `"ConditionalOperator":"OR","Expected":{"s":{"Value":{"S":"x"}}}`, msgCoSingle},
		{"ConditionalOperator alone", `"ConditionalOperator":"OR"`, msgCoAlone},
		{
			"ConditionExpression", `"Expected":{"s":{"Value":{"S":"x"}}},"ConditionExpression":"attribute_exists(s)"`,
			legacyMixed + "Expected} Expression parameters: {ConditionExpression}",
		},
		{
			"ConditionalOperator and ConditionExpression",
			`"ConditionalOperator":"OR","ConditionExpression":"attribute_exists(s)"`,
			legacyMixed + "ConditionalOperator} Expression parameters: {ConditionExpression}",
		},
		{"names", `"Expected":{"s":{"Value":{"S":"x"}}},"ExpressionAttributeNames":{"#s":"s"}`, msgNamesOnly},
	} {
		t.Run("PutItem refuses "+tt.name, func(t *testing.T) {
			s.fails(t, opPut, put(tt.expected), codeValidation, tt.message)
		})
	}

	s.fails(t, update, `{"TableName":"legw",`+key+`,"Expected":{"s":{"Value":{"S":"x"}}},`+
		`"UpdateExpression":"SET n = :n","ExpressionAttributeValues":{":n":{"N":"1"}}}`, codeValidation,
		legacyMixed+"Expected} Expression parameters: {UpdateExpression}")
}

// Test_legacy_localIndexCapacity: a Scan of a local index that asks for an
// attribute the index does not project reads the table's item as well, and
// reports its units on the table. AWS documents the charge ("you are charged
// for read capacity units for every base table item fetched", the developer
// guide's local secondary indexes page); LocalStack's image reports 0 for the
// table there.
func Test_legacy_localIndexCapacity(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeTable)
	fill(t, s, 1, 1)

	got := s.ok(t, opScan, `{"TableName":"tab2","IndexName":"lidx","AttributesToGet":["x","y"],`+
		`"ReturnConsumedCapacity":"INDEXES"}`)
	require.InDelta(t, 0.5, path(t, got, "ConsumedCapacity", "Table", "CapacityUnits"), 0)
	require.InDelta(t, 0.5, path(t, got, "ConsumedCapacity", "LocalSecondaryIndexes", "lidx", "CapacityUnits"), 0)
	require.InDelta(t, 1, path(t, got, "ConsumedCapacity", "CapacityUnits"), 0)
	require.Contains(t, path(t, got, "Items", 0), "y")

	projected := s.ok(t, opScan, `{"TableName":"tab2","IndexName":"lidx","AttributesToGet":["x"],`+
		`"ReturnConsumedCapacity":"INDEXES"}`)
	require.InDelta(t, 0, path(t, projected, "ConsumedCapacity", "Table", "CapacityUnits"), 0)
}
