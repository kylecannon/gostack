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
		{"a UUID without dashes", usEast1, "134f2428cec14b25a1ae9048164dba47", "Invalid keyId '134f2428cec14b25a1ae9048164dba47'"},
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
			r := s.call(tt.giveRegion, "DescribeKey", map[string]any{"KeyId": tt.giveKeyID})
			require.Equal(t, http.StatusBadRequest, r.status)
			require.Equal(t, "NotFoundException", r.errorCode())
			require.Equal(t, tt.wantMessage, r.message())
		})
	}
}

func Test_keyStates(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	key := map[string]any{"KeyId": id}

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
	s.fails("ScheduleKeyDeletion", map[string]any{"KeyId": id, "PendingWindowInDays": 6}, "ValidationException",
		"PendingWindowInDays should be between 7 and 30, but it is 6")

	scheduled := s.ok("ScheduleKeyDeletion", key)
	require.Equal(t, id, scheduled["KeyId"])
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
	require.Equal(t, "arn:aws:kms:us-east-1:"+account+":key/"+id, cancelled["KeyId"])

	meta, ok := s.ok("DescribeKey", key)["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Disabled", meta["KeyState"])
	require.NotContains(t, meta, "DeletionDate")

	s.ok("UpdateKeyDescription", map[string]any{"KeyId": id, "Description": "new"})
	meta, ok = s.ok("DescribeKey", key)["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "new", meta["Description"])
}

func Test_ListKeys(t *testing.T) {
	s := newStack(t)

	var ids []any
	for range 5 {
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
			got = append(got, entry["KeyId"])
		}
	}

	require.Equal(t, ids, got)
}

func Test_tags(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{"Tags": []map[string]string{
		{"TagKey": "tag1", "TagValue": "value1"}, {"TagKey": "tag2", "TagValue": "value2"}, {"TagKey": "tag3", "TagValue": "value3"},
	}}))
	key := map[string]any{"KeyId": id}

	s.ok("TagResource", map[string]any{"KeyId": id, "Tags": []map[string]string{
		{"TagKey": "tag4", "TagValue": "value4"}, {"TagKey": "tag2", "TagValue": "updated"},
	}})

	listed := s.ok("ListResourceTags", key)
	require.Equal(t, false, listed["Truncated"])
	require.Equal(t, []any{
		map[string]any{"TagKey": "tag1", "TagValue": "value1"},
		map[string]any{"TagKey": "tag2", "TagValue": "updated"},
		map[string]any{"TagKey": "tag3", "TagValue": "value3"},
		map[string]any{"TagKey": "tag4", "TagValue": "value4"},
	}, listed["Tags"])

	s.ok("UntagResource", map[string]any{"KeyId": id, "TagKeys": []string{"tag1", "tag2", "tag3", "tag4"}})
	require.Equal(t, []any{}, s.ok("ListResourceTags", key)["Tags"])

	s.fails("TagResource", map[string]any{"KeyId": id, "Tags": []map[string]string{
		{"TagKey": "tag1", "TagValue": "value1"}, {"TagKey": "tag1", "TagValue": "another"},
	}}, "TagException", "Duplicate tag keys")
	s.fails("TagResource", map[string]any{"KeyId": id, "Tags": []map[string]string{
		{"TagKey": "aws:key1", "TagValue": "value1"},
	}}, "TagException", "Tags beginning with aws: are reserved")
}

func Test_aliases(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	first := keyID(t, meta)
	second := keyID(t, s.createKey(nil))

	s.fails("CreateAlias", map[string]any{"AliasName": "no-prefix", "TargetKeyId": first}, "ValidationException",
		`Alias must start with the prefix "alias/". Please see https://docs.aws.amazon.com/kms/latest/developerguide/kms-alias.html`)

	s.ok("CreateAlias", map[string]any{"AliasName": "alias/a", "TargetKeyId": first})
	s.fails("CreateAlias", map[string]any{"AliasName": "alias/a", "TargetKeyId": second}, "AlreadyExistsException",
		"An alias with the name arn:aws:kms:us-east-1:"+account+":alias/a already exists")

	described, ok := s.ok("DescribeKey", map[string]any{
		"KeyId": "arn:aws:kms:us-east-1:" + account + ":alias/a",
	})["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, first, described["KeyId"])

	aliasesOf := func(id string) []any {
		aliases, ok := s.ok("ListAliases", map[string]any{"KeyId": id})["Aliases"].([]any)
		require.True(t, ok)

		return aliases
	}

	require.Len(t, aliasesOf(first), 1)
	require.Empty(t, aliasesOf(second))

	s.ok("UpdateAlias", map[string]any{"AliasName": "alias/a", "TargetKeyId": arn(t, meta)})
	s.ok("UpdateAlias", map[string]any{"AliasName": "alias/a", "TargetKeyId": second})
	require.Empty(t, aliasesOf(first))

	entry, ok := aliasesOf(second)[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "arn:aws:kms:us-east-1:"+account+":alias/a", entry["AliasArn"])
	require.Equal(t, second, entry["TargetKeyId"])
	require.Contains(t, entry, "LastUpdatedDate")

	s.fails("UpdateAlias", map[string]any{"AliasName": "alias/none", "TargetKeyId": second}, "NotFoundException",
		"Alias arn:aws:kms:us-east-1:"+account+":alias/none is not found.")

	s.ok("DeleteAlias", map[string]any{"AliasName": "alias/a"})
	s.fails("DeleteAlias", map[string]any{"AliasName": "alias/a"}, "NotFoundException",
		"Alias arn:aws:kms:us-east-1:"+account+":alias/a is not found")
	require.Equal(t, []any{}, s.ok("ListAliases", nil)["Aliases"])
}

func Test_reservedAlias(t *testing.T) {
	s := newStack(t)

	meta, ok := s.ok("DescribeKey", map[string]any{"KeyId": "alias/aws/s3"})["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, int32(1), s.changes.Load(), "a read that makes a reserved alias reports the change")

	again, ok := s.ok("DescribeKey", map[string]any{"KeyId": "alias/aws/s3"})["KeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, meta["KeyId"], again["KeyId"])
	require.Equal(t, int32(1), s.changes.Load())
}

func Test_policies(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))

	policy := s.ok("GetKeyPolicy", map[string]any{"KeyId": id, "PolicyName": "default"})["Policy"]
	require.Equal(t, `{"Version": "2012-10-17", "Id": "key-default-1", "Statement": [{"Sid": "Enable IAM User `+
		`Permissions", "Effect": "Allow", "Principal": {"AWS": "arn:aws:iam::`+account+`:root"}, "Action": "kms:*", `+
		`"Resource": "*"}]}`, policy)

	s.ok("PutKeyPolicy", map[string]any{"KeyId": id, "PolicyName": "default", "Policy": `{"x": 1}`})
	require.Equal(t, `{"x": 1}`, s.ok("GetKeyPolicy", map[string]any{"KeyId": id})["Policy"])

	listed := s.ok("ListKeyPolicies", map[string]any{"KeyId": id})
	require.Equal(t, []any{"default"}, listed["PolicyNames"])
	require.Equal(t, false, listed["Truncated"])

	s.fails("GetKeyPolicy", map[string]any{"KeyId": id, "PolicyName": "other"}, "NotFoundException", "No such policy exists")
	s.fails("PutKeyPolicy", map[string]any{"KeyId": id, "PolicyName": "other", "Policy": "{}"},
		"UnsupportedOperationException", "Only default policy is supported")
}

func Test_GenerateRandom(t *testing.T) {
	s := newStack(t)

	for _, n := range []int{1, 12, 1024} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			plaintext, ok := s.ok("GenerateRandom", map[string]any{"NumberOfBytes": n})["Plaintext"].(string)
			require.True(t, ok)
			require.Len(t, plaintext, (n+2)/3*4)
		})
	}

	s.fails("GenerateRandom", map[string]any{}, "ValidationException", "NumberOfBytes is required.")
	s.fails("GenerateRandom", map[string]any{"NumberOfBytes": 0}, "ValidationException",
		"1 validation error detected: Value '0' at 'numberOfBytes' failed to satisfy constraint: "+
			"Member must have value greater than or equal to 1")
	s.fails("GenerateRandom", map[string]any{"NumberOfBytes": 1025}, "ValidationException",
		"1 validation error detected: Value '1025' at 'numberOfBytes' failed to satisfy constraint: "+
			"Member must have value less than or equal to 1024")
}

func Test_unimplemented(t *testing.T) {
	s := newStack(t)

	r := s.call(usEast1, "Encrypt", map[string]any{"KeyId": "alias/x", "Plaintext": "AA=="})
	require.Equal(t, http.StatusNotImplemented, r.status)
}
