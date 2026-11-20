package kms_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

const fieldCiphertextForRecipient = "CiphertextForRecipient"

// cborHead is a CBOR item head of major type major and argument n.
func cborHead(major byte, n int) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	default:
		out := []byte{major<<5 | 25, 0, 0}
		binary.BigEndian.PutUint16(out[1:], uint16(n))

		return out
	}
}

// attestation is the document test_decrypt_recipient builds:
// cbor2.dumps(["", "", cbor2.dumps({"public_key": der}), ""]).
func attestation(der []byte) []byte {
	const (
		bytesType = 2
		textType  = 3
		arrayType = 4
		mapType   = 5
	)

	inner := cborHead(mapType, 1)
	inner = append(inner, cborHead(textType, len("public_key"))...)
	inner = append(inner, "public_key"...)
	inner = append(inner, cborHead(bytesType, len(der))...)
	inner = append(inner, der...)

	doc := cborHead(arrayType, 4)
	doc = append(doc, cborHead(textType, 0)...)
	doc = append(doc, cborHead(textType, 0)...)
	doc = append(doc, cborHead(bytesType, len(inner))...)
	doc = append(doc, inner...)
	doc = append(doc, cborHead(textType, 0)...)

	return doc
}

// The envelope as the test reads it, written apart from the service's own
// structures so that the two must agree on the DER.
type (
	testContentInfo struct {
		ContentType asn1.ObjectIdentifier
		Content     testEnvelopedData `asn1:"explicit,tag:0"`
	}
	testEnvelopedData struct {
		Version        int
		RecipientInfos []testRecipientInfo `asn1:"set"`
		Content        testContent
	}
	testContent struct {
		ContentType asn1.ObjectIdentifier
		Algorithm   testCBC
		Encrypted   []byte `asn1:"tag:0"`
	}
	testCBC struct {
		Algorithm asn1.ObjectIdentifier
		IV        []byte
	}
	testRecipientInfo struct {
		Version      int
		RID          testIssuerSerial
		Algorithm    testOAEP
		EncryptedKey []byte
	}
	testIssuerSerial struct {
		Issuer pkix.RDNSequence
		Serial int
	}
	testOAEP struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters testOAEPParams
	}
	testOAEPParams struct {
		Hash testAlgorithm `asn1:"explicit,tag:0"`
		MGF  testMGF       `asn1:"explicit,tag:1"`
	}
	testMGF struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters testAlgorithm
	}
	testAlgorithm struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters asn1.RawValue `asn1:"optional"`
	}
)

// openEnvelope reads a CMS EnvelopedData as test_decrypt_recipient reads it,
// checks the fields pkcs7_envelope_encrypt writes, and decrypts it with key.
func openEnvelope(t *testing.T, der []byte, key *rsa.PrivateKey) []byte {
	t.Helper()

	var info testContentInfo

	rest, err := asn1.Unmarshal(der, &info)
	require.NoError(t, err)
	require.Empty(t, rest)

	sha256OID := asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}

	require.Equal(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 3}, info.ContentType, "enveloped-data")
	require.Equal(t, 0, info.Content.Version)
	require.Len(t, info.Content.RecipientInfos, 1)

	r := info.Content.RecipientInfos[0]
	require.Equal(t, 0, r.Version)
	require.Equal(t, pkix.RDNSequence{{{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: "recipient"}}}, r.RID.Issuer)
	require.Equal(t, 1, r.RID.Serial)
	require.Equal(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 7}, r.Algorithm.Algorithm, "RSAES-OAEP")
	require.Equal(t, sha256OID, r.Algorithm.Parameters.Hash.Algorithm)
	require.Equal(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}, r.Algorithm.Parameters.MGF.Algorithm, "MGF1")
	require.Equal(t, sha256OID, r.Algorithm.Parameters.MGF.Parameters.Algorithm)

	c := info.Content.Content
	require.Equal(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}, c.ContentType, "data")
	require.Equal(t, asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}, c.Algorithm.Algorithm, "aes256-CBC")
	require.Len(t, c.Algorithm.IV, aes.BlockSize)

	contentKey, err := rsa.DecryptOAEP(sha256.New(), nil, key, r.EncryptedKey, nil)
	require.NoError(t, err)
	require.Len(t, contentKey, 32)

	block, err := aes.NewCipher(contentKey)
	require.NoError(t, err)
	require.Zero(t, len(c.Encrypted)%aes.BlockSize)

	plain := make([]byte, len(c.Encrypted))
	cipher.NewCBCDecrypter(block, c.Algorithm.IV).CryptBlocks(plain, c.Encrypted)

	pad := int(plain[len(plain)-1])
	require.True(t, pad >= 1 && pad <= aes.BlockSize, "padding %d", pad)

	return plain[:len(plain)-pad]
}

// Test_Recipient_envelope holds test_decrypt_recipient for Decrypt, and the
// same answer for the other three calls that take a Recipient: the plaintext
// in CiphertextForRecipient, a CMS envelope the recipient's key opens, and no
// plaintext beside it.
func Test_Recipient_envelope(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	r := map[string]any{fieldAttestation: b64(attestation(der))}

	message := []byte("test message 123 !%$@ 1234567890")
	blob := blobOf(t, s.ok("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64(message)}),
		fieldCiphertextBlob)

	t.Run("Decrypt", func(t *testing.T) {
		out := s.ok(opDecrypt, map[string]any{fieldKeyID: id, fieldCiphertextBlob: b64(blob), fieldRecipient: r})
		require.NotContains(t, out, fieldPlaintext)
		require.Equal(t, message, openEnvelope(t, blobOf(t, out, fieldCiphertextForRecipient), key))
	})

	t.Run(opGenerateDataKey, func(t *testing.T) {
		out := s.ok(opGenerateDataKey, map[string]any{fieldKeyID: id, fieldRecipient: r})
		require.NotContains(t, out, fieldPlaintext)

		dataKey := openEnvelope(t, blobOf(t, out, fieldCiphertextForRecipient), key)
		require.Len(t, dataKey, 32)

		plain := s.ok(opDecrypt, map[string]any{fieldCiphertextBlob: out[fieldCiphertextBlob]})
		require.Equal(t, b64(dataKey), plain[fieldPlaintext], "the envelope holds the key the blob holds")
	})

	t.Run(opGenerateDataKeyPair, func(t *testing.T) {
		out := s.ok(opGenerateDataKeyPair, map[string]any{fieldKeyID: id, fieldKeyPairSpec: eccP256, fieldRecipient: r})
		require.NotContains(t, out, fieldPrivateKey)

		private := openEnvelope(t, blobOf(t, out, fieldCiphertextForRecipient), key)
		_, err := x509.ParsePKCS8PrivateKey(private)
		require.NoError(t, err)

		plain := s.ok(opDecrypt, map[string]any{fieldCiphertextBlob: out["PrivateKeyCiphertextBlob"]})
		require.Equal(t, b64(private), plain[fieldPlaintext], "the envelope holds the private key the blob holds")
	})

	t.Run("GenerateRandom", func(t *testing.T) {
		out := s.ok("GenerateRandom", map[string]any{fieldNumberOfBytes: 48, fieldRecipient: r})
		require.NotContains(t, out, fieldPlaintext)
		require.Len(t, openEnvelope(t, blobOf(t, out, fieldCiphertextForRecipient), key), 48)
	})

	t.Run("a document with a key that is not RSA is ignored", func(t *testing.T) {
		ec := blobOf(t, s.ok(opGetPublicKey, map[string]any{
			fieldKeyID: keyID(t, s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})),
		}), "PublicKey")

		out := s.ok(opDecrypt, map[string]any{
			fieldCiphertextBlob: b64(blob), fieldRecipient: map[string]any{fieldAttestation: b64(attestation(ec))},
		})
		require.Equal(t, b64(message), out[fieldPlaintext])
		require.NotContains(t, out, fieldCiphertextForRecipient)
	})
}
