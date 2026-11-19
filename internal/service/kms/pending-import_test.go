package kms_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_pendingImport holds a key made with Origin EXTERNAL, which has no
// material until one is imported, to AWS's key-state table (Key states of AWS
// KMS keys, the Pending import column): EnableKey, DisableKey and
// CancelKeyDeletion fail with a KMSInvalidStateException, and the key stays
// pending import. Before, each answered 200 and left it Enabled or Disabled
// with no material, so the pending-import guard of a data operation never fired.
func Test_pendingImport(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(map[string]any{fieldOrigin: "EXTERNAL"})
	id := keyID(t, meta)
	key := map[string]any{fieldKeyID: id}
	wantMessage := arn(t, meta) + " is pending import."

	state := func() any {
		described, ok := s.ok("DescribeKey", key)["KeyMetadata"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, false, described["Enabled"])

		return described["KeyState"]
	}

	for _, op := range []string{"EnableKey", "DisableKey", "CancelKeyDeletion"} {
		t.Run(op+" is refused", func(t *testing.T) {
			s.fails(op, key, "KMSInvalidStateException", wantMessage)
			require.Equal(t, "PendingImport", state())
		})
	}

	// The calls that edit a key without using it pass, as the table has them.
	t.Run("the calls that edit the key pass", func(t *testing.T) {
		s.ok("UpdateKeyDescription", map[string]any{fieldKeyID: id, fieldDescription: "waiting"})
		s.ok("TagResource", map[string]any{fieldKeyID: id, fieldTags: []map[string]string{
			{fieldTagKey: tag1, fieldTagValue: value1},
		}})
		s.ok("UntagResource", map[string]any{fieldKeyID: id, "TagKeys": []string{tag1}})
		s.ok("PutKeyPolicy", map[string]any{fieldKeyID: id, fieldPolicy: "{}"})
		s.ok("GetKeyPolicy", key)
		s.ok("ListKeyPolicies", key)
		s.ok("ListResourceTags", key)
		s.ok("CreateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: id})
		s.ok("UpdateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: id})
		require.Equal(t, "PendingImport", state())
	})

	t.Run("ScheduleKeyDeletion moves it", func(t *testing.T) {
		scheduled := s.ok("ScheduleKeyDeletion", key)
		require.Equal(t, "PendingDeletion", scheduled["KeyState"])
		require.Equal(t, "PendingDeletion", state())
	})
}
