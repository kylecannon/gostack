package filter_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Policy_Match_Decisions covers the messages LocalStack's matcher fails
// on with an exception, which drops the delivery there. Here a value an
// operator cannot test does not match, and the other values still count.
func Test_Policy_Match_Decisions(t *testing.T) {
	array := func(s string) map[string]filter.Attribute {
		return map[string]filter.Attribute{"k": {DataType: typeStringArray, StringValue: s}}
	}
	tests := []struct {
		name      string
		givePol   string
		giveScope filter.Scope
		giveBody  string
		giveAttrs map[string]filter.Attribute
		want      bool
	}{
		{"prefix/number", `{"a": [{"prefix": "x"}]}`, filter.ScopeMessageBody, `{"a": 1}`, nil, false},
		{"prefix/number-then-match", `{"a": [{"prefix": "x"}]}`, filter.ScopeMessageBody, `{"a": [1, "x", 2]}`, nil, true},
		{
			"equals-ignore-case/bool", `{"a": [{"equals-ignore-case": "X"}]}`, filter.ScopeMessageBody,
			`{"a": true}`, nil, false,
		},
		{"string-array/scalar", `{"k": [{"exists": true}]}`, filter.ScopeMessageAttributes, "", array("5"), false},
		{"string-array/empty", `{"k": [{"exists": true}]}`, filter.ScopeMessageAttributes, "", array(""), false},
		{
			"string-array/numeric-object", `{"k": [{"numeric": [">=", 10]}]}`, filter.ScopeMessageAttributes, "",
			array(`[5, true, {"a": 1}, [1]]`), false,
		},
		{
			"string-array/suffix-mixed", `{"k": [{"suffix": "yz"}]}`, filter.ScopeMessageAttributes, "",
			array(`[12, "abcxyz"]`), true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := filter.Parse(tt.givePol, tt.giveScope)
			require.NoError(t, err)
			if tt.giveScope == filter.ScopeMessageBody {
				assert.Equal(t, tt.want, p.MatchBody(filter.ParseBody(tt.giveBody)))
				return
			}
			assert.Equal(t, tt.want, p.MatchAttributes(tt.giveAttrs))
		})
	}
}
