package dynamodb_test

import (
	"strings"
	"testing"
)

// Test_legacyWrite_returnValuesOnConditionCheckFailure: an invalid
// ReturnValuesOnConditionCheckFailure is reported after the checks DynamoDB
// Local makes first, which differ by operation: the entries of Expected for
// all three; the ConditionalOperator, and the ConditionExpression or
// UpdateExpression, after it for PutItem and DeleteItem and before it for
// UpdateItem (the operator only). Each row is one request, its message by
// PutItem, DeleteItem and UpdateItem; the enum's own message is empty.
func Test_legacyWrite_returnValuesOnConditionCheckFailure(t *testing.T) {
	s, _ := auStack(t)

	const (
		bad    = `"ReturnValuesOnConditionCheckFailure":"X"`
		badExp = `"Expected":{"s":{"ComparisonOperator":"XX"}}`
		okExp  = `"Expected":{"s":{"Value":{"S":"hello"}}}`
		syntax = "Invalid ConditionExpression: Syntax error; token: \"<EOF>\", near: \"(\""
		names  = `"ExpressionAttributeNames":{"#a":"b"}`
		opOr   = `"ConditionalOperator":"OR"`
		badCE  = `"ConditionExpression":"attribute_exists("`
	)

	heads := map[string]string{
		opPut:        `{"TableName":"` + auTable + `","Item":{"id":{"S":"1"}}`,
		opDeleteItem: `{"TableName":"` + auTable + `",` + auKey,
		opUpdateItem: `{"TableName":"` + auTable + `",` + auKey,
	}

	for _, tt := range []struct {
		name  string
		extra []string
		want  [3]string
	}{
		{"nothing else", nil, [3]string{}},
		{"an Expected that is invalid", []string{badExp}, [3]string{msgBadCmp, msgBadCmp, msgBadCmp}},
		{"an Expected that holds", []string{okExp}, [3]string{}},
		{"an Expected that fails", []string{`"Expected":{"s":{"Value":{"S":"x"}}}`}, [3]string{}},
		{
			"an invalid Expected and a ConditionalOperator for one",
			[]string{badExp, opOr},
			[3]string{msgBadCmp, msgBadCmp, msgBadCmp},
		},
		{"a ConditionalOperator alone", []string{opOr}, [3]string{"", "", msgCoAlone}},
		{"a ConditionalOperator for one Expected", []string{okExp, opOr}, [3]string{"", "", msgCoSingle}},
		{"a ConditionExpression that does not parse", []string{badCE}, [3]string{"", "", syntax}},
		{"names no expression uses", []string{names}, [3]string{msgNamesOnly, msgNamesOnly, msgNamesOnly}},
	} {
		for i, op := range []string{opPut, opDeleteItem, opUpdateItem} {
			want := tt.want[i]
			if want == "" {
				want = msgRvocf
			}

			t.Run(op+" with "+tt.name, func(t *testing.T) {
				body := heads[op] + "," + bad
				if len(tt.extra) > 0 {
					body += "," + strings.Join(tt.extra, ",")
				}

				s.fails(t, op, body+"}", codeValidation, want)
			})
		}
	}

	// Before the enum: a PutItem's Expected mixed with an expression, and the
	// key of a DeleteItem or UpdateItem that is not the table's.
	s.fails(t, opPut, heads[opPut]+","+bad+","+okExp+`,"ConditionExpression":"attribute_exists(id)"}`, codeValidation,
		legacyMixed+"Expected} Expression parameters: {ConditionExpression}")
	s.fails(t, opUpdateItem, `{"TableName":"`+auTable+`","Key":{"id":{"N":"1"}},`+bad+"}", codeValidation,
		legacyInvalid+"Type mismatch for key")
	s.fails(t, opDeleteItem, `{"TableName":"`+auTable+`","Key":{"id":{"N":"1"}},`+bad+"}", codeValidation, msgRvocf)
}

// Test_legacyWrite_entriesBeforeOperator: the entries of Expected are checked
// before the ConditionalOperator that joins them.
func Test_legacyWrite_entriesBeforeOperator(t *testing.T) {
	s, _ := auStack(t)

	for _, op := range []string{opPut, opDeleteItem, opUpdateItem} {
		head := `{"TableName":"` + auTable + `",` + auKey
		if op == opPut {
			head = `{"TableName":"` + auTable + `","Item":{"id":{"S":"1"}}`
		}

		s.fails(t, op, head+`,"ConditionalOperator":"OR","Expected":{"s":{"ComparisonOperator":"XX"}}}`,
			codeValidation, msgBadCmp)
	}
}
