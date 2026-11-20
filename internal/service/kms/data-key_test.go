package kms_test

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"maps"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fieldKeyPairSpec = "KeyPairSpec"
	aes256Spec       = "AES_256"
)

func Test_GenerateDataKey(t *testing.T) {
	tests := []struct {
		name      string
		giveInput map[string]any
		wantSize  int
	}{
		{name: aes256Spec, giveInput: map[string]any{fieldKeySpec: aes256Spec}, wantSize: 32},
		{name: "AES_128", giveInput: map[string]any{fieldKeySpec: "AES_128"}, wantSize: 16},
		{name: "NumberOfBytes", giveInput: map[string]any{fieldNumberOfBytes: 7}, wantSize: 7},
		{name: "neither, as LocalStack's cross-account test sends", giveInput: map[string]any{}, wantSize: 32},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStack(t)
			meta := s.createKey(nil)
			context := map[string]string{"purpose": "test"}

			input := map[string]any{fieldKeyID: keyID(t, meta), fieldEncryptionContext: context}
			maps.Copy(input, tt.giveInput)

			out := s.ok("GenerateDataKey", input)
			require.Equal(t, arn(t, meta), out[fieldKeyID])

			plaintext := blobOf(t, out, fieldPlaintext)
			require.Len(t, plaintext, tt.wantSize)

			blob := blobOf(t, out, fieldCiphertextBlob)
			opened := s.ok("Decrypt", map[string]any{fieldCiphertextBlob: b64(blob), fieldEncryptionContext: context})
			require.Equal(t, plaintext, blobOf(t, opened, fieldPlaintext))

			s.failsCiphertext("Decrypt", map[string]any{fieldKeyID: keyID(t, meta), fieldCiphertextBlob: b64(blob)})

			without := s.ok("GenerateDataKeyWithoutPlaintext", input)
			require.NotContains(t, without, fieldPlaintext)

			opened = s.ok("Decrypt", map[string]any{
				fieldCiphertextBlob: b64(blobOf(t, without, fieldCiphertextBlob)), fieldEncryptionContext: context,
			})
			require.Len(t, blobOf(t, opened, fieldPlaintext), tt.wantSize)

			require.Zero(t, s.changes.Load())
		})
	}
}

func Test_GenerateDataKey_errors(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))
	signKey := s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify})

	tests := []struct {
		name        string
		giveInput   map[string]any
		wantCode    string
		wantMessage string
	}{
		{
			name:        "both KeySpec and NumberOfBytes",
			giveInput:   map[string]any{fieldKeyID: id, fieldKeySpec: aes256Spec, fieldNumberOfBytes: 32},
			wantCode:    codeValidation,
			wantMessage: "Please specify either number of bytes or key spec.",
		},
		{
			name:      "an unknown KeySpec",
			giveInput: map[string]any{fieldKeyID: id, fieldKeySpec: "AES_512"},
			wantCode:  codeValidation,
			wantMessage: "1 validation error detected: Value 'AES_512' at 'keySpec' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [AES_256, AES_128]",
		},
		{
			name:      "NumberOfBytes above 1024",
			giveInput: map[string]any{fieldKeyID: id, fieldNumberOfBytes: 1025},
			wantCode:  codeValidation,
			wantMessage: "1 validation error detected: Value '1025' at 'numberOfBytes' failed to satisfy constraint: " +
				"Member must have value less than or equal to 1024",
		},
		{
			name:      "NumberOfBytes below 1",
			giveInput: map[string]any{fieldKeyID: id, fieldNumberOfBytes: 0},
			wantCode:  codeValidation,
			wantMessage: "1 validation error detected: Value '0' at 'numberOfBytes' failed to satisfy constraint: " +
				"Member must have value greater than or equal to 1",
		},
		{
			name:        "a signing key, before the size",
			giveInput:   map[string]any{fieldKeyID: keyID(t, signKey), fieldNumberOfBytes: 5000},
			wantCode:    codeInvalidKeyUsage,
			wantMessage: arn(t, signKey) + " key usage is SIGN_VERIFY which is not valid for GenerateDataKey.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(*testing.T) {
			s.fails("GenerateDataKey", tt.giveInput, tt.wantCode, tt.wantMessage)
		})
	}
}

func Test_GenerateDataKeyPair(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	context := map[string]string{"pair": "yes"}

	for _, spec := range []string{rsa2048, "RSA_3072", eccP256, "ECC_NIST_P384", "ECC_NIST_P521"} {
		t.Run(spec, func(t *testing.T) {
			input := map[string]any{fieldKeyID: keyID(t, meta), fieldKeyPairSpec: spec, fieldEncryptionContext: context}

			out := s.ok("GenerateDataKeyPair", input)
			require.Equal(t, arn(t, meta), out[fieldKeyID])
			require.Equal(t, spec, out[fieldKeyPairSpec])

			private := blobOf(t, out, "PrivateKeyPlaintext")
			parsed, err := x509.ParsePKCS8PrivateKey(private)
			require.NoError(t, err)

			public, err := x509.ParsePKIXPublicKey(blobOf(t, out, "PublicKey"))
			require.NoError(t, err)

			switch key := parsed.(type) {
			case *rsa.PrivateKey:
				require.True(t, key.PublicKey.Equal(public))
			case *ecdsa.PrivateKey:
				require.True(t, key.PublicKey.Equal(public))
			default:
				t.Fatalf("a %T private key", parsed)
			}

			opened := s.ok("Decrypt", map[string]any{
				fieldCiphertextBlob: b64(blobOf(t, out, "PrivateKeyCiphertextBlob")), fieldEncryptionContext: context,
			})
			require.Equal(t, private, blobOf(t, opened, fieldPlaintext))

			without := s.ok("GenerateDataKeyPairWithoutPlaintext", input)
			require.NotContains(t, without, "PrivateKeyPlaintext")
			require.Contains(t, without, "PrivateKeyCiphertextBlob")
			require.Contains(t, without, "PublicKey")
		})
	}

	require.Zero(t, s.changes.Load())
}

func Test_GenerateDataKeyPair_errors(t *testing.T) {
	s := newStack(t)
	id := keyID(t, s.createKey(nil))

	for _, op := range []string{"GenerateDataKeyPair", "GenerateDataKeyPairWithoutPlaintext"} {
		t.Run(op, func(t *testing.T) {
			s.fails(op, map[string]any{fieldKeyID: id, fieldKeyPairSpec: rsa2048, "DryRun": true},
				"DryRunOperationException", "The request would have succeeded, but the DryRun option is set.")
			s.fails(op, map[string]any{fieldKeyID: id}, codeValidation,
				"1 validation error detected: Value null at 'keyPairSpec' failed to satisfy constraint: "+
					"Member must not be null")
			s.fails(op, map[string]any{fieldKeyID: id, fieldKeyPairSpec: symmetricDefault}, codeValidation,
				"1 validation error detected: Value 'SYMMETRIC_DEFAULT' at 'keyPairSpec' failed to satisfy "+
					"constraint: Member must satisfy enum value set: [RSA_2048, RSA_3072, RSA_4096, ECC_NIST_P256, "+
					"ECC_NIST_P384, ECC_NIST_P521, ECC_SECG_P256K1, SM2, ECC_NIST_EDWARDS25519]")
			s.fails(op, map[string]any{fieldKeyID: id, fieldKeyPairSpec: "SM2"},
				"UnsupportedOperationException", "KeySpec SM2 is not supported")

			// The secp256k1 pair is made with the dependency; its DER is checked
			// in Test_GenerateDataKeyPair_secp256k1.
			r := s.call(usEast1, op, map[string]any{fieldKeyID: id, fieldKeyPairSpec: "ECC_SECG_P256K1"})
			require.Equal(t, http.StatusOK, r.status)
		})
	}
}
