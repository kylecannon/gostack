package kms_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opRotate         = "RotateKeyOnDemand"
	opRotationStatus = "GetKeyRotationStatus"
	opEnableRotation = "EnableKeyRotation"
	opReplicate      = "ReplicateKey"
	opDescribe       = "DescribeKey"

	fieldMaterialID     = "CurrentKeyMaterialId"
	fieldRotationOn     = "KeyRotationEnabled"
	fieldRotationPeriod = "RotationPeriodInDays"
	fieldNextRotation   = "NextRotationDate"
	fieldReplicaRegion  = "ReplicaRegion"
	fieldMultiRegion    = "MultiRegion"
	fieldMRConfig       = "MultiRegionConfiguration"

	fieldRegion        = "Region"
	fieldPendingWindow = "PendingWindowInDays"
	originExternal     = "EXTERNAL"

	codeUnsupported = "UnsupportedOperationException"
)

// decrypted decrypts blob in region with no key id, so the key is found
// through the blob, and returns the plaintext.
func (s *stack) decrypted(region string, blob []byte) []byte {
	s.t.Helper()

	r := s.call(region, opDecrypt, map[string]any{fieldCiphertextBlob: b64(blob)})
	require.Equal(s.t, http.StatusOK, r.status, "%v", r.body)

	plain, ok := r.body[fieldPlaintext].(string)
	require.True(s.t, ok)

	out, err := base64.StdEncoding.DecodeString(plain)
	require.NoError(s.t, err)

	return out
}

// object is v as a JSON object.
func object(t *testing.T, v any) map[string]any {
	t.Helper()

	m, ok := v.(map[string]any)
	require.True(t, ok, "%v", v)

	return m
}

func (s *stack) materialID(id string) string {
	s.t.Helper()

	meta, ok := s.ok(opDescribe, map[string]any{fieldKeyID: id})[fieldKeyMetadata].(map[string]any)
	require.True(s.t, ok)

	material, _ := meta[fieldMaterialID].(string)

	return material
}

// Test_RotateKeyOnDemand_decryptsEveryGeneration seals one plaintext under each
// material a key has had and decrypts every blob after one and after three
// rotations: the blob does not name its material, so Decrypt tries them all.
func Test_RotateKeyOnDemand_decryptsEveryGeneration(t *testing.T) {
	for _, rotations := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d rotations", rotations), func(t *testing.T) {
			s := newStack(t)
			meta := s.createKey(nil)
			id := keyID(t, meta)

			blobs := [][]byte{s.encrypt(id, []byte("generation 0"), nil)}
			ids := []string{s.materialID(id)}

			for i := 1; i <= rotations; i++ {
				out := s.ok(opRotate, map[string]any{fieldKeyID: id})
				require.Equal(t, arn(t, meta), out[fieldKeyID])

				blobs = append(blobs, s.encrypt(id, fmt.Appendf(nil, "generation %d", i), nil))
				ids = append(ids, s.materialID(id))
			}

			for i, blob := range blobs {
				require.Equal(t, fmt.Sprintf("generation %d", i), string(s.decrypted(usEast1, blob)))
			}

			for i := 1; i < len(ids); i++ {
				require.Len(t, ids[i], 64)
				require.NotEqual(t, ids[i-1], ids[i])
			}
		})
	}
}

func Test_RotateKeyOnDemand_errors(t *testing.T) {
	s := newStack(t)

	t.Run("limit of ten", func(t *testing.T) {
		meta := s.createKey(nil)
		for range 10 {
			s.ok(opRotate, map[string]any{fieldKeyID: keyID(t, meta)})
		}

		s.fails(opRotate, map[string]any{fieldKeyID: keyID(t, meta)}, "LimitExceededException",
			"The on-demand rotations limit has been reached for the given keyId. "+
				"No more on-demand rotations can be performed for this key: "+arn(t, meta))
	})

	t.Run("not symmetric", func(t *testing.T) {
		meta := s.createKey(map[string]any{fieldKeyUsage: usageEncryptDecrypt, fieldKeySpec: rsa2048})
		s.fails(opRotate, map[string]any{fieldKeyID: keyID(t, meta)}, codeUnsupported, "")
	})

	t.Run("disabled before spec", func(t *testing.T) {
		meta := s.createKey(map[string]any{fieldKeyUsage: usageEncryptDecrypt, fieldKeySpec: rsa2048})
		s.ok("DisableKey", map[string]any{fieldKeyID: keyID(t, meta)})
		s.fails(opRotate, map[string]any{fieldKeyID: keyID(t, meta)}, "DisabledException", arn(t, meta)+" is disabled.")
	})

	t.Run("missing", func(_ *testing.T) {
		missing := "1234abcd-12ab-34cd-56ef-1234567890ab"
		s.fails(opRotate, map[string]any{fieldKeyID: missing}, "NotFoundException",
			"Key 'arn:aws:kms:us-east-1:"+account+":key/"+missing+"' does not exist")
	})

	t.Run("external pending import", func(t *testing.T) {
		meta := s.createKey(map[string]any{fieldOrigin: originExternal})
		s.fails(opRotate, map[string]any{fieldKeyID: keyID(t, meta)}, "KMSInvalidStateException",
			arn(t, meta)+" is pending import.")
	})
}

func Test_KeyRotationStatus(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	id := keyID(t, meta)

	status := s.ok(opRotationStatus, map[string]any{fieldKeyID: id})
	require.Equal(t, map[string]any{fieldKeyID: arn(t, meta), fieldRotationOn: false}, status)

	s.ok(opEnableRotation, map[string]any{fieldKeyID: id, fieldRotationPeriod: 90})
	status = s.ok(opRotationStatus, map[string]any{fieldKeyID: id})
	require.Equal(t, true, status[fieldRotationOn])
	require.InDelta(t, 90, status[fieldRotationPeriod], 0)
	require.Contains(t, status, fieldNextRotation)

	// A rotation on demand leaves the automatic schedule as it was.
	s.ok(opRotate, map[string]any{fieldKeyID: id})
	require.Equal(t, status, s.ok(opRotationStatus, map[string]any{fieldKeyID: id}))

	s.ok("DisableKeyRotation", map[string]any{fieldKeyID: id})
	status = s.ok(opRotationStatus, map[string]any{fieldKeyID: id})
	require.Equal(t, false, status[fieldRotationOn])
	require.NotContains(t, status, fieldRotationPeriod)

	t.Run("default period", func(t *testing.T) {
		other := keyID(t, s.createKey(nil))
		s.ok(opEnableRotation, map[string]any{fieldKeyID: other})
		require.InDelta(t, 365, s.ok(opRotationStatus, map[string]any{fieldKeyID: other})[fieldRotationPeriod], 0)
	})

	t.Run("keys that cannot rotate", func(t *testing.T) {
		rsaKey := s.createKey(map[string]any{fieldKeyUsage: usageEncryptDecrypt, fieldKeySpec: rsa2048})
		s.fails(opEnableRotation, map[string]any{fieldKeyID: keyID(t, rsaKey)}, codeUnsupported, "")

		hmacKey := s.createKey(map[string]any{fieldKeyUsage: usageGenerateMAC, fieldKeySpec: hmac256})
		s.fails("DisableKeyRotation", map[string]any{fieldKeyID: keyID(t, hmacKey)}, codeUnsupported, "")

		external := s.createKey(map[string]any{fieldOrigin: originExternal})
		s.fails(opEnableRotation, map[string]any{fieldKeyID: keyID(t, external)}, codeUnsupported,
			arn(t, external)+" origin is EXTERNAL which is not valid for this operation.")
	})

	t.Run("period out of range", func(_ *testing.T) {
		s.fails(opEnableRotation, map[string]any{fieldKeyID: id, fieldRotationPeriod: 89}, codeValidation,
			"1 validation error detected: Value '89' at 'rotationPeriodInDays' failed to satisfy constraint: "+
				"Member must have value greater than or equal to 90")
	})
}

// Test_crossAccount_rotation reaches another account's key by its ARN for the
// calls LocalStack parses an ARN for, and not for EnableKeyRotation.
func Test_crossAccount_rotation(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)

	r := s.callAsOther(opRotationStatus, map[string]any{fieldKeyID: arn(t, meta)})
	require.Equal(t, http.StatusOK, r.status, "%v", r.body)
	require.Equal(t, arn(t, meta), r.body[fieldKeyID])

	before := s.materialID(keyID(t, meta))
	r = s.callAsOther(opRotate, map[string]any{fieldKeyID: arn(t, meta)})
	require.Equal(t, http.StatusOK, r.status, "%v", r.body)
	require.NotEqual(t, before, s.materialID(keyID(t, meta)))

	r = s.callAsOther(opEnableRotation, map[string]any{fieldKeyID: arn(t, meta)})
	require.Equal(t, http.StatusBadRequest, r.status, "%v", r.body)
	require.Equal(t, "NotFoundException", r.errorCode())
}

func Test_ReplicateKey(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(map[string]any{fieldMultiRegion: true, fieldDescription: "primary"})
	id := keyID(t, meta)
	blob := s.encrypt(id, []byte("from the primary"), nil)

	out := s.ok(opReplicate, map[string]any{
		fieldKeyID: id, fieldReplicaRegion: usWest2, fieldDescription: "replica",
		fieldTags: []map[string]any{{fieldTagKey: tag1, fieldTagValue: value1}},
	})

	replicaARN := "arn:aws:kms:us-west-2:" + account + ":key/" + id
	replicaMeta, ok := out["ReplicaKeyMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, replicaARN, replicaMeta[fieldARN])
	require.Equal(t, "replica", replicaMeta[fieldDescription])
	require.NotContains(t, replicaMeta, fieldMaterialID)
	require.Equal(t, map[string]any{
		"MultiRegionKeyType": "REPLICA",
		"PrimaryKey":         map[string]any{fieldARN: arn(t, meta), fieldRegion: usEast1},
		"ReplicaKeys":        []any{map[string]any{fieldARN: replicaARN, fieldRegion: usWest2}},
	}, replicaMeta[fieldMRConfig])
	require.Contains(t, out["ReplicaPolicy"], `"Id": "key-default-1"`)
	require.Equal(t, []any{map[string]any{fieldTagKey: tag1, fieldTagValue: value1}}, out["ReplicaTags"])

	primaryMeta := object(t, s.ok(opDescribe, map[string]any{fieldKeyID: id})[fieldKeyMetadata])
	require.Equal(t, "PRIMARY", object(t, primaryMeta[fieldMRConfig])["MultiRegionKeyType"])
	require.Equal(t, []any{map[string]any{fieldARN: replicaARN, fieldRegion: usWest2}},
		object(t, primaryMeta[fieldMRConfig])["ReplicaKeys"])

	described := s.call(usWest2, opDescribe, map[string]any{fieldKeyID: id})
	require.Equal(t, http.StatusOK, described.status)
	require.Equal(t, s.materialID(id), object(t, described.body[fieldKeyMetadata])[fieldMaterialID])

	t.Run("replica decrypts the primary's ciphertext and back", func(t *testing.T) {
		require.Equal(t, "from the primary", string(s.decrypted(usWest2, blob)))

		r := s.call(usWest2, opEncrypt, map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("from the replica"))})
		require.Equal(t, http.StatusOK, r.status, "%v", r.body)
		sealedBlob, ok := r.body[fieldCiphertextBlob].(string)
		require.True(t, ok)
		back, err := base64.StdEncoding.DecodeString(sealedBlob)
		require.NoError(t, err)
		require.Equal(t, "from the replica", string(s.decrypted(usEast1, back)))
	})

	t.Run("errors", func(t *testing.T) {
		r := s.call(usWest2, opReplicate, map[string]any{fieldKeyID: id, fieldReplicaRegion: usWest2})
		require.Equal(t, codeUnsupported, r.errorCode())
		require.Equal(t, replicaARN+" is not a multi-region primary key.", r.message())

		s.fails(opReplicate, map[string]any{fieldKeyID: id, fieldReplicaRegion: usWest2}, "AlreadyExistsException",
			"Unable to replicate key "+id+" to region us-west-2, as the key already exist there")

		single := keyID(t, s.createKey(nil))
		s.fails(opReplicate, map[string]any{fieldKeyID: single, fieldReplicaRegion: usWest2}, codeUnsupported,
			"Unable to replicate a non-MultiRegion key "+single)

		s.fails(opReplicate, map[string]any{fieldKeyID: id}, codeValidation,
			"1 validation error detected: Value null at 'replicaRegion' failed to satisfy constraint: "+
				"Member must not be null")
	})

	t.Run("deleting the replica leaves the primary", func(t *testing.T) {
		r := s.call(usWest2, "ScheduleKeyDeletion", map[string]any{fieldKeyID: id, fieldPendingWindow: 7})
		require.Equal(t, http.StatusOK, r.status, "%v", r.body)

		primary, ok := s.ok(opDescribe, map[string]any{fieldKeyID: id})[fieldKeyMetadata].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "Enabled", primary["KeyState"])
		require.Equal(t, "from the primary", string(s.decrypted(usEast1, blob)))
	})
}
