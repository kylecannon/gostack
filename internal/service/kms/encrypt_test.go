package kms_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fieldPlaintext         = "Plaintext"
	fieldCiphertextBlob    = "CiphertextBlob"
	fieldEncryptionContext = "EncryptionContext"
	fieldAlgorithm         = "EncryptionAlgorithm"
	fieldSourceKeyID       = "SourceKeyId"
	fieldDestinationKeyID  = "DestinationKeyId"

	usageMAC   = "GENERATE_VERIFY_MAC"
	eccP256    = "ECC_NIST_P256"
	oaepSHA256 = "RSAES_OAEP_SHA_256"

	codeInvalidCiphertext = "InvalidCiphertextException"
	codeIncorrectKey      = "IncorrectKeyException"
	codeInvalidKeyUsage   = "InvalidKeyUsageException"

	incorrectKeyMessage = "The key ID in the request does not identify a CMK that can perform this operation."

	// headerLen is LocalStack's HEADER_LEN: key id, IV and tag.
	headerLen = 36 + 16 + 16

	// vectorKeyID, vectorMaterial and vectorBlob are a blob LocalStack's own
	// code built at 8b9a79f (models.py _serialize_ciphertext_blob) for the
	// plaintext "0123456789abcdef", key 00 01 .. 1f and IV 64 65 .. 73, the
	// vector ciphertext/ciphertext_test.go also holds.
	vectorKeyID = "5d6f1c0e-8a43-4b3e-9a0b-1f7f3b2e9c41"
	vectorBlob  = "35643666316330652d386134332d346233652d396130622d3166376633623265396334316465666768696a6b6c" +
		"6d6e6f707172738fdc237cf52b25de3fa22aa515fb4670e8827162d02111ef8ac0cceb24b389034a6148d08cfff01bd590047e89e717d8"
)

func b64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// blobOf decodes a base64 member of a reply.
func blobOf(t *testing.T, body map[string]any, member string) []byte {
	t.Helper()

	text, ok := body[member].(string)
	require.True(t, ok, "%s is missing from %v", member, body)

	data, err := base64.StdEncoding.DecodeString(text)
	require.NoError(t, err)

	return data
}

// failsCiphertext sends a call that must fail with InvalidCiphertextException
// and no message member, as LocalStack and AWS answer it.
func (s *stack) failsCiphertext(operation string, input map[string]any) {
	s.t.Helper()

	r := s.call(usEast1, operation, input)
	require.Equal(s.t, http.StatusBadRequest, r.status, "%s: %v", operation, r.body)
	require.Equal(s.t, codeInvalidCiphertext, r.errorCode())
	require.NotContains(s.t, r.body, "message")
}

func (s *stack) encrypt(keyID string, plaintext []byte, encryptionContext map[string]string) []byte {
	s.t.Helper()

	input := map[string]any{fieldKeyID: keyID, fieldPlaintext: b64(plaintext)}
	if encryptionContext != nil {
		input[fieldEncryptionContext] = encryptionContext
	}

	return blobOf(s.t, s.ok("Encrypt", input), fieldCiphertextBlob)
}

func Test_EncryptDecrypt(t *testing.T) {
	tests := []struct {
		name        string
		givePlain   []byte
		giveContext map[string]string
		giveDecrypt map[string]string
	}{
		{
			name:        "no context",
			givePlain:   []byte("test message 123 !%$@ 1234567890"),
			giveContext: nil,
			giveDecrypt: nil,
		},
		{
			name:        "context of two pairs",
			givePlain:   []byte("a"),
			giveContext: map[string]string{"b": "2", "a": "1"},
			giveDecrypt: map[string]string{"a": "1", "b": "2"},
		},
		{
			name:        "a whole block, ignored context key left out",
			givePlain:   []byte("0123456789abcdef"),
			giveContext: map[string]string{"k": "v", "aws-crypto-public-key": "x"},
			giveDecrypt: map[string]string{"k": "v", "aws-crypto-public-key": "different"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStack(t)
			meta := s.createKey(nil)
			id := keyID(t, meta)

			blob := s.encrypt(id, tt.givePlain, tt.giveContext)

			// LocalStack's layout: the key id, padded to 36 bytes, IV, tag,
			// then the PKCS7-padded plaintext.
			require.Equal(t, id, string(blob[:36]))
			require.Len(t, blob, headerLen+(len(tt.givePlain)/16+1)*16)

			for _, keyIDs := range [][]any{{id}, {arn(t, meta)}, {nil}, {""}} {
				input := map[string]any{fieldCiphertextBlob: b64(blob), fieldAlgorithm: symmetricDefault}
				if keyIDs[0] != nil {
					input[fieldKeyID] = keyIDs[0]
				}

				if tt.giveDecrypt != nil {
					input[fieldEncryptionContext] = tt.giveDecrypt
				}

				out := s.ok("Decrypt", input)
				require.Equal(t, tt.givePlain, blobOf(t, out, fieldPlaintext))
				require.Equal(t, arn(t, meta), out[fieldKeyID])
				require.Equal(t, symmetricDefault, out[fieldAlgorithm])
			}

			require.Zero(t, s.changes.Load())
		})
	}
}

func Test_Encrypt_answer(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)

	out := s.ok("Encrypt", map[string]any{fieldKeyID: keyID(t, meta), fieldPlaintext: b64([]byte("x"))})
	require.Equal(t, arn(t, meta), out[fieldKeyID])
	require.NotContains(t, out, fieldAlgorithm, "the algorithm is echoed, never made up")
}

// Test_Decrypt_localStackBlob opens a blob LocalStack built, under a key with
// the same id and material.
func Test_Decrypt_localStackBlob(t *testing.T) {
	s := newStack(t)

	material := make([]byte, 32)
	for i := range material {
		material[i] = byte(i)
	}

	s.createKey(map[string]any{fieldTags: []map[string]any{
		{fieldTagKey: tagCustomID, fieldTagValue: vectorKeyID},
		{fieldTagKey: "_custom_key_material_", fieldTagValue: b64(material)},
	}})

	blob, err := hex.DecodeString(vectorBlob)
	require.NoError(t, err)

	out := s.ok("Decrypt", map[string]any{fieldCiphertextBlob: b64(blob)})
	require.Equal(t, "0123456789abcdef", string(blobOf(t, out, fieldPlaintext)))
}

func Test_Decrypt_errors(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	other := keyID(t, s.createKey(nil))
	hmac := keyID(t, s.createKey(map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: usageMAC}))
	context := map[string]string{"context-key": "context-value"}
	blob := s.encrypt(id, []byte("secret"), context)

	flipped := []byte(string(blob))
	flipped[len(flipped)-1] ^= 1

	t.Run("a missing or different context", func(*testing.T) {
		s.failsCiphertext("Decrypt", map[string]any{fieldKeyID: id, fieldCiphertextBlob: b64(blob)})
		s.failsCiphertext("Decrypt", map[string]any{
			fieldKeyID: id, fieldCiphertextBlob: b64(blob),
			fieldEncryptionContext: map[string]string{"context-key": "changed"},
		})
	})

	t.Run("a corrupted blob", func(*testing.T) {
		s.failsCiphertext("Decrypt", map[string]any{
			fieldCiphertextBlob: b64(flipped), fieldEncryptionContext: context,
		})
	})

	t.Run("a blob without its header names a key", func(*testing.T) {
		s.failsCiphertext("Decrypt", map[string]any{
			fieldKeyID: id, fieldCiphertextBlob: b64(blob[headerLen:]), fieldEncryptionContext: context,
		})
	})

	t.Run("a blob without its header and no key", func(*testing.T) {
		s.fails("Decrypt", map[string]any{fieldCiphertextBlob: b64(blob[:10])}, codeInvalidCiphertext,
			"LocalStack is unable to deserialize the ciphertext blob. Perhaps the blob didn't come from LocalStack")
	})

	t.Run("another key's blob", func(*testing.T) {
		s.fails("Decrypt", map[string]any{fieldKeyID: other, fieldCiphertextBlob: b64(blob)},
			codeIncorrectKey, incorrectKeyMessage)
		// The blob's key comes before the usage: LocalStack's re_encrypt test.
		s.fails("Decrypt", map[string]any{fieldKeyID: hmac, fieldCiphertextBlob: b64(blob)},
			codeIncorrectKey, incorrectKeyMessage)
	})

	t.Run("a key that does not exist", func(t *testing.T) {
		r := s.call(usEast1, "Decrypt", map[string]any{
			fieldKeyID: "6a1b2c3d-0000-4000-8000-000000000000", fieldCiphertextBlob: b64(blob),
		})
		require.Equal(t, "NotFoundException", r.errorCode())
	})

	t.Run("a disabled key, from the blob", func(t *testing.T) {
		s.ok("DisableKey", map[string]any{fieldKeyID: id})
		defer s.ok("EnableKey", map[string]any{fieldKeyID: id})

		r := s.call(usEast1, "Decrypt", map[string]any{fieldCiphertextBlob: b64(blob), fieldEncryptionContext: context})
		require.Equal(t, "DisabledException", r.errorCode())
	})

	t.Run("a Recipient whose document holds no key is ignored", func(t *testing.T) {
		r := s.call(usEast1, "Decrypt", map[string]any{
			fieldCiphertextBlob: b64(blob), fieldEncryptionContext: context,
			"Recipient": map[string]any{fieldAttestation: b64([]byte("doc"))},
		})
		require.Equal(t, http.StatusOK, r.status, "%v", r.body)
		require.Contains(t, r.body, fieldPlaintext)
	})
}

func Test_Encrypt_errors(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	rsaKey := keyID(t, s.createKey(map[string]any{fieldKeySpec: rsa2048, fieldKeyUsage: usageEncryptDecrypt}))
	signKey := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})

	s.fails("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64(make([]byte, 4097))}, codeValidation,
		"1 validation error detected: Value at 'plaintext' failed to satisfy constraint: "+
			"Member must have length less than or equal to 4096")
	s.fails("Encrypt", map[string]any{fieldKeyID: id}, codeValidation,
		"1 validation error detected: Value null at 'plaintext' failed to satisfy constraint: Member must not be null")
	s.fails("Encrypt", map[string]any{
		fieldKeyID: rsaKey, fieldPlaintext: b64(make([]byte, 215)), fieldAlgorithm: "RSAES_OAEP_SHA_1",
	}, codeValidation, "Algorithm RSAES_OAEP_SHA_1 and key spec RSA_2048 cannot encrypt data larger than 214 bytes.")
	s.fails("Encrypt", map[string]any{fieldKeyID: keyID(t, signKey), fieldPlaintext: b64([]byte("x"))},
		codeInvalidKeyUsage, arn(t, signKey)+" key usage is SIGN_VERIFY which is not valid for Encrypt.")

	s.ok("ScheduleKeyDeletion", map[string]any{fieldKeyID: id, "PendingWindowInDays": 7})

	r := s.call(usEast1, "Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64(make([]byte, 5000))})
	require.Equal(t, "KMSInvalidStateException", r.errorCode(), "the key's state comes before the size")
}

// Test_Encrypt_rsaKey holds LocalStack's quirk: an RSA key encrypts with the
// symmetric scheme, and its blob decrypts.
func Test_Encrypt_rsaKey(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{fieldKeySpec: rsa2048, fieldKeyUsage: usageEncryptDecrypt}))

	out := s.ok("Encrypt", map[string]any{
		fieldKeyID: id, fieldPlaintext: b64([]byte("rsa")), fieldAlgorithm: oaepSHA256,
	})
	blob := blobOf(t, out, fieldCiphertextBlob)
	require.Equal(t, id, string(blob[:36]))
	require.Equal(t, oaepSHA256, out[fieldAlgorithm])

	plain := s.ok("Decrypt", map[string]any{
		fieldKeyID: id, fieldCiphertextBlob: b64(blob), fieldAlgorithm: oaepSHA256,
	})
	require.Equal(t, "rsa", string(blobOf(t, plain, fieldPlaintext)))
}

// Test_Encrypt_customDERKey holds the answer for an RSA key made from custom
// DER material, whose symmetric material is that DER: LocalStack's AES fails
// with a 500, and gostack answers KMSInvalidStateException.
func Test_Encrypt_customDERKey(t *testing.T) {
	s := newStack(t)

	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)

	meta := s.createKey(map[string]any{
		fieldKeySpec: rsa2048, fieldKeyUsage: usageEncryptDecrypt,
		fieldTags: []map[string]string{customMaterialTag(der)},
	})

	r := s.call(usEast1, "Encrypt", map[string]any{fieldKeyID: keyID(t, meta), fieldPlaintext: b64([]byte("x"))})
	require.Equal(t, http.StatusBadRequest, r.status)
	require.Equal(t, "KMSInvalidStateException", r.errorCode())

	r = s.call(usEast1, "GenerateDataKey", map[string]any{fieldKeyID: keyID(t, meta)})
	require.Equal(t, "KMSInvalidStateException", r.errorCode())
}

func Test_ReEncrypt(t *testing.T) {
	s := newStack(t)
	source := s.createKey(nil)
	destination := s.createKey(nil)
	sourceContext := map[string]string{"from": "1"}
	destinationContext := map[string]string{"to": "2"}
	blob := s.encrypt(keyID(t, source), []byte("move me"), sourceContext)

	for _, give := range []any{keyID(t, source), nil} {
		input := map[string]any{
			fieldCiphertextBlob: b64(blob), fieldDestinationKeyID: keyID(t, destination),
			"SourceEncryptionContext": sourceContext, "DestinationEncryptionContext": destinationContext,
			"SourceEncryptionAlgorithm": symmetricDefault, "DestinationEncryptionAlgorithm": symmetricDefault,
		}
		if give != nil {
			input[fieldSourceKeyID] = give
		}

		out := s.ok("ReEncrypt", input)
		require.Equal(t, arn(t, source), out[fieldSourceKeyID])
		require.Equal(t, arn(t, destination), out[fieldKeyID])
		require.Equal(t, symmetricDefault, out["SourceEncryptionAlgorithm"])
		require.Equal(t, symmetricDefault, out["DestinationEncryptionAlgorithm"])

		moved := blobOf(t, out, fieldCiphertextBlob)
		require.Equal(t, keyID(t, destination), string(moved[:36]))

		plain := s.ok("Decrypt", map[string]any{
			fieldCiphertextBlob: b64(moved), fieldEncryptionContext: destinationContext,
		})
		require.Equal(t, "move me", string(blobOf(t, plain, fieldPlaintext)))
	}

	hmac := keyID(t, s.createKey(map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: usageMAC}))
	s.fails("ReEncrypt", map[string]any{
		fieldCiphertextBlob: b64(blob), fieldSourceKeyID: hmac, fieldDestinationKeyID: hmac,
	}, codeIncorrectKey, incorrectKeyMessage)

	signKey := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})
	s.fails("ReEncrypt", map[string]any{
		fieldCiphertextBlob: b64(blob), fieldSourceKeyID: keyID(t, source), fieldDestinationKeyID: keyID(t, signKey),
		"SourceEncryptionContext": sourceContext,
	}, codeInvalidKeyUsage, arn(t, signKey)+" key usage is SIGN_VERIFY which is not valid for ReEncrypt.")

	s.failsCiphertext("ReEncrypt", map[string]any{
		fieldCiphertextBlob: b64(blob), fieldSourceKeyID: keyID(t, source), fieldDestinationKeyID: keyID(t, destination),
	})

	require.Zero(t, s.changes.Load())
	require.True(t, strings.HasPrefix(arn(t, source), "arn:aws:kms:"))
}
