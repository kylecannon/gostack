package sqs_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dlqARN          = "arn:aws:sqs:us-east-1:000000000000:dlq"
	missingTarget   = "The required parameter 'deadLetterTargetArn' is missing"
	missingCount    = "The required parameter 'maxReceiveCount' is missing"
	invalidPolicy   = "Invalid value for the parameter RedrivePolicy."
	countRangeTail  = ", valid values are from 1 to 1000 both inclusive."
	countRangeHead  = " for parameter RedrivePolicy is invalid. Reason: Invalid value for maxReceiveCount: "
	policyTarget    = `{"deadLetterTargetArn":"` + dlqARN + `",`
	allowPermission = `{"redrivePermission":"allowAll"}`
)

// Test_RedrivePolicy_is_checked_as_LocalStack_checks_it is SQ-67. Every row was
// sent to LocalStack 8b9a79f as SetQueueAttributes (verification/SQ-145/
// ls-probe.out). The messages are LocalStack's. Rows LocalStack answers with a
// 500 answer a 400 InvalidParameterValue here, and no refused policy is stored.
func Test_RedrivePolicy_is_checked_as_LocalStack_checks_it(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		givePolicy  string
		wantMessage string // empty: accepted
	}{
		{"an integer count", policyTarget + `"maxReceiveCount":5}`, ""},
		{"the largest count", policyTarget + `"maxReceiveCount":1000}`, ""},
		{"a string count", policyTarget + `"maxReceiveCount":"5"}`, ""},
		{"a padded string count", policyTarget + `"maxReceiveCount":" 5"}`, ""},
		{"an underscore string count", policyTarget + `"maxReceiveCount":"1_0"}`, ""},
		{"a whole float count", policyTarget + `"maxReceiveCount":5.0}`, ""},
		{"a fractional count", policyTarget + `"maxReceiveCount":5.9}`, ""},
		{"a true count", policyTarget + `"maxReceiveCount":true}`, ""},
		{"a target that is not text", `{"deadLetterTargetArn":5,"maxReceiveCount":5}`, ""},
		{"a target that is no queue", `{"deadLetterTargetArn":"dummy","maxReceiveCount":5}`, ""},
		{
			"a zero count", policyTarget + `"maxReceiveCount":0}`,
			"Value " + policyTarget + `"maxReceiveCount":0}` + countRangeHead + "0" + countRangeTail,
		},
		{
			"a count past the limit", policyTarget + `"maxReceiveCount":1001}`,
			"Value " + policyTarget + `"maxReceiveCount":1001}` + countRangeHead + "1001" + countRangeTail,
		},
		{
			"a negative count", policyTarget + `"maxReceiveCount":-1}`,
			"Value " + policyTarget + `"maxReceiveCount":-1}` + countRangeHead + "-1" + countRangeTail,
		},
		{
			"a count that is a word", policyTarget + `"maxReceiveCount":"abc"}`,
			"Value " + policyTarget + `"maxReceiveCount":"abc"}` + countRangeHead + "abc" + countRangeTail,
		},
		{"a null count", policyTarget + `"maxReceiveCount":null}`, missingCount},
		{"a missing count", strings.TrimSuffix(policyTarget, ",") + "}", missingCount},
		{"a missing target", `{"maxReceiveCount":5}`, missingTarget},
		{"an empty target", `{"deadLetterTargetArn":"","maxReceiveCount":5}`, missingTarget},
		{"an empty object", `{}`, missingTarget},
		// LocalStack answers these with a 500 and keeps the policy.
		{"text that is not json", "not a policy", invalidPolicy},
		{"a json array", `[]`, invalidPolicy},
		{"a json string", `"x"`, invalidPolicy},
		{"a json null", `null`, invalidPolicy},
		{
			"a count that is a list", policyTarget + `"maxReceiveCount":[1]}`,
			"Value " + policyTarget + `"maxReceiveCount":[1]}` + countRangeHead + "invalid" + countRangeTail,
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+" "+tt.name, func(t *testing.T) {
				t.Parallel()

				c := newStack(t).as(protocol)
				queueURL := c.createQueue("src", nil)

				res := c.call(opSetAttributes, map[string]any{
					paramQueueURL: queueURL, paramAttributes: map[string]string{attrRedrivePolicy: tt.givePolicy},
				})

				_, stored := attributesOf(c, queueURL)[attrRedrivePolicy]

				if tt.wantMessage == "" {
					require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())
					assert.True(t, stored)

					return
				}

				assert.Equal(t, 400, res.status)
				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, tt.wantMessage, res.errorMessage())
				assert.False(t, stored, "a policy that is refused is not stored")
			})
		}
	}
}

// Test_RedriveAllowPolicy_is_stored_unchecked is SQ-67. LocalStack 8b9a79f takes
// any text as a RedriveAllowPolicy, in CreateQueue and in SetQueueAttributes,
// and so does gostack. The messages gostack answered with before were its own.
func Test_RedriveAllowPolicy_is_stored_unchecked(t *testing.T) {
	t.Parallel()

	policies := map[string]string{
		"allowAll":          allowPermission,
		"byQueue":           `{"redrivePermission":"byQueue","sourceQueueArns":["` + dlqARN + `"]}`,
		"byQueue no queues": `{"redrivePermission":"byQueue"}`,
		"an unknown value":  `{"redrivePermission":"sometimes"}`,
		"no permission":     `{"sourceQueueArns":["x"]}`,
		"eleven queues":     `{"redrivePermission":"byQueue","sourceQueueArns":[` + strings.Repeat(`"q",`, 10) + `"q"]}`,
		"plain text":        "not a policy",
		"an array":          `[]`,
	}

	for _, protocol := range protocols {
		for name, policy := range policies {
			t.Run(protocol+" "+name, func(t *testing.T) {
				t.Parallel()

				c := newStack(t).as(protocol)

				created := c.createQueue("created", map[string]string{attrRedriveAllowPolicy: policy})
				assert.Equal(t, policy, attributesOf(c, created)[attrRedriveAllowPolicy])

				set := c.createQueue("set", nil)
				res := c.call(opSetAttributes, map[string]any{
					paramQueueURL: set, paramAttributes: map[string]string{attrRedriveAllowPolicy: policy},
				})

				require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())
				assert.Equal(t, policy, attributesOf(c, set)[attrRedriveAllowPolicy])
			})
		}
	}
}
