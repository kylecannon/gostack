package kms_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fieldMessageType      = "MessageType"
	fieldSigningAlgorithm = "SigningAlgorithm"
	fieldSignature        = "Signature"
	fieldPublicKey        = "PublicKey"
	fieldAgreement        = "KeyAgreementAlgorithm"

	usageKeyAgreement = "KEY_AGREEMENT"
	eccP384           = "ECC_NIST_P384"
	eccP521           = "ECC_NIST_P521"
	oaepSHA1          = "RSAES_OAEP_SHA_1"
	secgK1            = "ECC_SECG_P256K1"
	ecdsaSHA256       = "ECDSA_SHA_256"
	ecdsaSHA384       = "ECDSA_SHA_384"
	ecdsaSHA512       = "ECDSA_SHA_512"
	rsa3072           = "RSA_3072"
	rsa4096           = "RSA_4096"
	pssSHA256         = "RSASSA_PSS_SHA_256"
	typeDigest        = "DIGEST"
	typeRaw           = "RAW"
	algorithmECDH     = "ECDH"

	codeInvalidSignature = "KMSInvalidSignatureException"
)

var signMessage = []byte("test message !%$@ 1234567890")

// rsaFixture is one RSA_2048 key the tests share as custom material, since
// generating RSA keys under -race is slow.
var rsaFixture = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return key
}()

// customKey makes a key of spec and usage from private, as
// _custom_key_material_.
func (s *stack) customKey(spec, usage string, private any) map[string]any {
	s.t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(s.t, err)

	return s.createKey(map[string]any{
		fieldKeySpec: spec, fieldKeyUsage: usage,
		fieldTags: []map[string]any{{fieldTagKey: tagCustomMaterial, fieldTagValue: b64(der)}},
	})
}

// digestFor is the digest an algorithm names, of message.
func digestFor(algorithm string, message []byte) []byte {
	switch {
	case strings.Contains(algorithm, "SHA_384"):
		s := sha512.Sum384(message)

		return s[:]
	case strings.Contains(algorithm, "SHA_512"):
		s := sha512.Sum512(message)

		return s[:]
	default:
		s := sha256.Sum256(message)

		return s[:]
	}
}

// Test_SignVerify is test_sign_verify on every spec and algorithm a key of
// each spec lists: RAW and DIGEST sign and verify, the signature checked by the
// standard library where it can read the key, and the errors.
func Test_SignVerify(t *testing.T) {
	s := newStack(t)

	tests := []struct {
		spec, algorithm string
		custom          any
	}{
		{spec: rsa2048, algorithm: pssSHA256, custom: rsaFixture},
		{spec: rsa2048, algorithm: "RSASSA_PSS_SHA_384", custom: rsaFixture},
		{spec: rsa2048, algorithm: "RSASSA_PSS_SHA_512", custom: rsaFixture},
		{spec: rsa2048, algorithm: "RSASSA_PKCS1_V1_5_SHA_256", custom: rsaFixture},
		{spec: rsa2048, algorithm: "RSASSA_PKCS1_V1_5_SHA_384", custom: rsaFixture},
		{spec: rsa2048, algorithm: "RSASSA_PKCS1_V1_5_SHA_512", custom: rsaFixture},
		{spec: eccP256, algorithm: ecdsaSHA256},
		{spec: eccP384, algorithm: ecdsaSHA384},
		{spec: eccP521, algorithm: ecdsaSHA512},
		{spec: secgK1, algorithm: ecdsaSHA256},
	}

	for _, tt := range tests {
		t.Run(tt.spec+"/"+tt.algorithm, func(t *testing.T) {
			var meta map[string]any
			if tt.custom != nil {
				meta = s.customKey(tt.spec, usageSignVerify, tt.custom)
			} else {
				meta = s.createKey(map[string]any{fieldKeySpec: tt.spec, fieldKeyUsage: usageSignVerify})
			}

			id := keyID(t, meta)
			base := map[string]any{fieldKeyID: id, fieldSigningAlgorithm: tt.algorithm}
			with := func(extra map[string]any) map[string]any {
				out := maps.Clone(base)
				maps.Copy(out, extra)

				return out
			}

			digest := digestFor(tt.algorithm, signMessage)

			signed := s.ok("Sign", with(map[string]any{fieldMessageType: typeRaw, fieldMessage: b64(signMessage)}))
			require.Equal(t, arn(t, meta), signed[fieldKeyID])
			require.Equal(t, tt.algorithm, signed[fieldSigningAlgorithm])

			signature := blobOf(t, signed, fieldSignature)

			verified := s.ok("Verify", with(map[string]any{
				fieldMessageType: typeRaw, fieldMessage: b64(signMessage), fieldSignature: b64(signature),
			}))
			require.Equal(t, true, verified["SignatureValid"])

			publicDER := blobOf(t, s.ok("GetPublicKey", map[string]any{fieldKeyID: id}), fieldPublicKey)
			checkWithStandardLibrary(t, tt.algorithm, publicDER, digest, signature)

			// A DIGEST signature verifies as RAW of the same message.
			signedDigest := blobOf(t, s.ok("Sign", with(map[string]any{
				fieldMessageType: typeDigest, fieldMessage: b64(digest),
			})), fieldSignature)
			s.ok("Verify", with(map[string]any{
				fieldMessageType: typeRaw, fieldMessage: b64(signMessage), fieldSignature: b64(signedDigest),
			}))

			s.fails("Sign", with(map[string]any{fieldMessageType: typeDigest, fieldMessage: b64(signMessage)}),
				codeValidation, "Digest is invalid length for algorithm "+tt.algorithm+".")
			s.fails("Verify", with(map[string]any{
				fieldMessageType: typeDigest, fieldMessage: b64([]byte("bad message 321")), fieldSignature: b64(signature),
			}), codeValidation, "Digest is invalid length for algorithm "+tt.algorithm+".")
			s.fails("Verify", with(map[string]any{
				fieldMessageType: typeRaw, fieldMessage: b64([]byte("bad message 321")), fieldSignature: b64(signature),
			}), codeInvalidSignature, "")
			s.fails("Verify", with(map[string]any{
				fieldMessageType: typeRaw, fieldMessage: b64(signMessage), fieldSignature: b64([]byte("junk")),
			}), codeInvalidSignature, "")
		})
	}

	require.Zero(t, s.changes.Load())
}

// checkWithStandardLibrary verifies signature with the standard library, which
// reads every public key but secp256k1's (that one is checked against the
// dependency in the package's own tests).
func checkWithStandardLibrary(t *testing.T, algorithm string, publicDER, digest, signature []byte) {
	t.Helper()

	public, err := x509.ParsePKIXPublicKey(publicDER)
	if err != nil {
		require.Contains(t, err.Error(), "unsupported elliptic curve")

		return
	}

	sizes := map[int]crypto.Hash{sha256.Size: crypto.SHA256, sha512.Size384: crypto.SHA384, sha512.Size: crypto.SHA512}
	hash := sizes[len(digest)]

	switch key := public.(type) {
	case *rsa.PublicKey:
		if strings.Contains(algorithm, "PSS") {
			require.NoError(t, rsa.VerifyPSS(key, hash, digest, signature, &rsa.PSSOptions{SaltLength: len(digest)}))
		} else {
			require.NoError(t, rsa.VerifyPKCS1v15(key, hash, digest, signature))
		}
	case *ecdsa.PublicKey:
		require.True(t, ecdsa.VerifyASN1(key, digest, signature))
	default:
		t.Fatalf("a %T public key", public)
	}
}

func Test_SignVerify_errors(t *testing.T) {
	s := newStack(t)
	ecc := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})
	rsaEncrypt := s.customKey(rsa2048, usageEncryptDecrypt, rsaFixture)
	message := b64([]byte("hello"))

	t.Run("a key of another usage", func(t *testing.T) {
		s.fails("Sign", map[string]any{
			fieldKeyID: keyID(t, rsaEncrypt), fieldMessage: message, fieldSigningAlgorithm: pssSHA256,
		}, codeInvalidKeyUsage, arn(t, rsaEncrypt)+" key usage is ENCRYPT_DECRYPT which is not valid for Sign.")
		s.fails("Verify", map[string]any{
			fieldKeyID: keyID(t, rsaEncrypt), fieldMessage: message, fieldSigningAlgorithm: pssSHA256,
			fieldSignature: message,
		}, codeInvalidKeyUsage, arn(t, rsaEncrypt)+" key usage is ENCRYPT_DECRYPT which is not valid for Verify.")
	})

	t.Run("an algorithm of another family", func(t *testing.T) {
		s.fails("Sign", map[string]any{
			fieldKeyID: keyID(t, ecc), fieldMessage: message, fieldSigningAlgorithm: pssSHA256,
		}, codeInvalidKeyUsage, "Algorithm RSASSA_PSS_SHA_256 is incompatible with key spec ECC_NIST_P256.")
	})

	t.Run("an algorithm that names no digest", func(t *testing.T) {
		s.fails("Sign", map[string]any{
			fieldKeyID: keyID(t, ecc), fieldMessage: message, fieldSigningAlgorithm: "SM2DSA",
		}, codeValidation, "Unsupported hash type in SigningAlgorithm 'SM2DSA'")
	})

	t.Run("another digest of the family signs, as in LocalStack", func(t *testing.T) {
		s.ok("Sign", map[string]any{fieldKeyID: keyID(t, ecc), fieldMessage: message, fieldSigningAlgorithm: ecdsaSHA512})
	})

	t.Run("missing members", func(t *testing.T) {
		s.fails("Sign", map[string]any{fieldKeyID: keyID(t, ecc)}, codeValidation,
			"2 validation errors detected: Value null at 'message' failed to satisfy constraint: Member must not be null; "+
				"Value null at 'signingAlgorithm' failed to satisfy constraint: Member must not be null")
		s.fails("Verify", map[string]any{
			fieldKeyID: keyID(t, ecc), fieldMessage: message, fieldSigningAlgorithm: ecdsaSHA256,
		}, codeValidation,
			"1 validation error detected: Value null at 'signature' failed to satisfy constraint: Member must not be null")
	})

	t.Run("a disabled key", func(t *testing.T) {
		disabled := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})
		s.ok("DisableKey", map[string]any{fieldKeyID: keyID(t, disabled)})
		s.fails("Sign", map[string]any{
			fieldKeyID: keyID(t, disabled), fieldMessage: message, fieldSigningAlgorithm: ecdsaSHA256,
		}, "DisabledException", arn(t, disabled)+" is disabled.")

		// GetPublicKey answers for a disabled key, as AWS does.
		s.ok("GetPublicKey", map[string]any{fieldKeyID: keyID(t, disabled)})
	})
}

// Test_GetPublicKey is test_get_public_key and the algorithm lists of each
// usage, with test_create_key_custom_key_material_rsa_2048's check that custom
// material answers its own public key.
func Test_GetPublicKey(t *testing.T) {
	s := newStack(t)

	encrypt := s.customKey(rsa2048, usageEncryptDecrypt, rsaFixture)
	out := s.ok("GetPublicKey", map[string]any{fieldKeyID: keyID(t, encrypt)})
	require.Equal(t, arn(t, encrypt), out[fieldKeyID])
	require.Equal(t, rsa2048, out[fieldKeySpec])
	require.Equal(t, usageEncryptDecrypt, out[fieldKeyUsage])
	require.Equal(t, []any{oaepSHA1, "RSAES_OAEP_SHA_256"}, out["EncryptionAlgorithms"])
	require.NotContains(t, out, "SigningAlgorithms")

	want, err := x509.MarshalPKIXPublicKey(&rsaFixture.PublicKey)
	require.NoError(t, err)
	require.Equal(t, want, blobOf(t, out, fieldPublicKey))

	sign := s.createKey(map[string]any{fieldKeySpec: eccP384, fieldKeyUsage: usageSignVerify})
	out = s.ok("GetPublicKey", map[string]any{fieldKeyID: arn(t, sign)})
	require.Equal(t, []any{ecdsaSHA384}, out["SigningAlgorithms"])
	require.NotContains(t, out, "EncryptionAlgorithms")

	// A symmetric key has no public key; LocalStack answers without one.
	symmetric := s.ok("GetPublicKey", map[string]any{fieldKeyID: keyID(t, s.createKey(nil))})
	require.NotContains(t, symmetric, fieldPublicKey)

	require.Zero(t, s.changes.Load())
}

// Test_offlineEncryptOnlineDecrypt is test_symmetric_encrypt_offline_decrypt_online:
// data sealed under GetPublicKey's key with OAEP SHA-256 decrypts in KMS.
func Test_offlineEncryptOnlineDecrypt(t *testing.T) {
	s := newStack(t)
	meta := s.customKey(rsa2048, usageEncryptDecrypt, rsaFixture)

	parsed, err := x509.ParsePKIXPublicKey(s.publicKey(meta))
	require.NoError(t, err)

	public, ok := parsed.(*rsa.PublicKey)
	require.True(t, ok)

	sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, public, signMessage, nil)
	require.NoError(t, err)

	opened := s.ok("Decrypt", map[string]any{
		fieldKeyID: keyID(t, meta), fieldCiphertextBlob: b64(sealed), "EncryptionAlgorithm": oaepSHA1,
	})
	require.Equal(t, signMessage, blobOf(t, opened, fieldPlaintext))
}

// Test_DeriveSharedSecret is test_derive_shared_secret on every curve: two keys
// derive one secret, and an outside peer's ECDH gives the same one.
func Test_DeriveSharedSecret(t *testing.T) {
	s := newStack(t)

	for _, spec := range []string{eccP256, eccP384, eccP521, secgK1} {
		t.Run(spec, func(t *testing.T) {
			first := s.createKey(map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageKeyAgreement})
			second := s.createKey(map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageKeyAgreement})

			out := s.ok("DeriveSharedSecret", map[string]any{
				fieldKeyID: keyID(t, first), fieldAgreement: algorithmECDH, fieldPublicKey: b64(s.publicKey(second)),
			})
			require.Equal(t, keyID(t, first), out[fieldKeyID])
			require.Equal(t, algorithmECDH, out[fieldAgreement])
			require.Equal(t, "AWS_KMS", out["KeyOrigin"])

			back := s.ok("DeriveSharedSecret", map[string]any{
				fieldKeyID: arn(t, second), fieldAgreement: algorithmECDH, fieldPublicKey: b64(s.publicKey(first)),
			})
			require.Equal(t, blobOf(t, out, "SharedSecret"), blobOf(t, back, "SharedSecret"))
		})
	}

	t.Run("matches the standard library's ECDH", func(t *testing.T) {
		meta := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageKeyAgreement})

		parsed, err := x509.ParsePKIXPublicKey(s.publicKey(meta))
		require.NoError(t, err)

		kmsPublic, ok := parsed.(*ecdsa.PublicKey)
		require.True(t, ok)

		local, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		localDER, err := x509.MarshalPKIXPublicKey(&local.PublicKey)
		require.NoError(t, err)

		localECDH, err := local.ECDH()
		require.NoError(t, err)

		peer, err := kmsPublic.ECDH()
		require.NoError(t, err)

		want, err := localECDH.ECDH(peer)
		require.NoError(t, err)

		out := s.ok("DeriveSharedSecret", map[string]any{
			fieldKeyID: keyID(t, meta), fieldAgreement: algorithmECDH, fieldPublicKey: b64(localDER),
		})
		require.Equal(t, want, blobOf(t, out, "SharedSecret"))
	})

	require.Zero(t, s.changes.Load())
}

func Test_DeriveSharedSecret_errors(t *testing.T) {
	s := newStack(t)
	agreement := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageKeyAgreement})
	peer := b64(s.publicKey(agreement))
	signing := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})
	symmetric := s.createKey(nil)

	derive := func(meta map[string]any, algorithm string, public any) map[string]any {
		input := map[string]any{fieldKeyID: keyID(t, meta), fieldAgreement: algorithm}
		if public != nil {
			input[fieldPublicKey] = public
		}

		return input
	}

	const notValid = " which is not valid for DeriveSharedSecret."

	s.fails("DeriveSharedSecret", derive(signing, algorithmECDH, peer),
		codeInvalidKeyUsage, arn(t, signing)+" key usage is SIGN_VERIFY"+notValid)
	s.fails("DeriveSharedSecret", derive(symmetric, algorithmECDH, peer),
		codeInvalidKeyUsage, arn(t, symmetric)+" key usage is ENCRYPT_DECRYPT"+notValid)
	s.fails("DeriveSharedSecret", derive(agreement, "INVALID", peer),
		codeValidation, "1 validation error detected: Value 'INVALID' at 'keyAgreementAlgorithm' failed to satisfy "+
			"constraint: Member must satisfy enum value set: [ECDH]")
	s.fails("DeriveSharedSecret", derive(agreement, algorithmECDH, b64([]byte("InvalidPublicKey"))),
		codeValidation, "")
	s.fails("DeriveSharedSecret", derive(agreement, algorithmECDH, nil),
		codeValidation, "1 validation error detected: Value null at 'publicKey' failed to satisfy constraint: "+
			"Member must not be null")
	s.fails("CreateKey", map[string]any{fieldKeySpec: rsa2048, fieldKeyUsage: usageKeyAgreement},
		codeValidation, "KeyUsage KEY_AGREEMENT is not compatible with KeySpec RSA_2048")

	// A disabled key derives, as in LocalStack.
	s.ok("DisableKey", map[string]any{fieldKeyID: keyID(t, agreement)})
	s.ok("DeriveSharedSecret", derive(agreement, algorithmECDH, peer))

	r := s.call(usEast1, "DeriveSharedSecret", map[string]any{
		fieldKeyID: keyID(t, agreement), fieldAgreement: algorithmECDH, fieldPublicKey: peer,
		"Recipient": map[string]any{"AttestationDocument": b64([]byte("doc"))},
	})
	require.Equal(t, http.StatusNotImplemented, r.status)
}

// Test_GenerateDataKeyPair_secp256k1: the pair's private key is PKCS #8 and
// its public key a SubjectPublicKeyInfo, both of the secp256k1 OID, and the
// blob opens to the private key.
func Test_GenerateDataKeyPair_secp256k1(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)

	out := s.ok("GenerateDataKeyPair", map[string]any{fieldKeyID: keyID(t, meta), "KeyPairSpec": secgK1})
	private := blobOf(t, out, "PrivateKeyPlaintext")

	// 06 05 2b 81 04 00 0a is the DER of OID 1.3.132.0.10.
	secgOID := []byte{0x06, 0x05, 0x2b, 0x81, 0x04, 0x00, 0x0a}
	require.Contains(t, string(private), string(secgOID))
	require.Contains(t, string(blobOf(t, out, fieldPublicKey)), string(secgOID))

	opened := s.ok("Decrypt", map[string]any{fieldCiphertextBlob: b64(blobOf(t, out, "PrivateKeyCiphertextBlob"))})
	require.Equal(t, private, blobOf(t, opened, fieldPlaintext))
}

// publicKey is the DER public key GetPublicKey answers for a key.
func (s *stack) publicKey(meta map[string]any) []byte {
	s.t.Helper()

	return blobOf(s.t, s.ok("GetPublicKey", map[string]any{fieldKeyID: keyID(s.t, meta)}), fieldPublicKey)
}
