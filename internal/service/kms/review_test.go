package kms_test

import (
	"maps"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opGenerateDataKey     = "GenerateDataKey"
	opGenerateDataKeyPair = "GenerateDataKeyPair"
	opDecrypt             = "Decrypt"
	opReEncrypt           = "ReEncrypt"
	fieldRecipient        = "Recipient"
	fieldPrivateKey       = "PrivateKeyPlaintext"
)

// recipient is a RecipientInfo no operation can honour yet.
func recipient() map[string]any {
	return map[string]any{"AttestationDocument": b64([]byte("junk"))}
}

// Test_Recipient_isNotImplemented holds the ruling Decrypt already follows: a
// call that carries a Recipient answers 501 and never the plaintext, which AWS
// leaves out when a recipient is given.
func Test_Recipient_isNotImplemented(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	blob := blobOf(t, s.ok("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("x"))}),
		fieldCiphertextBlob)

	calls := []struct {
		operation string
		input     map[string]any
	}{
		{opGenerateDataKey, map[string]any{fieldKeyID: id}},
		{opGenerateDataKey, map[string]any{fieldKeyID: id, fieldNumberOfBytes: 16}},
		{opGenerateDataKeyPair, map[string]any{fieldKeyID: id, fieldKeyPairSpec: eccP256}},
		{"GenerateRandom", map[string]any{fieldNumberOfBytes: 32}},
		{opDecrypt, map[string]any{fieldCiphertextBlob: b64(blob)}},
	}

	for _, c := range calls {
		t.Run(c.operation, func(t *testing.T) {
			input := map[string]any{fieldRecipient: recipient()}
			maps.Copy(input, c.input)

			r := s.call(usEast1, c.operation, input)
			require.Equal(t, http.StatusNotImplemented, r.status, "%v", r.body)
			require.NotContains(t, r.body, fieldPlaintext)
			require.NotContains(t, r.body, fieldPrivateKey)
		})
	}

	t.Run("it comes before the key is looked up", func(t *testing.T) {
		for _, operation := range []string{opGenerateDataKey, opGenerateDataKeyPair} {
			r := s.call(usEast1, operation, map[string]any{
				fieldKeyID: "6a1b2c3d-0000-4000-8000-000000000000", fieldKeyPairSpec: eccP256,
				fieldRecipient: recipient(),
			})
			require.Equal(t, http.StatusNotImplemented, r.status, "%s: %v", operation, r.body)
		}
	})

	t.Run("without a Recipient the calls still answer", func(t *testing.T) {
		require.Contains(t, s.ok(opGenerateDataKey, map[string]any{fieldKeyID: id}), fieldPlaintext)
		require.Contains(t, s.ok(opGenerateDataKeyPair, map[string]any{fieldKeyID: id, fieldKeyPairSpec: eccP256}),
			fieldPrivateKey)
		require.Contains(t, s.ok("GenerateRandom", map[string]any{fieldNumberOfBytes: 8}), fieldPlaintext)
	})

	// The WithoutPlaintext operations have no Recipient member: the image
	// ignores the unknown member, and so does the decoder here.
	t.Run("the WithoutPlaintext operations have no Recipient", func(t *testing.T) {
		out := s.ok("GenerateDataKeyWithoutPlaintext", map[string]any{fieldKeyID: id, fieldRecipient: recipient()})
		require.NotContains(t, out, fieldPlaintext)

		out = s.ok("GenerateDataKeyPairWithoutPlaintext", map[string]any{
			fieldKeyID: id, fieldKeyPairSpec: eccP256, fieldRecipient: recipient(),
		})
		require.NotContains(t, out, fieldPrivateKey)
	})
}

// forgedBlob is a blob in LocalStack's layout whose id field is name, padded
// with NULs to the 36 bytes, so the key id Parse reads is name.
func forgedBlob(name string) []byte {
	blob := make([]byte, headerLen+16)
	copy(blob, name)

	return blob
}

// Test_Decrypt_forgedBlobMakesNothing holds that a blob naming an AWS managed
// alias is a blob naming no key: it must not make the alias and its key.
func Test_Decrypt_forgedBlobMakesNothing(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	blob := blobOf(t, s.ok("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("x"))}),
		fieldCiphertextBlob)

	aliases := s.ok("ListAliases", nil)
	keys := s.ok("ListKeys", nil)
	changes := s.changes.Load()

	for _, name := range []string{"alias/aws/ssm", aliasS3, "alias/mine"} {
		forged := b64(forgedBlob(name))

		for _, c := range []struct {
			operation string
			input     map[string]any
		}{
			{opDecrypt, map[string]any{fieldCiphertextBlob: forged}},
			{opReEncrypt, map[string]any{fieldCiphertextBlob: forged, fieldDestinationKeyID: id}},
		} {
			t.Run(c.operation+" "+name, func(t *testing.T) {
				r := s.call(usEast1, c.operation, c.input)
				require.Equal(t, http.StatusBadRequest, r.status, "%v", r.body)
				require.Equal(t, "NotFoundException", r.errorCode())
			})
		}
	}

	t.Run("a real blob still names its key", func(t *testing.T) {
		out := s.ok(opDecrypt, map[string]any{fieldCiphertextBlob: b64(blob)})
		require.Equal(t, b64([]byte("x")), out[fieldPlaintext])
	})

	require.Equal(t, aliases, s.ok("ListAliases", nil), "a forged blob made an alias")
	require.Equal(t, keys, s.ok("ListKeys", nil), "a forged blob made a key")
	require.Equal(t, changes, s.changes.Load(), "a forged blob marked the service changed")
}
