package filter_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

// The attribute data types the tests send.
const (
	typeString      = "String"
	typeStringArray = "String.Array"
)

// FuzzMatch feeds (policy, message) pairs through Parse and both matchers.
// Parse never panics and refuses only with *filter.Error; a policy it accepts
// never makes either matcher panic, whatever the message. The message is the
// body, and also the attributes: a JSON object of name to {DataType,
// StringValue} when it is one, otherwise its text under a few names and types.
// The seed corpus in testdata/fuzz/FuzzMatch holds the policies and messages
// of testdata/localstack-filter.json, LocalStack's test_sns_filter_policy.py
// rows among them.
func FuzzMatch(f *testing.F) {
	f.Add(`{"object": {"key": [{"prefix": "auto-"}, "hardcodedvalue"]}}`, true, `{"object": {"key": "auto-test"}}`)
	f.Add(`{"attr1": [{"numeric": [">", 0, "<=", 100]}]}`, false, `{"attr1": {"DataType": "Number", "StringValue": "99"}}`)
	f.Fuzz(func(t *testing.T, policy string, body bool, message string) {
		scope := filter.ScopeMessageAttributes
		if body {
			scope = filter.ScopeMessageBody
		}
		p, err := filter.Parse(policy, scope)
		if err != nil {
			if !errors.As(err, new(*filter.Error)) {
				t.Fatalf("Parse(%q) = %T %v, want *filter.Error", policy, err, err)
			}
			return
		}
		checkErr := filter.CheckJSON(policy)
		if checkErr != nil {
			t.Fatalf("CheckJSON(%q) refused what Parse accepted: %v", policy, checkErr)
		}
		p.MatchBody(filter.ParseBody(message))
		var attrs map[string]filter.Attribute
		if json.Unmarshal([]byte(message), &attrs) != nil {
			attrs = map[string]filter.Attribute{
				message: {DataType: typeString, StringValue: message},
				"key":   {DataType: typeStringArray, StringValue: message},
				"num":   {DataType: "Number", StringValue: message},
			}
		}
		p.MatchAttributes(attrs)
	})
}
