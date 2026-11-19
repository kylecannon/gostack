package kms_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	tagCustomID       = "_custom_id_"
	tagCustomMaterial = "_custom_key_material_"
	materialIDPattern = `^[0-9a-f]{64}$`
)

func customMaterialTag(material []byte) map[string]string {
	return map[string]string{
		fieldTagKey: tagCustomMaterial, fieldTagValue: base64.StdEncoding.EncodeToString(material),
	}
}

func Test_CreateKey_emptyUsage(t *testing.T) {
	s := newStack(t)

	// LocalStack: request_key_usage or "ENCRYPT_DECRYPT".
	meta := s.createKey(map[string]any{fieldKeyUsage: ""})
	require.Equal(t, usageEncryptDecrypt, meta[fieldKeyUsage])
	require.Equal(t, []any{symmetricDefault}, meta["EncryptionAlgorithms"])

	meta = s.createKey(map[string]any{fieldKeySpec: ecdsaP256, fieldKeyUsage: ""})
	require.Equal(t, usageEncryptDecrypt, meta[fieldKeyUsage])

	// An HMAC key still has to name its usage.
	s.fails("CreateKey", map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: ""}, codeValidation,
		"1 validation error detected: Value '' at 'keyUsage' failed to satisfy constraint: "+
			"Member must satisfy enum value set: [ENCRYPT_DECRYPT, SIGN_VERIFY, GENERATE_VERIFY_MAC]")
}

const ecdsaP256 = "ECC_NIST_P256"

func Test_CreateKey_customMaterial(t *testing.T) {
	s := newStack(t)

	t.Run("an empty value makes the key's own material, as LocalStack does", func(t *testing.T) {
		empty := []map[string]string{customMaterialTag(nil)}

		symmetric := s.createKey(map[string]any{fieldTags: empty})
		require.Regexp(t, materialIDPattern, symmetric["CurrentKeyMaterialId"])

		hmac := s.createKey(map[string]any{fieldTags: empty, fieldKeySpec: hmac256, fieldKeyUsage: usageGenerateMAC})
		require.NotContains(t, hmac, "CurrentKeyMaterialId")

		rsaKey := s.createKey(map[string]any{fieldTags: empty, fieldKeySpec: rsa2048})
		require.Equal(t, "Enabled", rsaKey["KeyState"])
	})

	t.Run("a symmetric key takes an AES key size and no other", func(t *testing.T) {
		for _, n := range []int{16, 24, 32} {
			meta := s.createKey(map[string]any{fieldTags: []map[string]string{customMaterialTag(make([]byte, n))}})
			require.Regexp(t, materialIDPattern, meta["CurrentKeyMaterialId"], n)
		}

		for _, n := range []int{1, 5, 15, 33, 64} {
			r := s.call(usEast1, "CreateKey", map[string]any{
				fieldTags: []map[string]string{customMaterialTag(make([]byte, n))},
			})
			require.Equal(t, 400, r.status, n)
			require.Equal(t, codeValidation, r.errorCode(), n)
			require.Contains(t, r.message(), "must be 16, 24 or 32 bytes", n)
		}

		require.Len(t, s.ok("ListKeys", nil)["Keys"], 3+3, "the refused keys are not made")
	})

	t.Run("an HMAC key takes any length, as LocalStack's test gives one 24 bytes", func(t *testing.T) {
		meta := s.createKey(map[string]any{
			fieldTags:    []map[string]string{customMaterialTag([]byte("custom test key material"))},
			fieldKeySpec: hmac256, fieldKeyUsage: usageGenerateMAC,
		})
		require.Contains(t, meta, "CurrentKeyMaterialId")
	})

	t.Run("an asymmetric key takes a private key of its size and curve", func(t *testing.T) {
		rsa2048Key, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)

		p256Key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		der := func(key any) []byte {
			t.Helper()

			out, err := x509.MarshalPKCS8PrivateKey(key)
			require.NoError(t, err)

			return out
		}

		create := func(spec string, key any) reply {
			return s.call(usEast1, "CreateKey", map[string]any{
				fieldKeySpec: spec, fieldKeyUsage: usageSignVerify,
				fieldTags: []map[string]string{customMaterialTag(der(key))},
			})
		}

		require.Equal(t, 200, create(rsa2048, rsa2048Key).status)
		require.Equal(t, 200, create(ecdsaP256, p256Key).status)

		for _, tt := range []struct {
			name, spec string
			give       any
			want       string
		}{
			{
				"a smaller RSA key", "RSA_3072", rsa2048Key,
				"The custom key material is a 2048-bit RSA key, but the key spec is RSA_3072",
			},
			{
				"another curve", "ECC_NIST_P384", p256Key,
				"The custom key material is an EC key of the P-256 curve, but the key spec is ECC_NIST_P384",
			},
			{
				"an EC key for an RSA spec", rsa2048, p256Key,
				"The custom key material is an EC key of the P-256 curve, but the key spec is RSA_2048",
			},
			{
				"an RSA key for an EC spec", ecdsaP256, rsa2048Key,
				"The custom key material is a 2048-bit RSA key, but the key spec is ECC_NIST_P256",
			},
		} {
			r := create(tt.spec, tt.give)
			require.Equal(t, 400, r.status, tt.name)
			require.Equal(t, codeValidation, r.errorCode(), tt.name)
			require.Equal(t, tt.want, r.message(), tt.name)
		}
	})
}

// Test_CreateKey_customID holds a key made with the id of one that exists to
// LocalStack's tag store, which is by ARN and apart from the keys: the new key
// replaces the old, and the tags of both are kept, the new values winning.
func Test_CreateKey_customID(t *testing.T) {
	s := newStack(t)

	const custom = "5d6f1c0e-8a43-4b3e-9a0b-1f7f3b2e9c41"

	idTag := map[string]string{fieldTagKey: tagCustomID, fieldTagValue: custom}

	s.createKey(map[string]any{fieldTags: []map[string]string{
		idTag, {fieldTagKey: tag1, fieldTagValue: value1}, {fieldTagKey: tag2, fieldTagValue: "old"},
	}})
	s.ok("TagResource", map[string]any{fieldKeyID: custom, fieldTags: []map[string]string{
		{fieldTagKey: tag3, fieldTagValue: "added"},
	}})

	again := s.createKey(map[string]any{fieldTags: []map[string]string{
		idTag, {fieldTagKey: tag2, fieldTagValue: valueNew}, {fieldTagKey: tag4, fieldTagValue: "fresh"},
	}})
	require.Equal(t, custom, keyID(t, again))
	require.Len(t, s.ok("ListKeys", nil)["Keys"], 1, "the key is replaced, not added")

	require.Equal(t, []any{
		map[string]any{fieldTagKey: tagCustomID, fieldTagValue: custom},
		map[string]any{fieldTagKey: tag1, fieldTagValue: value1},
		map[string]any{fieldTagKey: tag2, fieldTagValue: valueNew},
		map[string]any{fieldTagKey: tag3, fieldTagValue: "added"},
		map[string]any{fieldTagKey: tag4, fieldTagValue: "fresh"},
	}, s.ok("ListResourceTags", map[string]any{fieldKeyID: custom})[fieldTags])

	// With no tags in the request, LocalStack leaves the stored ones as they were.
	s.createKey(map[string]any{fieldTags: []map[string]string{idTag}})
	require.Len(t, s.ok("ListResourceTags", map[string]any{fieldKeyID: custom})[fieldTags], 5)
}

// Test_reservedAlias_key holds the key made for a reserved alias to a key
// CreateKey makes of an empty request, as LocalStack's _create_kms_key does:
// the AWS_KMS origin and a material id, no description, the CUSTOMER manager.
func Test_reservedAlias_key(t *testing.T) {
	s := newStack(t)

	reserved, ok := s.ok("DescribeKey", map[string]any{fieldKeyID: aliasS3})["KeyMetadata"].(map[string]any)
	require.True(t, ok)

	plain := s.createKey(map[string]any{})

	require.Equal(t, "AWS_KMS", reserved[fieldOrigin])
	require.Regexp(t, materialIDPattern, reserved["CurrentKeyMaterialId"])
	require.Equal(t, "CUSTOMER", reserved["KeyManager"])
	require.Empty(t, reserved[fieldDescription])

	for _, drop := range []string{fieldKeyID, fieldARN, "CreationDate", "CurrentKeyMaterialId"} {
		delete(reserved, drop)
		delete(plain, drop)
	}

	require.Equal(t, plain, reserved, "a reserved alias's key is a default key")
}
