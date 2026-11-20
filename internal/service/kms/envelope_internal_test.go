package kms

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// localStackMessage is what testdata/cms/localstack-envelope.der holds. The
// file is LocalStack's own pkcs7_envelope_encrypt at 8b9a79f, run in the
// parity baseline image on this message to a new RSA-2048 key, whose PKCS #8
// is testdata/cms/recipient-key.der.
const localStackMessage = "test message 123 !%$@ 1234567890"

func readCMS(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", "cms", name))
	require.NoError(t, err)

	return b
}

func localStackRecipient(t *testing.T) *rsa.PrivateKey {
	t.Helper()

	parsed, err := x509.ParsePKCS8PrivateKey(readCMS(t, "recipient-key.der"))
	require.NoError(t, err)

	key, ok := parsed.(*rsa.PrivateKey)
	require.True(t, ok)

	return key
}

// openContent decrypts a parsed envelope with key, as test_decrypt_recipient
// does, and takes off the PKCS #7 padding.
func openContent(t *testing.T, info *contentInfo, key *rsa.PrivateKey) []byte {
	t.Helper()

	r := info.Content.RecipientInfos[0]

	contentKey, err := rsa.DecryptOAEP(sha256.New(), nil, key, r.EncryptedKey, nil)
	require.NoError(t, err)

	block, err := aes.NewCipher(contentKey)
	require.NoError(t, err)

	c := info.Content.EncryptedContentInfo
	plain := make([]byte, len(c.EncryptedContent))
	cipher.NewCBCDecrypter(block, c.ContentEncryptionAlgorithm.IV).CryptBlocks(plain, c.EncryptedContent)

	return plain[:len(plain)-int(plain[len(plain)-1])]
}

// Test_envelope_matchesLocalStack holds the envelope's ASN.1 structures to the
// DER LocalStack writes: its envelope parses into them and encodes back to
// the same bytes, so an envelope written from them differs from LocalStack's
// only in the random content key, IV and OAEP seed.
func Test_envelope_matchesLocalStack(t *testing.T) {
	der := readCMS(t, "localstack-envelope.der")

	var info contentInfo

	rest, err := asn1.Unmarshal(der, &info)
	require.NoError(t, err)
	require.Empty(t, rest)

	again, err := asn1.Marshal(info)
	require.NoError(t, err)
	require.Equal(t, der, again, "the structures encode LocalStack's envelope byte for byte")

	require.Equal(t, localStackMessage, string(openContent(t, &info, localStackRecipient(t))))
}

// Test_envelope holds that an envelope this service writes is LocalStack's,
// field for field, and that the recipient's key opens it for every length of
// plaintext around a block.
func Test_envelope(t *testing.T) {
	key := localStackRecipient(t)

	var want contentInfo

	_, err := asn1.Unmarshal(readCMS(t, "localstack-envelope.der"), &want)
	require.NoError(t, err)

	for n := range 2*aes.BlockSize + 1 {
		plaintext := bytes.Repeat([]byte{byte(n)}, n)

		der, err := envelope(plaintext, &key.PublicKey, rand.Reader)
		require.NoError(t, err)

		var got contentInfo

		rest, err := asn1.Unmarshal(der, &got)
		require.NoError(t, err)
		require.Empty(t, rest)

		require.Equal(t, plaintext, openContent(t, &got, key), "%d bytes", n)
		require.Len(t, got.Content.EncryptedContentInfo.EncryptedContent, (n/aes.BlockSize+1)*aes.BlockSize)

		// All but the random parts are LocalStack's.
		got.Content.RecipientInfos[0].EncryptedKey = want.Content.RecipientInfos[0].EncryptedKey
		gotContent, wantContent := &got.Content.EncryptedContentInfo, &want.Content.EncryptedContentInfo
		gotContent.ContentEncryptionAlgorithm.IV = wantContent.ContentEncryptionAlgorithm.IV
		gotContent.EncryptedContent = wantContent.EncryptedContent

		gotDER, err := asn1.Marshal(got)
		require.NoError(t, err)

		wantDER, err := asn1.Marshal(want)
		require.NoError(t, err)
		require.Equal(t, wantDER, gotDER)
	}
}

// cborDoc is the attestation document test_decrypt_recipient builds around
// der, written out byte by byte: ["", "", bstr({"public_key": der}), ""].
func cborDoc(der []byte) []byte {
	inner := slices.Concat([]byte{0xa1, 0x6a}, []byte(attestationKey), bstrHead(der), der)

	return slices.Concat([]byte{0x84, 0x60, 0x60}, bstrHead(inner), inner, []byte{0x60})
}

// bstrHead is the head of a CBOR byte string of b's length, below 64 KiB.
func bstrHead(b []byte) []byte {
	return []byte{0x59, byte(len(b) >> 8), byte(len(b))}
}

// Test_attestationKeyOf holds the reader to the documents LocalStack reads
// and those it ignores.
func Test_attestationKeyOf(t *testing.T) {
	key := localStackRecipient(t)

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	got, err := attestationKeyOf(cborDoc(der))
	require.NoError(t, err)
	require.True(t, key.PublicKey.Equal(got))

	// Other items before the payload, and other pairs in it, are skipped:
	// an unsigned, a negative, a tagged array, a float, an indefinite map
	// and a key that is not text.
	inner := slices.Concat(
		[]byte{0xa3, 0x01, 0x20, 0x63, 'a', 'b', 'c', 0xd8, 0x18, 0x82, 0x01, 0xf9, 0x3c, 0x00, 0x6a},
		[]byte(attestationKey), bstrHead(der), der)
	doc := slices.Concat([]byte{0x84, 0xbf, 0x61, 'k', 0x80, 0xff, 0xf6}, bstrHead(inner), inner, []byte{0x40})

	got, err = attestationKeyOf(doc)
	require.NoError(t, err)
	require.True(t, key.PublicKey.Equal(got))

	for name, doc := range map[string][]byte{
		"empty":                  {},
		"the junk of SQ-274":     []byte("junk"),
		"base64 of junk":         []byte("aW52YWxpZGF0dGVzdGF0aW9u"),
		"three items":            {0x83, 0x60, 0x60, 0x40},
		"a payload that is text": {0x84, 0x60, 0x60, 0x60, 0x60},
		"no public_key":          {0x84, 0x60, 0x60, 0x41, 0xa0, 0x60},
		"a length past the end":  {0x84, 0x60, 0x60, 0x5a, 0xff, 0xff, 0xff, 0xff},
		"a key that is not DER": slices.Concat(
			[]byte{0x84, 0x60, 0x60, 0x4e, 0xa1, 0x6a}, []byte(attestationKey), []byte{0x41, 0x00, 0x60}),
		"nesting past the bound": append(append([]byte{0x84}, bytes.Repeat([]byte{0x81}, cborMaxDepth+2)...), 0x00),
	} {
		_, err := attestationKeyOf(doc)
		require.Error(t, err, name)
	}
}

// FuzzAttestationKeyOf holds that no document panics the reader.
func FuzzAttestationKeyOf(f *testing.F) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(f, err)

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(f, err)

	f.Add(cborDoc(der))
	f.Add([]byte("junk"))
	f.Add([]byte{0x84, 0xbf, 0x61, 'k', 0x80, 0xff, 0xf6, 0x41, 0xa0, 0x40})

	f.Fuzz(func(_ *testing.T, doc []byte) {
		_, _ = attestationKeyOf(doc)
	})
}
