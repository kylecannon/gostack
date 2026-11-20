package kms_test

import (
	"bytes"
	"crypto/aes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opParams       = "GetParametersForImport"
	opImport       = "ImportKeyMaterial"
	opDeleteImport = "DeleteImportedKeyMaterial"

	fieldWrapAlgorithm = "WrappingAlgorithm"
	fieldWrapSpec      = "WrappingKeySpec"
	fieldImportToken   = "ImportToken"
	fieldWrapped       = "EncryptedKeyMaterial"
	fieldExpiration    = "ExpirationModel"
	fieldImportType    = "ImportType"
	fieldKeyMaterialID = "KeyMaterialId"
	fieldKeyState      = "KeyState"

	aesWrapSHA256  = "RSA_AES_KEY_WRAP_SHA_256"
	noExpiry       = "KEY_MATERIAL_DOES_NOT_EXPIRE"
	statePending   = "PendingImport"
	codeKMSInvalid = "KMSInvalidStateException"
)

// importParams are the parameters of one GetParametersForImport.
type importParams struct {
	token     string
	public    *rsa.PublicKey
	algorithm string
}

func (s *stack) importParams(id, algorithm string) importParams {
	s.t.Helper()

	out := s.ok(opParams, map[string]any{fieldKeyID: id, fieldWrapAlgorithm: algorithm, fieldWrapSpec: rsa2048})

	parsed, err := x509.ParsePKIXPublicKey(blobOf(s.t, out, fieldPublicKey))
	require.NoError(s.t, err)

	public, ok := parsed.(*rsa.PublicKey)
	require.True(s.t, ok)
	require.Equal(s.t, 2048, public.N.BitLen())

	token, ok := out[fieldImportToken].(string)
	require.True(s.t, ok)

	return importParams{token: token, public: public, algorithm: algorithm}
}

// wrap is material wrapped as a client wraps it for params' algorithm.
func (p importParams) wrap(t *testing.T, material []byte) []byte {
	t.Helper()

	h := sha256.New
	if p.algorithm == oaepSHA1 {
		h = sha1.New
	}

	if p.algorithm != aesWrapSHA256 {
		out, err := rsa.EncryptOAEP(h(), rand.Reader, p.public, material, nil)
		require.NoError(t, err)

		return out
	}

	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)

	wrappedKEK, err := rsa.EncryptOAEP(h(), rand.Reader, p.public, kek, nil)
	require.NoError(t, err)

	return append(wrappedKEK, wrapPadded(t, kek, material)...)
}

// wrapPadded is a client's RFC 5649 wrap, written apart from the service's
// unwrap: the alternative initial value, zero padding, then RFC 3394's six
// rounds, which for one semiblock are a single AES encryption.
func wrapPadded(t *testing.T, kek, key []byte) []byte {
	t.Helper()

	block, err := aes.NewCipher(kek)
	require.NoError(t, err)

	a := make([]byte, 8, 8+len(key)+8)
	copy(a, []byte{0xA6, 0x59, 0x59, 0xA6})
	binary.BigEndian.PutUint32(a[4:], uint32(len(key)))

	r := make([]byte, (len(key)+7)/8*8)
	copy(r, key)

	if len(r) == 8 {
		out := slices.Concat(a, r)
		block.Encrypt(out, out)

		return out
	}

	n := len(r) / 8
	b := make([]byte, 16)

	for j := range 6 {
		for i := range n {
			copy(b, a)
			copy(b[8:], r[i*8:])
			block.Encrypt(b, b)

			copy(a, b[:8])
			binary.BigEndian.PutUint64(b, binary.BigEndian.Uint64(a)^uint64(n*j+i+1))
			copy(a, b[:8])
			copy(r[i*8:(i+1)*8], b[8:])
		}
	}

	return append(a, r...)
}

// importInput is an ImportKeyMaterial of material under params.
func (p importParams) input(t *testing.T, id string, material []byte) map[string]any {
	t.Helper()

	return map[string]any{
		fieldKeyID: id, fieldImportToken: p.token, fieldWrapped: b64(p.wrap(t, material)), fieldExpiration: noExpiry,
	}
}

func (s *stack) metadata(id string) map[string]any {
	s.t.Helper()

	return object(s.t, s.ok(opDescribe, map[string]any{fieldKeyID: id})[fieldKeyMetadata])
}

func random(t *testing.T, n int) []byte {
	t.Helper()

	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)

	return b
}

// Test_import_symmetric is test_import_key_symmetric with the steps around
// it: the key refuses Encrypt until an import, an expiring import needs
// ValidTo, the import enables the key, delete moves it back keeping its
// material id, and the same material imported again decrypts the old blobs.
func Test_import_symmetric(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(map[string]any{fieldOrigin: originExternal})
	id, keyARN := keyID(t, meta), arn(t, meta)
	pendingImport := keyARN + " is pending import."

	s.fails("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("x"))}, codeKMSInvalid, pendingImport)

	params := s.importParams(id, oaepSHA256)
	material := random(t, 32)

	in := params.input(t, id, material)
	delete(in, fieldExpiration)
	s.fails(opImport, in, codeValidation, "A validTo date must be set if the ExpirationModel is KEY_MATERIAL_EXPIRES")
	require.Equal(t, statePending, s.metadata(id)[fieldKeyState], "a refused import changes nothing")
	require.NotContains(t, s.metadata(id), fieldExpiration)

	out := s.ok(opImport, params.input(t, id, material))
	require.Equal(t, keyARN, out[fieldKeyID])

	after := s.metadata(id)
	require.Equal(t, "Enabled", after[fieldKeyState])
	require.Equal(t, true, after["Enabled"])
	require.Equal(t, noExpiry, after[fieldExpiration])
	require.Equal(t, out[fieldKeyMaterialID], after[fieldMaterialID])
	require.Len(t, after[fieldMaterialID], 64)

	blob := s.encrypt(id, []byte("secret"), nil)
	require.Equal(t, []byte("secret"), s.decrypted(usEast1, blob))

	deleted := s.ok(opDeleteImport, map[string]any{fieldKeyID: id})
	require.Equal(t, keyARN, deleted[fieldKeyID])
	require.Equal(t, after[fieldMaterialID], deleted[fieldKeyMaterialID])

	gone := s.metadata(id)
	require.Equal(t, statePending, gone[fieldKeyState])
	require.Equal(t, false, gone["Enabled"])
	require.Equal(t, after[fieldMaterialID], gone[fieldMaterialID], "the material id stays, as AWS shows it")
	require.NotContains(t, gone, fieldExpiration)

	s.fails("Encrypt", map[string]any{fieldKeyID: id, fieldPlaintext: b64([]byte("x"))}, codeKMSInvalid, pendingImport)
	s.fails(opDecrypt, map[string]any{fieldCiphertextBlob: b64(blob)}, codeKMSInvalid, pendingImport)

	t.Run("other material after a delete is refused", func(t *testing.T) {
		r := s.call(usEast1, opImport, params.input(t, id, random(t, 32)))
		require.Equal(t, "IncorrectKeyMaterialException", r.errorCode(), "%v", r.body)
		require.Equal(t, statePending, s.metadata(id)[fieldKeyState])
	})

	t.Run("the same material after a delete is current again", func(t *testing.T) {
		s.ok(opImport, params.input(t, id, material))
		require.Equal(t, []byte("secret"), s.decrypted(usEast1, blob))
		require.Equal(t, after[fieldMaterialID], s.metadata(id)[fieldMaterialID])
	})
}

// Test_import_pendingRotation is test_import_key_material_raises_if_there_is_
// already_key_material_pending and the two on-demand rotation tests of
// external keys: new material waits for RotateKeyOnDemand, a third is
// refused while it waits, and after the rotation the old blobs still decrypt.
func Test_import_pendingRotation(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(map[string]any{fieldOrigin: originExternal})
	id, keyARN := keyID(t, meta), arn(t, meta)
	params := s.importParams(id, oaepSHA256)

	s.ok(opImport, params.input(t, id, random(t, 32)))
	first := s.materialID(id)

	s.fails(opRotate, map[string]any{fieldKeyID: id}, codeKMSInvalid,
		"No available key material pending rotation for the key: "+keyARN+".")

	old := s.encrypt(id, []byte("old"), nil)

	in := params.input(t, id, random(t, 32))
	in[fieldImportType] = "NEW_KEY_MATERIAL"
	second, ok := s.ok(opImport, in)[fieldKeyMaterialID].(string)
	require.True(t, ok)
	require.NotEqual(t, first, second)
	require.Equal(t, first, s.materialID(id), "new material waits for the rotation")

	third := params.input(t, id, random(t, 32))
	r := s.call(usEast1, opImport, third)
	require.Equal(t, codeKMSInvalid, r.errorCode())
	require.Regexp(t, `^New key material \(id: [0-9a-f]{64}\) cannot be imported into KMS key `+keyARN+
		`, because another key material \(id: `+second+`\) is pending rotation\.$`, r.message())

	s.ok(opRotate, map[string]any{fieldKeyID: id})
	require.Equal(t, second, s.materialID(id))

	require.Equal(t, []byte("old"), s.decrypted(usEast1, old))
	require.Equal(t, []byte("new"), s.decrypted(usEast1, s.encrypt(id, []byte("new"), nil)))
}

// Test_import_asymmetric is test_import_key_asymmetric and
// test_import_key_ecc_keys: an ECC key pair wrapped with RSAES_OAEP_SHA_1
// replaces the key's own, GetPublicKey answers its public key and it signs.
func Test_import_asymmetric(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(map[string]any{fieldOrigin: originExternal, fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})
	id := keyID(t, meta)

	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)

	public, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	require.NoError(t, err)

	params := s.importParams(id, oaepSHA1)

	out := s.ok(opImport, params.input(t, id, der))
	require.NotContains(t, out, fieldKeyMaterialID, "only a symmetric key shows its material id")
	require.NotContains(t, s.metadata(id), fieldMaterialID)
	require.Equal(t, public, blobOf(t, s.ok(opGetPublicKey, map[string]any{fieldKeyID: id}), fieldPublicKey))

	message := b64([]byte("test sign verify"))
	sign := map[string]any{
		fieldKeyID: id, fieldMessage: message, fieldMessageType: typeRaw, fieldSigningAlgorithm: ecdsaSHA256,
	}
	signature := blobOf(t, s.ok(opSign, sign), fieldSignature)

	digest := sha256.Sum256([]byte("test sign verify"))
	require.True(t, ecdsa.VerifyASN1(&private.PublicKey, digest[:], signature), "signed with the imported key")

	s.ok(opDeleteImport, map[string]any{fieldKeyID: id})
	require.Equal(t, statePending, s.metadata(id)[fieldKeyState])
	require.Equal(t, codeKMSInvalid, s.call(usEast1, opSign, sign).errorCode())

	t.Run("a key of another curve is refused", func(t *testing.T) {
		other, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		require.NoError(t, err)

		der, err := x509.MarshalPKCS8PrivateKey(other)
		require.NoError(t, err)

		r := s.call(usEast1, opImport, params.input(t, id, der))
		require.Equal(t, "IncorrectKeyMaterialException", r.errorCode(), "%v", r.body)
	})
}

// Test_import_rsaAESKeyWrap is test_import_key_rsa_aes_wrap_sha256: an RSA
// private key under an AES key under the wrapping key.
func Test_import_rsaAESKeyWrap(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{
		fieldOrigin: originExternal, fieldKeySpec: rsa2048, fieldKeyUsage: usageEncryptDecrypt,
	}))

	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)

	public, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	require.NoError(t, err)

	params := s.importParams(id, aesWrapSHA256)
	s.ok(opImport, params.input(t, id, der))
	require.Equal(t, "Enabled", s.metadata(id)[fieldKeyState])
	require.Equal(t, public, blobOf(t, s.ok(opGetPublicKey, map[string]any{fieldKeyID: id}), fieldPublicKey))

	// Decrypt of an offline OAEP blob uses the imported private key.
	offline, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &private.PublicKey, []byte("offline"), nil)
	require.NoError(t, err)

	out := s.ok(opDecrypt, map[string]any{
		fieldKeyID: id, fieldCiphertextBlob: b64(offline), "EncryptionAlgorithm": oaepSHA256,
	})
	require.Equal(t, b64([]byte("offline")), out[fieldPlaintext])

	t.Run("a blob too short for the wrapped AES key", func(t *testing.T) {
		in := params.input(t, id, der)
		in[fieldWrapped] = b64(make([]byte, 256))

		require.Equal(t, "InvalidCiphertextException", s.call(usEast1, opImport, in).errorCode())
	})
}

// Test_import_hmac is test_import_key_hmac_keys: the imported bytes are the
// MAC key.
func Test_import_hmac(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(map[string]any{
		fieldOrigin: originExternal, fieldKeySpec: hmac256, fieldKeyUsage: usageGenerateMAC,
	}))

	params := s.importParams(id, oaepSHA256)
	material := random(t, 32)
	s.ok(opImport, params.input(t, id, material))

	mac := hmac.New(sha256.New, material)
	mac.Write([]byte("test-mac-message"))

	out := s.ok("GenerateMac", map[string]any{
		fieldKeyID: id, fieldMessage: b64([]byte("test-mac-message")), fieldMacAlgorithm: alg256,
	})
	require.Equal(t, mac.Sum(nil), blobOf(t, out, fieldMac))

	r := s.call(usEast1, opImport, params.input(t, id, random(t, 16)))
	require.Equal(t, "IncorrectKeyMaterialException", r.errorCode(), "16 bytes is under HMAC_256's 32")
}

// Test_import_errors holds the refusals around an import.
func Test_import_errors(t *testing.T) {
	s := newStack(t)
	awsKey := s.createKey(nil)
	external := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
	other := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))

	notExternal := arn(t, awsKey) + " origin is AWS_KMS which is not valid for this operation."

	s.fails(opParams, map[string]any{fieldKeyID: keyID(t, awsKey), fieldWrapAlgorithm: oaepSHA256, fieldWrapSpec: rsa2048},
		codeUnsupported, notExternal)
	s.fails(opDeleteImport, map[string]any{fieldKeyID: keyID(t, awsKey)}, codeUnsupported, notExternal)

	s.fails(opParams, map[string]any{fieldKeyID: external, fieldWrapAlgorithm: "RSA_OAEP", fieldWrapSpec: rsa2048},
		codeValidation, "1 validation error detected: Value 'RSA_OAEP' at 'wrappingAlgorithm' failed to satisfy "+
			"constraint: Member must satisfy enum value set: [RSAES_PKCS1_V1_5, RSAES_OAEP_SHA_1, RSAES_OAEP_SHA_256, "+
			"RSA_AES_KEY_WRAP_SHA_1, RSA_AES_KEY_WRAP_SHA_256, SM2PKE]")
	s.fails(opParams, map[string]any{fieldKeyID: external, fieldWrapAlgorithm: oaepSHA256, fieldWrapSpec: "RSA_1024"},
		codeValidation, "1 validation error detected: Value 'RSA_1024' at 'wrappingKeySpec' failed to satisfy "+
			"constraint: Member must satisfy enum value set: [RSA_2048, RSA_3072, RSA_4096, SM2]")
	s.fails(opParams, map[string]any{fieldKeyID: external, fieldWrapAlgorithm: oaepSHA256, fieldWrapSpec: "SM2"},
		codeUnsupported, "KeySpec SM2 is not supported")

	params := s.importParams(external, oaepSHA256)

	in := params.input(t, external, random(t, 32))
	in[fieldImportToken] = b64([]byte("nope"))
	s.fails(opImport, in, "NotFoundException", "Unable to find key import token 'nope'")

	r := s.call(usEast1, opImport, params.input(t, other, random(t, 32)))
	require.Equal(t, "InvalidImportTokenException", r.errorCode(), "a token is for one key")

	in = params.input(t, external, random(t, 32))
	in[fieldWrapped] = b64(bytes.Repeat([]byte{1}, 256))
	require.Equal(t, "InvalidCiphertextException", s.call(usEast1, opImport, in).errorCode())

	r = s.call(usEast1, opImport, params.input(t, external, random(t, 16)))
	require.Equal(t, "IncorrectKeyMaterialException", r.errorCode(), "symmetric material is 256 bits")

	in = params.input(t, external, random(t, 32))
	in[fieldExpiration] = "SOMETIMES"
	require.Equal(t, codeValidation, s.call(usEast1, opImport, in).errorCode())

	require.Equal(t, statePending, s.metadata(external)[fieldKeyState], "no refused import changed the key")
}
