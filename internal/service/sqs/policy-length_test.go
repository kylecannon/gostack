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

// Test_PermissionWrites_KeepThePolicyQuota is SQ-320. Every write of the
// Policy that AddPermission and RemovePermission make is held to the 8,192
// bytes, and is counted as the policy is written back, which escapes no more
// than json must:
//   - a statement whose Resource holds U+2028 three bytes at a time is written
//     back as six ( , which encoding/json escapes always), so removing
//     another statement would store a longer policy than was set. botocore's
//     SQS model lists no error for RemovePermission that names a limit, so it
//     is refused with OverLimit, as AddPermission is, and the policy is left;
//   - a policy of 1,573 bytes with 1,400 & takes another statement: escaped
//     as & for HTML, the & alone would have taken it past the quota.
func Test_PermissionWrites_KeepThePolicyQuota(t *testing.T) {
	t.Parallel()

	statement := func(sid, resource string) string {
		return `{"Sid":"` + sid + `","Effect":"Allow","Principal":"*","Action":"SQS:SendMessage","Resource":"` +
			resource + `"}`
	}
	document := func(statements ...string) string {
		return `{"Version":"2012-10-17","Statement":[` + strings.Join(statements, ",") + `]}`
	}

	for _, protocol := range protocols {
		t.Run(protocol+" RemovePermission", func(t *testing.T) {
			t.Parallel()

			c := newStack(t).as(protocol)
			url := c.createQueue("remove", nil)

			policy := document(statement("wide", strings.Repeat(" ", 2000)), statement("gone", "*"))
			require.LessOrEqual(t, len(policy), 8192)

			set := c.call(opSetAttributes, map[string]any{
				paramQueueURL: url, paramAttributes: map[string]string{attrPolicy: policy},
			})
			require.True(t, set.ok(), "%s %s", set.errorCode(), set.errorMessage())

			res := c.call("RemovePermission", map[string]any{paramQueueURL: url, paramLabel: "gone"})

			assert.Equal(t, 400, res.status)
			assert.Equal(t, "OverLimit", res.errorCode(), res.errorMessage())
			assert.Equal(t, "The Policy attribute would be longer than 8192 bytes.", res.errorMessage())
			assert.Equal(t, policy, attributesOf(c, url)[attrPolicy], "a refused removal leaves the policy")
		})

		t.Run(protocol+" AddPermission", func(t *testing.T) {
			t.Parallel()

			c := newStack(t).as(protocol)
			url := c.createQueue("amp", nil)

			policy := document(statement("amp", strings.Repeat("&", 1400)))
			policy = policy[:len(policy)-1] + strings.Repeat(" ", 1573-len(policy)) + "}"
			require.Len(t, policy, 1573)

			set := c.call(opSetAttributes, map[string]any{
				paramQueueURL: url, paramAttributes: map[string]string{attrPolicy: policy},
			})
			require.True(t, set.ok(), "%s %s", set.errorCode(), set.errorMessage())

			res := c.call("AddPermission", map[string]any{
				paramQueueURL: url, paramLabel: "more",
				paramAccountIDs: []string{"111111111111"}, paramActions: []string{opSend},
			})
			require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

			stored := attributesOf(c, url)[attrPolicy]
			assert.LessOrEqual(t, len(stored), 8192)
			assert.Equal(t, 1400, strings.Count(stored, "&"), "the & are written as they are")
			assert.Contains(t, stored, `"Sid":"more"`)
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
