package sqs_test

import (
	"strconv"
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

// Test_AddPermission_IsRefusedPastThePolicyQuota is SQ-320. The Policy that
// AddPermission writes is held to the same 8,192 bytes as one that
// SetQueueAttributes sets. The botocore SQS model lists OverLimit among
// AddPermission's errors ("the maximum number of permissions for the queue is
// reached"), so a statement that would take the policy past the quota is
// refused with it and the policy is left as it was.
func Test_AddPermission_IsRefusedPastThePolicyQuota(t *testing.T) {
	t.Parallel()

	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()

			c := newStack(t).as(protocol)
			url := c.createQueue("grants", nil)

			accounts := []string{"111111111111", "222222222222", "333333333333"}
			actions := []string{opSend, opReceiveMessage, opDeleteMessage, opGetQueueAttributes}

			var refused bool

			for i := range 100 {
				before := attributesOf(c, url)[attrPolicy]
				res := c.call("AddPermission", map[string]any{
					paramQueueURL: url, paramLabel: "grant-" + strconv.Itoa(i),
					paramAccountIDs: accounts, paramActions: actions,
				})

				if res.ok() {
					assert.LessOrEqual(t, len(attributesOf(c, url)[attrPolicy]), 8192, "grant %d", i)

					continue
				}

				assert.Equal(t, 400, res.status)
				assert.Equal(t, "OverLimit", res.errorCode(), res.errorMessage())
				assert.Equal(t, before, attributesOf(c, url)[attrPolicy], "a refused grant leaves the policy")
				assert.Positive(t, i, "grants within the quota are stored")

				refused = true

				break
			}

			assert.True(t, refused, "a grant past the quota is refused")
		})
	}
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
