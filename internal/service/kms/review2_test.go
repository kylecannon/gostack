package kms_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opSign              = "Sign"
	opVerify            = "Verify"
	opGetPublicKey      = "GetPublicKey"
	opDerive            = "DeriveSharedSecret"
	fieldKeyMetadata    = "KeyMetadata"
	fieldAgreementAlgs  = "KeyAgreementAlgorithms"
	fieldEncryptionAlgs = "EncryptionAlgorithms"
	missingKeyID        = "6a1b2c3d-0000-4000-8000-000000000000"
	aliasSSM            = "alias/aws/ssm"

	messageTooShort = "1 validation error detected: Value at 'message' failed to satisfy constraint: " +
		"Member must have length greater than or equal to 1"
	messageTooLong = "1 validation error detected: Value at 'message' failed to satisfy constraint: " +
		"Member must have length less than or equal to 4096"
)

// Test_SignVerify_messageBounds holds the model's PlaintextType bounds on
// Message (1 to 4096 bytes) and CiphertextType's minimum on Signature, checked
// before the key is looked up, as AWS's validation runs first.
func Test_SignVerify_messageBounds(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify}))

	sign := func(keyID string, message []byte, messageType string) map[string]any {
		return map[string]any{
			fieldKeyID: keyID, fieldSigningAlgorithm: ecdsaSHA256, fieldMessageType: messageType, fieldMessage: b64(message),
		}
	}
	verify := func(keyID string, message []byte, messageType string, signature []byte) map[string]any {
		input := sign(keyID, message, messageType)
		input[fieldSignature] = b64(signature)

		return input
	}

	largest := []byte(strings.Repeat("x", 4096))
	signature := blobOf(t, s.ok(opSign, sign(id, largest, typeRaw)), fieldSignature)
	s.ok(opVerify, verify(id, largest, typeRaw, signature))

	changes := s.changes.Load()
	aliases := s.ok("ListAliases", nil)

	tooLong := []byte(strings.Repeat("x", 4097))

	for _, keyID := range []string{id, missingKeyID, aliasSSM} {
		for _, messageType := range []string{typeRaw, typeDigest} {
			s.fails(opSign, sign(keyID, tooLong, messageType), codeValidation, messageTooLong)
			s.fails(opSign, sign(keyID, []byte{}, messageType), codeValidation, messageTooShort)
			s.fails(opVerify, verify(keyID, tooLong, messageType, signature), codeValidation, messageTooLong)
			s.fails(opVerify, verify(keyID, []byte{}, messageType, signature), codeValidation, messageTooShort)
		}

		s.fails(opVerify, verify(keyID, signMessage, typeRaw, []byte{}), codeValidation,
			"1 validation error detected: Value at 'signature' failed to satisfy constraint: "+
				"Member must have length greater than or equal to 1")
		s.fails(opVerify, verify(keyID, []byte{}, typeRaw, []byte{}), codeValidation,
			"2 validation errors detected: Value at 'message' failed to satisfy constraint: "+
				"Member must have length greater than or equal to 1; Value at 'signature' failed to satisfy "+
				"constraint: Member must have length greater than or equal to 1")
	}

	require.Equal(t, aliases, s.ok("ListAliases", nil), "a refused Sign or Verify made an alias")
	require.Equal(t, changes, s.changes.Load())
}

// Test_Decrypt_recipientBeforeLookup holds that a Recipient answers 501 before
// a key is looked up, so it neither finds a missing key nor makes a managed
// alias, and that a null Recipient is no Recipient.
func Test_Decrypt_recipientBeforeLookup(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	blob := blobOf(t, s.ok("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("x"))}),
		fieldCiphertextBlob)

	aliases := s.ok("ListAliases", nil)
	keys := s.ok("ListKeys", nil)
	changes := s.changes.Load()

	for _, input := range []map[string]any{
		{fieldKeyID: missingKeyID, fieldCiphertextBlob: b64(blob), fieldRecipient: map[string]any{}},
		{fieldKeyID: aliasSSM, fieldCiphertextBlob: b64([]byte("AAAA")), fieldRecipient: map[string]any{}},
		{fieldCiphertextBlob: b64(forgedBlob(aliasSSM)), fieldRecipient: recipient()},
	} {
		r := s.call(usEast1, opDecrypt, input)
		require.Equal(t, http.StatusNotImplemented, r.status, "%v: %v", input, r.body)
	}

	require.Equal(t, aliases, s.ok("ListAliases", nil), "a Decrypt with a Recipient made an alias")
	require.Equal(t, keys, s.ok("ListKeys", nil), "a Decrypt with a Recipient made a key")
	require.Equal(t, changes, s.changes.Load())

	out := s.ok(opDecrypt, map[string]any{fieldCiphertextBlob: b64(blob), fieldRecipient: nil})
	require.Equal(t, b64([]byte("x")), out[fieldPlaintext])
}

// spki is a SubjectPublicKeyInfo with the algorithm kept as it came.
type spki struct {
	Algorithm asn1.RawValue
	PublicKey asn1.BitString
}

// The point forms withPoint writes.
const (
	formCompressed        = "compressed"
	formHybrid            = "hybrid"
	formHybridWrongParity = "hybrid with the wrong parity"
)

// withPoint is der, a SubjectPublicKeyInfo of an uncompressed point, with the
// point rewritten by form: its compressed or hybrid encoding (SEC 1, 2.3.3),
// or a hybrid one whose parity bit does not match y.
func withPoint(t *testing.T, der []byte, form string) []byte {
	t.Helper()

	var info spki

	rest, err := asn1.Unmarshal(der, &info)
	require.NoError(t, err)
	require.Empty(t, rest)

	point := info.PublicKey.Bytes
	require.Equal(t, byte(4), point[0])

	size := (len(point) - 1) / 2
	odd := point[len(point)-1] & 1

	var out []byte

	switch form {
	case formCompressed:
		out = append([]byte{2 | odd}, point[1:1+size]...)
	case formHybrid:
		out = append([]byte{6 | odd}, point[1:]...)
	case formHybridWrongParity:
		out = append([]byte{6 | odd ^ 1}, point[1:]...)
	default:
		t.Fatalf("no form %q", form)
	}

	info.PublicKey = asn1.BitString{Bytes: out, BitLength: len(out) * 8}

	encoded, err := asn1.Marshal(info)
	require.NoError(t, err)

	return encoded
}

// Test_DeriveSharedSecret_pointForms holds that a peer key's point may be
// compressed or hybrid on every curve, as OpenSSL reads it for LocalStack, and
// derives the secret the uncompressed point gives.
func Test_DeriveSharedSecret_pointForms(t *testing.T) {
	s := newStack(t)

	for _, spec := range []string{eccP256, eccP384, eccP521, secgK1} {
		t.Run(spec, func(t *testing.T) {
			own := keyID(t, s.createKey(map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageKeyAgreement}))
			peer := s.publicKey(s.createKey(map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageKeyAgreement}))

			derive := func(public []byte) reply {
				return s.call(usEast1, opDerive, map[string]any{
					fieldKeyID: own, fieldAgreement: algorithmECDH, fieldPublicKey: b64(public),
				})
			}

			want := blobOf(t, s.ok(opDerive, map[string]any{
				fieldKeyID: own, fieldAgreement: algorithmECDH, fieldPublicKey: b64(peer),
			}), "SharedSecret")

			for _, form := range []string{formCompressed, formHybrid} {
				r := derive(withPoint(t, peer, form))
				require.Equal(t, http.StatusOK, r.status, "%s: %v", form, r.body)
				require.Equal(t, want, blobOf(t, r.body, "SharedSecret"), form)
			}

			// A hybrid point whose parity bit does not match y is refused, as
			// is either form with a byte after it.
			r := derive(withPoint(t, peer, formHybridWrongParity))
			require.Equal(t, http.StatusBadRequest, r.status, "%v", r.body)
			require.Equal(t, codeValidation, r.errorCode())

			for _, form := range []string{formCompressed, formHybrid} {
				var info spki

				_, err := asn1.Unmarshal(withPoint(t, peer, form), &info)
				require.NoError(t, err)

				long := append(bytes.Clone(info.PublicKey.Bytes), 0)
				info.PublicKey = asn1.BitString{Bytes: long, BitLength: len(long) * 8}

				der, err := asn1.Marshal(info)
				require.NoError(t, err)

				r := derive(der)
				require.Equal(t, http.StatusBadRequest, r.status, "%s: %v", form, r.body)
				require.Equal(t, codeValidation, r.errorCode())
			}
		})
	}

	t.Run("a compressed point of another curve is refused", func(t *testing.T) {
		own := keyID(t, s.createKey(map[string]any{fieldKeySpec: eccP384, fieldKeyUsage: usageKeyAgreement}))

		other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		der, err := x509.MarshalPKIXPublicKey(&other.PublicKey)
		require.NoError(t, err)

		r := s.call(usEast1, opDerive, map[string]any{
			fieldKeyID: own, fieldAgreement: algorithmECDH, fieldPublicKey: b64(withPoint(t, der, formCompressed)),
		})
		require.Equal(t, http.StatusBadRequest, r.status, "%v", r.body)
		require.Equal(t, codeValidation, r.errorCode())
	})
}

// Test_keyAgreementAlgorithms holds the model's KeyAgreementAlgorithms on a
// KEY_AGREEMENT key, in CreateKey, DescribeKey and GetPublicKey.
func Test_keyAgreementAlgorithms(t *testing.T) {
	s := newStack(t)

	for _, spec := range []string{eccP256, secgK1} {
		meta := s.createKey(map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageKeyAgreement})
		require.Equal(t, []any{algorithmECDH}, meta[fieldAgreementAlgs])

		described, ok := s.ok("DescribeKey", map[string]any{fieldKeyID: keyID(t, meta)})[fieldKeyMetadata].(map[string]any)
		require.True(t, ok)
		require.Equal(t, []any{algorithmECDH}, described[fieldAgreementAlgs])

		out := s.ok(opGetPublicKey, map[string]any{fieldKeyID: keyID(t, meta)})
		require.Equal(t, []any{algorithmECDH}, out[fieldAgreementAlgs])
		require.NotContains(t, out, fieldEncryptionAlgs)
		require.NotContains(t, out, "SigningAlgorithms")
	}

	sign := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})
	require.NotContains(t, sign, fieldAgreementAlgs)
	require.NotContains(t, s.ok(opGetPublicKey, map[string]any{fieldKeyID: keyID(t, sign)}), fieldAgreementAlgs)
}

// Test_eccEncryptDecrypt holds that an ECC key made for ENCRYPT_DECRYPT, which
// LocalStack's CreateKey accepts, lists no encryption algorithm: the RSA ones
// LocalStack lists do not apply to it.
func Test_eccEncryptDecrypt(t *testing.T) {
	s := newStack(t)

	meta := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageEncryptDecrypt})
	require.Equal(t, usageEncryptDecrypt, meta[fieldKeyUsage])
	require.NotContains(t, meta, fieldEncryptionAlgs)

	described := s.ok("DescribeKey", map[string]any{fieldKeyID: keyID(t, meta)})[fieldKeyMetadata]
	require.NotContains(t, described, fieldEncryptionAlgs)

	out := s.ok(opGetPublicKey, map[string]any{fieldKeyID: keyID(t, meta)})
	require.NotContains(t, out, fieldEncryptionAlgs)
	require.Contains(t, out, fieldPublicKey)

	// An RSA key still lists its OAEP algorithms, a symmetric key its own.
	rsaMeta := s.customKey(rsa2048, usageEncryptDecrypt, rsaFixture)
	require.Equal(t, []any{oaepSHA1, oaepSHA256}, rsaMeta[fieldEncryptionAlgs])
	require.Equal(t, []any{symmetricDefault}, s.createKey(nil)[fieldEncryptionAlgs])

	// An HMAC key answers 200 without a public key, as a symmetric one does.
	hmacKey := s.createKey(map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: usageGenerateMAC})
	require.NotContains(t, s.ok(opGetPublicKey, map[string]any{fieldKeyID: keyID(t, hmacKey)}), fieldPublicKey)
}

// Test_CreateKey_customMaterialTrailingByte holds that custom DER with a byte
// after the key is refused on every curve and for RSA, as OpenSSL refuses it.
func Test_CreateKey_customMaterialTrailingByte(t *testing.T) {
	s := newStack(t)

	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	sec1, err := x509.MarshalECPrivateKey(p256)
	require.NoError(t, err)

	// A secp256k1 key in PKCS #8, as GenerateDataKeyPair answers it.
	pair := s.ok("GenerateDataKeyPair", map[string]any{
		fieldKeyID: keyID(t, s.createKey(nil)), "KeyPairSpec": secgK1,
	})

	cases := []struct {
		spec string
		der  []byte
	}{
		{eccP256, mustPKCS8(t, p256)},
		{eccP256, sec1},
		{rsa2048, mustPKCS8(t, rsaFixture)},
		{rsa2048, x509.MarshalPKCS1PrivateKey(rsaFixture)},
		{secgK1, blobOf(t, pair, fieldPrivateKey)},
	}

	for _, c := range cases {
		input := func(der []byte) map[string]any {
			return map[string]any{
				fieldKeySpec: c.spec, fieldKeyUsage: usageSignVerify,
				fieldTags: []map[string]any{{fieldTagKey: tagCustomMaterial, fieldTagValue: b64(der)}},
			}
		}

		s.ok("CreateKey", input(c.der))

		r := s.call(usEast1, "CreateKey", input(append(append([]byte{}, c.der...), 0)))
		require.Equal(t, http.StatusBadRequest, r.status, "%s: %v", c.spec, r.body)
		require.Equal(t, codeValidation, r.errorCode())
	}
}

func mustPKCS8(t *testing.T, key any) []byte {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return der
}
