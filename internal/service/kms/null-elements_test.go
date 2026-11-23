package kms_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The LocalStack image answers CreateGrant with a null in Operations as a
// ValidationError, where an entry left out would be a grant that was asked for.
// KMS tells a null from an absent entry, so it decodes with awsjson.KeepNulls
// (serviceOptions.KeepNulls in the generator) and a null in Operations is read
// as the empty operation the enum check refuses.
func Test_a_null_grant_operation_is_refused(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{}))

	for _, ops := range [][]any{{nil}, {"Decrypt", nil}} {
		got := s.call(usEast1, "CreateGrant", map[string]any{
			fieldKeyID: id, "GranteePrincipal": "arn:aws:iam::000000000000:user/u", "Operations": ops,
		})
		assert.Equal(t, http.StatusBadRequest, got.status, "%v: %v", ops, got.body)
		assert.Equal(t, "ValidationError", got.errorCode(), "%v", ops)
		assert.Contains(t, got.message(), "at 'operations' failed to satisfy constraint", "%v", ops)
	}

	grants := s.ok("ListGrants", map[string]any{fieldKeyID: id})
	assert.Empty(t, grants["Grants"], "no grant was made")
}

// A null encryption context value is the empty string, as it was before nulls
// were dropped anywhere; the image answers 500 here, which is not copied
// (docs/compat.md, "Null elements").
func Test_a_null_encryption_context_value_is_the_empty_string(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{}))

	enc := s.ok("Encrypt", map[string]any{
		fieldKeyID: id, "Plaintext": "aGVsbG8=", fieldEncryptionContext: map[string]any{"k": nil},
	})
	blob, ok := enc["CiphertextBlob"].(string)
	require.True(t, ok)

	s.ok("Decrypt", map[string]any{"CiphertextBlob": blob, fieldEncryptionContext: map[string]any{"k": ""}})
}
