package sqs_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// policyOfLength is a json object of exactly n bytes: one statement padded
// with spaces, which any parser reads as the object it is.
func policyOfLength(n int) string {
	const head, tail = `{"Version":"2012-10-17","Statement":[`, `]}`

	return head + strings.Repeat(" ", n-len(head)-len(tail)) + tail
}

// redriveOfLength is a valid RedrivePolicy of exactly n bytes.
func redriveOfLength(n int) string {
	const head, tail = policyTarget + `"maxReceiveCount":5`, `}`

	return head + strings.Repeat(" ", n-len(head)-len(tail)) + tail
}

// Test_PolicyAttributes_AreRefusedPastTheQuota is SQ-320. A queue's Policy and
// RedrivePolicy are held to the SQS policy quotas' 8,192 bytes before either
// is parsed: a 16 KiB gzip body that held a policy of 16 MiB of {} took the
// process to 1,173 MiB, parsed again by every AddPermission (SQ-317), and a
// RedrivePolicy of {"x":[{},...]} to 665 MiB (SQ-322). A policy of exactly the
// quota is stored, in CreateQueue and in SetQueueAttributes.
func Test_PolicyAttributes_AreRefusedPastTheQuota(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, attr, code, message string
		value                     func(n int) string
	}{
		{"Policy", attrPolicy, "InvalidAttributeValue", "Invalid value for the parameter Policy.", policyOfLength},
		{"RedrivePolicy", attrRedrivePolicy, "", invalidPolicy, redriveOfLength},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			code := tt.code
			if code == "" {
				code = invalidParameterCode(protocol)
			}

			t.Run(protocol+" "+tt.name, func(t *testing.T) {
				t.Parallel()

				c := newStack(t).as(protocol)
				url := c.createQueue("set", nil)

				for _, n := range []int{8192, 8193, 1 << 20} {
					value := tt.value(n)
					res := c.call(opSetAttributes, map[string]any{
						paramQueueURL: url, paramAttributes: map[string]string{tt.attr: value},
					})

					stored := attributesOf(c, url)[tt.attr]

					if n == 8192 {
						require.True(t, res.ok(), "%d bytes: %s %s", n, res.errorCode(), res.errorMessage())
						assert.Equal(t, value, stored, "a policy of the quota is stored")

						continue
					}

					assert.Equal(t, 400, res.status, "%d bytes", n)
					assert.Equal(t, code, res.errorCode(), "%d bytes", n)
					assert.Equal(t, tt.message, res.errorMessage(), "%d bytes", n)
					assert.Equal(t, tt.value(8192), stored, "%d bytes: a refused policy is not stored", n)

					created := c.call("CreateQueue", map[string]any{
						paramQueueName: "created", paramAttributes: map[string]string{tt.attr: value},
					})
					assert.Equal(t, code, created.errorCode(), "CreateQueue, %d bytes", n)
				}
			})
		}
	}
}
