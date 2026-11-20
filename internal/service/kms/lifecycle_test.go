package kms_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_DescribeKey_errors(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	id := keyID(t, meta)

	const missing = "134f2428-cec1-4b25-a1ae-9048164dba47"

	tests := []struct {
		name        string
		giveRegion  string
		giveKeyID   string
		wantMessage string
	}{
		{"an id that is not a key id", usEast1, "fake-key-id", "Invalid keyId 'fake-key-id'"},
		{
			"a UUID without dashes", usEast1, "134f2428cec14b25a1ae9048164dba47",
			"Invalid keyId '134f2428cec14b25a1ae9048164dba47'",
		},
		{"an mrk- id that is not hex", usEast1, "mrk-fake-key-id", "Invalid keyId 'mrk-fake-key-id'"},
		{
			"a missing key", usEast1, missing,
			"Key 'arn:aws:kms:us-east-1:" + account + ":key/" + missing + "' does not exist",
		},
		{
			"a missing key by ARN", usEast1, strings.Replace(arn(t, meta), id, missing, 1),
			"Key 'arn:aws:kms:us-east-1:" + account + ":key/" + missing + "' does not exist",
		},
		{
			"a key of another region by id", usWest2, id,
			"Key 'arn:aws:kms:us-west-2:" + account + ":key/" + id + "' does not exist",
		},
		{"a key of another region by ARN", usWest2, arn(t, meta), "Invalid arn us-east-1"},
		{
			"an alias that does not exist", usEast1, "alias/nothing",
			"Unable to find KMS alias with name alias/nothing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.call(tt.giveRegion, "DescribeKey", map[string]any{fieldKeyID: tt.giveKeyID})
			require.Equal(t, http.StatusBadRequest, r.status)
			require.Equal(t, "NotFoundException", r.errorCode())
			require.Equal(t, tt.wantMessage, r.message())
		})
	}
}

func Test_keyStates(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	key := map[string]any{fieldKeyID: id}

	state := func() (string, any) {
		meta, ok := s.ok("DescribeKey", key)["KeyMetadata"].(map[string]any)
		require.True(t, ok)

		st, _ := meta["KeyState"].(string)

		return st, meta["Enabled"]
	}

	s.ok("DisableKey", key)
	st, enabled := state()
	require.Equal(t, "Disabled", st)
	require.Equal(t, false, enabled)

	s.ok("EnableKey", key)
	st, enabled = state()
	require.Equal(t, "Enabled", st)
	require.Equal(t, true, enabled)

	s.fails("CancelKeyDeletion", key, "KMSInvalidStateException",
		"arn:aws:kms:us-east-1:"+account+":key/"+id+" is enabled, but the operation doesn't support such a state")
	s.fails("ScheduleKeyDeletion", map[string]any{fieldKeyID: id, "PendingWindowInDays": 6}, codeValidation,
		"PendingWindowInDays should be between 7 and 30, but it is 6")

	scheduled := s.ok("ScheduleKeyDeletion", key)
	require.Equal(t, id, scheduled[fieldKeyID])
	require.Equal(t, "PendingDeletion", scheduled["KeyState"])
	require.InDelta(t, 30, scheduled["PendingWindowInDays"], 0)
	require.NotNil(t, scheduled["DeletionDate"])

	s.fails("ScheduleKeyDeletion", key, "KMSInvalidStateException",
		"arn:aws:kms:us-east-1:"+account+":key/"+id+" is pending deletion.")
	s.fails("EnableKey", key, "KMSInvalidStateException",
		"arn:aws:kms:us-east-1:"+account+":key/"+id+" is pending deletion.")

	// Any state is described.
	st, enabled = state()
	require.Equal(t, "PendingDeletion", st)
	require.Equal(t, false, enabled)

	cancelled := s.ok("CancelKeyDeletion", key)
	require.Equal(t, "arn:aws:kms:us-east-1:"+account+":key/"+id, cancelled[fieldKeyID])

	meta, ok := s.ok("DescribeKey", key)["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Disabled", meta["KeyState"])
	require.NotContains(t, meta, "DeletionDate")

	s.ok("UpdateKeyDescription", map[string]any{fieldKeyID: id, fieldDescription: valueNew})
	meta, ok = s.ok("DescribeKey", key)["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, valueNew, meta[fieldDescription])
}

func Test_ListKeys(t *testing.T) {
	s := newStack(t)

	const keys = 5

	ids := make([]any, 0, keys)
	for range keys {
		ids = append(ids, keyID(t, s.createKey(nil)))
	}

	all := s.ok("ListKeys", nil)
	require.Equal(t, false, all["Truncated"])
	require.NotContains(t, all, "NextMarker")

	first := s.ok("ListKeys", map[string]any{"Limit": 2})
	require.Equal(t, true, first["Truncated"])
	require.Equal(t, ids[2], first["NextMarker"])

	rest := s.ok("ListKeys", map[string]any{"Marker": first["NextMarker"], "Limit": 10})
	require.Equal(t, false, rest["Truncated"])

	got := make([]any, 0, len(ids))

	for _, page := range []map[string]any{first, rest} {
		entries, ok := page["Keys"].([]any)
		require.True(t, ok)

		for _, e := range entries {
			entry, ok := e.(map[string]any)
			require.True(t, ok)
			got = append(got, entry[fieldKeyID])
		}
	}

	require.Equal(t, ids, got)
}

func Test_tags(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{fieldTags: []map[string]string{
		{fieldTagKey: tag1, fieldTagValue: value1},
		{fieldTagKey: tag2, fieldTagValue: "value2"},
		{fieldTagKey: tag3, fieldTagValue: "value3"},
	}}))
	key := map[string]any{fieldKeyID: id}

	s.ok("TagResource", map[string]any{fieldKeyID: id, fieldTags: []map[string]string{
		{fieldTagKey: tag4, fieldTagValue: "value4"}, {fieldTagKey: tag2, fieldTagValue: "updated"},
	}})

	listed := s.ok("ListResourceTags", key)
	require.Equal(t, false, listed["Truncated"])
	require.Equal(t, []any{
		map[string]any{fieldTagKey: tag1, fieldTagValue: value1},
		map[string]any{fieldTagKey: tag2, fieldTagValue: "updated"},
		map[string]any{fieldTagKey: tag3, fieldTagValue: "value3"},
		map[string]any{fieldTagKey: tag4, fieldTagValue: "value4"},
	}, listed[fieldTags])

	s.ok("UntagResource", map[string]any{fieldKeyID: id, "TagKeys": []string{tag1, tag2, tag3, tag4}})
	require.Equal(t, []any{}, s.ok("ListResourceTags", key)[fieldTags])

	s.fails("TagResource", map[string]any{fieldKeyID: id, fieldTags: []map[string]string{
		{fieldTagKey: tag1, fieldTagValue: value1}, {fieldTagKey: tag1, fieldTagValue: "another"},
	}}, codeTag, "Duplicate tag keys")
	s.fails("TagResource", map[string]any{fieldKeyID: id, fieldTags: []map[string]string{
		{fieldTagKey: "aws:key1", fieldTagValue: value1},
	}}, codeTag, "Tags beginning with aws: are reserved")
}

func Test_aliases(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	first := keyID(t, meta)
	second := keyID(t, s.createKey(nil))

	s.fails("CreateAlias", map[string]any{fieldAliasName: "no-prefix", fieldTargetKeyID: first}, codeValidation,
		`Alias must start with the prefix "alias/". Please see `+
			`https://docs.aws.amazon.com/kms/latest/developerguide/kms-alias.html`)

	s.ok("CreateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: first})
	s.fails("CreateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: second}, "AlreadyExistsException",
		"An alias with the name arn:aws:kms:us-east-1:"+account+":alias/a already exists")

	described, ok := s.ok("DescribeKey", map[string]any{
		fieldKeyID: "arn:aws:kms:us-east-1:" + account + ":alias/a",
	})["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, first, described[fieldKeyID])

	aliasesOf := func(id string) []any {
		aliases, found := s.ok("ListAliases", map[string]any{fieldKeyID: id})["Aliases"].([]any)
		require.True(t, found)

		return aliases
	}

	require.Len(t, aliasesOf(first), 1)
	require.Empty(t, aliasesOf(second))

	s.ok("UpdateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: arn(t, meta)})
	s.ok("UpdateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: second})
	require.Empty(t, aliasesOf(first))

	entry, ok := aliasesOf(second)[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "arn:aws:kms:us-east-1:"+account+":alias/a", entry["AliasArn"])
	require.Equal(t, second, entry[fieldTargetKeyID])
	require.Contains(t, entry, "LastUpdatedDate")

	s.fails("UpdateAlias", map[string]any{fieldAliasName: "alias/none", fieldTargetKeyID: second}, "NotFoundException",
		"Alias arn:aws:kms:us-east-1:"+account+":alias/none is not found.")

	s.ok("DeleteAlias", map[string]any{fieldAliasName: aliasA})
	s.fails("DeleteAlias", map[string]any{fieldAliasName: aliasA}, "NotFoundException",
		"Alias arn:aws:kms:us-east-1:"+account+":alias/a is not found")
	require.Equal(t, []any{}, s.ok("ListAliases", nil)["Aliases"])
}

func Test_reservedAlias(t *testing.T) {
	s := newStack(t)

	meta, ok := s.ok("DescribeKey", map[string]any{fieldKeyID: aliasS3})["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, int32(1), s.changes.Load(), "a read that makes a reserved alias reports the change")

	again, ok := s.ok("DescribeKey", map[string]any{fieldKeyID: aliasS3})["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, meta[fieldKeyID], again[fieldKeyID])
	require.Equal(t, int32(1), s.changes.Load())
}

func Test_policies(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))

	got := s.ok("GetKeyPolicy", map[string]any{fieldKeyID: id, fieldPolicyName: policyDefault})
	policy, ok := got[fieldPolicy].(string)
	require.True(t, ok)
	require.JSONEq(t, `{"Version": "2012-10-17", "Id": "key-default-1", "Statement": [{"Sid": "Enable IAM User `+
		`Permissions", "Effect": "Allow", "Principal": {"AWS": "arn:aws:iam::`+account+`:root"}, "Action": "kms:*", `+
		`"Resource": "*"}]}`, policy)
	// The text is json.dumps's, with a space after each separator.
	require.Contains(t, policy, `{"Version": "2012-10-17", "Id": "key-default-1", "Statement": [{"Sid": `)

	s.ok("PutKeyPolicy", map[string]any{fieldKeyID: id, fieldPolicyName: policyDefault, fieldPolicy: `{"x": 1}`})

	stored, ok := s.ok("GetKeyPolicy", map[string]any{fieldKeyID: id})[fieldPolicy].(string)
	require.True(t, ok)
	require.Contains(t, stored, `{"x": 1}`)
	require.Len(t, stored, len(`{"x": 1}`), "the policy is kept as it was sent")

	listed := s.ok("ListKeyPolicies", map[string]any{fieldKeyID: id})
	require.Equal(t, []any{policyDefault}, listed["PolicyNames"])
	require.Equal(t, false, listed["Truncated"])

	s.fails("GetKeyPolicy", map[string]any{fieldKeyID: id, fieldPolicyName: "other"},
		"NotFoundException", "No such policy exists")
	s.fails("PutKeyPolicy", map[string]any{fieldKeyID: id, fieldPolicyName: "other", fieldPolicy: "{}"},
		"UnsupportedOperationException", "Only default policy is supported")
}

func Test_GenerateRandom(t *testing.T) {
	s := newStack(t)

	for _, n := range []int{1, 12, 1024} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			plaintext, ok := s.ok("GenerateRandom", map[string]any{fieldNumberOfBytes: n})["Plaintext"].(string)
			require.True(t, ok)
			require.Len(t, plaintext, (n+2)/3*4)
		})
	}

	s.fails("GenerateRandom", map[string]any{}, codeValidation, "NumberOfBytes is required.")
	s.fails("GenerateRandom", map[string]any{fieldNumberOfBytes: 0}, codeValidation,
		"1 validation error detected: Value '0' at 'numberOfBytes' failed to satisfy constraint: "+
			"Member must have value greater than or equal to 1")
	s.fails("GenerateRandom", map[string]any{fieldNumberOfBytes: 1025}, codeValidation,
		"1 validation error detected: Value '1025' at 'numberOfBytes' failed to satisfy constraint: "+
			"Member must have value less than or equal to 1024")
}

func Test_unimplemented(t *testing.T) {
	s := newStack(t)

	r := s.call(usEast1, "ConnectCustomKeyStore", map[string]any{"CustomKeyStoreId": "cks-1"})
	require.Equal(t, http.StatusNotImplemented, r.status)
}
