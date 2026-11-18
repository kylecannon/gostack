package sqs_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_AddPermission_keeps_a_single_statement_object is the code-review finding
// on AddPermission: a Policy whose Statement is one object, which IAM allows,
// lost that statement when a permission was added.
func Test_AddPermission_keeps_a_single_statement_object(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			single := `{"Version":"2012-10-17","Statement":{"Sid":"existing","Effect":"Allow",` +
				`"Principal":"*","Action":"sqs:SendMessage","Resource":"*"}}`
			require.True(t, c.call("SetQueueAttributes", map[string]any{
				paramQueueURL: url, paramAttributes: map[string]string{attrPolicy: single},
			}).ok())

			res := c.call("AddPermission", map[string]any{
				paramQueueURL: url, paramLabel: "added", paramAccountIDs: []string{"111111111111"},
				paramActions: []string{"ReceiveMessage"},
			})
			require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

			got := c.call(opGetQueueAttributes, map[string]any{
				paramQueueURL: url, paramAttributeNames: []string{attrPolicy},
			})
			require.True(t, got.ok())

			var doc struct {
				Statement []map[string]any `json:"Statement"`
			}

			require.NoError(t, json.Unmarshal([]byte(got.attributes()[attrPolicy]), &doc))
			require.Len(t, doc.Statement, 2, "the existing statement stays")
			assert.Equal(t, "existing", doc.Statement[0]["Sid"])
			assert.Equal(t, "added", doc.Statement[1]["Sid"])

			// And it can still be removed by its label.
			require.True(t, c.call("RemovePermission", map[string]any{paramQueueURL: url, paramLabel: "existing"}).ok())
		})
	}
}
