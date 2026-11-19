package kms_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_CreateKey(t *testing.T) {
	t.Run("a symmetric key shows what DescribeKey shows, on the 1.1 wire", func(t *testing.T) {
		s := newStack(t)

		r := s.call(usEast1, "CreateKey", map[string]any{"Description": "test key 123", "KeyUsage": "ENCRYPT_DECRYPT"})
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, jsonType, r.header.Get("Content-Type"))

		meta, ok := r.body["KeyMetadata"].(map[string]any)
		require.True(t, ok)

		id := keyID(t, meta)
		require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id)
		require.Regexp(t, `^[0-9a-f]{64}$`, meta["CurrentKeyMaterialId"])
		require.NotNil(t, meta["CreationDate"])

		for _, k := range []string{"KeyId", "CurrentKeyMaterialId", "CreationDate"} {
			delete(meta, k)
		}

		require.Equal(t, map[string]any{
			"AWSAccountId":          account,
			"Arn":                   "arn:aws:kms:us-east-1:" + account + ":key/" + id,
			"CustomerMasterKeySpec": "SYMMETRIC_DEFAULT",
			"Description":           "test key 123",
			"Enabled":               true,
			"EncryptionAlgorithms":  []any{"SYMMETRIC_DEFAULT"},
			"KeyManager":            "CUSTOMER",
			"KeySpec":               "SYMMETRIC_DEFAULT",
			"KeyState":              "Enabled",
			"KeyUsage":              "ENCRYPT_DECRYPT",
			"MultiRegion":           false,
			"Origin":                "AWS_KMS",
		}, meta)
	})

	t.Run("a multi-Region key has an mrk- id and an empty replica list", func(t *testing.T) {
		s := newStack(t)

		meta := s.createKey(map[string]any{"MultiRegion": true})
		id := keyID(t, meta)
		require.Regexp(t, `^mrk-[0-9a-f]{32}$`, id)
		require.Equal(t, map[string]any{
			"MultiRegionKeyType": "PRIMARY",
			"PrimaryKey":         map[string]any{"Arn": arn(t, meta), "Region": usEast1},
			"ReplicaKeys":        []any{},
		}, meta["MultiRegionConfiguration"])
	})

	t.Run("each spec lists its algorithms", func(t *testing.T) {
		s := newStack(t)

		tests := []struct {
			name      string
			giveInput map[string]any
			wantKey   string
			wantAlgs  []any
		}{
			{
				name:      "HMAC_256",
				giveInput: map[string]any{"KeySpec": "HMAC_256", "KeyUsage": "GENERATE_VERIFY_MAC"},
				wantKey:   "MacAlgorithms",
				wantAlgs:  []any{"HMAC_SHA_256"},
			},
			{
				name:      "ECC_NIST_P256 through CustomerMasterKeySpec",
				giveInput: map[string]any{"CustomerMasterKeySpec": "ECC_NIST_P256", "KeyUsage": "SIGN_VERIFY"},
				wantKey:   "SigningAlgorithms",
				wantAlgs:  []any{"ECDSA_SHA_256"},
			},
			{
				name:      "RSA_2048 for encryption",
				giveInput: map[string]any{"KeySpec": "RSA_2048"},
				wantKey:   "EncryptionAlgorithms",
				wantAlgs:  []any{"RSAES_OAEP_SHA_1", "RSAES_OAEP_SHA_256"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				meta := s.createKey(tt.giveInput)
				require.Equal(t, tt.wantAlgs, meta[tt.wantKey])
				require.NotContains(t, meta, "CurrentKeyMaterialId")
			})
		}
	})

	t.Run("an external key waits for its material", func(t *testing.T) {
		s := newStack(t)

		meta := s.createKey(map[string]any{"Origin": "EXTERNAL"})
		require.Equal(t, "PendingImport", meta["KeyState"])
		require.Equal(t, false, meta["Enabled"])
		require.NotContains(t, meta, "CurrentKeyMaterialId")
	})

	t.Run("the custom tags set the id and the material", func(t *testing.T) {
		s := newStack(t)

		const custom = "5d6f1c0e-8a43-4b3e-9a0b-1f7f3b2e9c41"

		material := base64.StdEncoding.EncodeToString(make([]byte, 32))
		meta := s.createKey(map[string]any{"Tags": []map[string]string{
			{"TagKey": "_custom_id_", "TagValue": custom},
			{"TagKey": "_custom_key_material_", "TagValue": material},
		}})
		require.Equal(t, custom, keyID(t, meta))
		require.Contains(t, meta, "CurrentKeyMaterialId")

		tags := s.ok("ListResourceTags", map[string]any{"KeyId": custom})["Tags"]
		require.Equal(t, []any{map[string]any{"TagKey": "_custom_id_", "TagValue": custom}}, tags)
	})
}

func Test_CreateKey_errors(t *testing.T) {
	long := strings.Repeat("a", 129)
	fiftyOne := make([]map[string]string, 0, 51)

	for i := range 51 {
		fiftyOne = append(fiftyOne, map[string]string{"TagKey": "key" + strconv.Itoa(i), "TagValue": "v"})
	}

	tests := []struct {
		name        string
		giveInput   map[string]any
		wantCode    string
		wantMessage string
	}{
		{
			name:        "an HMAC key without a usage",
			giveInput:   map[string]any{"KeySpec": "HMAC_256"},
			wantCode:    "ValidationException",
			wantMessage: "You must specify a KeyUsage value for all KMS keys except for symmetric encryption keys.",
		},
		{
			name:      "an unknown HMAC spec",
			giveInput: map[string]any{"KeySpec": "HMAC_random"},
			wantCode:  "ValidationException",
			wantMessage: "1 validation error detected: Value 'HMAC_random' at 'keySpec' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [RSA_2048, ECC_NIST_P384, ECC_NIST_P256, ECC_NIST_P521, HMAC_384, " +
				"RSA_3072, ECC_SECG_P256K1, RSA_4096, SYMMETRIC_DEFAULT, HMAC_256, HMAC_224, HMAC_512]",
		},
		{
			name:      "an HMAC key with an unknown usage",
			giveInput: map[string]any{"KeySpec": "HMAC_256", "KeyUsage": "RANDOM"},
			wantCode:  "ValidationException",
			wantMessage: "1 validation error detected: Value 'RANDOM' at 'keyUsage' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [ENCRYPT_DECRYPT, SIGN_VERIFY, GENERATE_VERIFY_MAC]",
		},
		{
			name:        "a spec of no known family",
			giveInput:   map[string]any{"KeySpec": "ML_DSA_44"},
			wantCode:    "UnsupportedOperationException",
			wantMessage: "KeySpec ML_DSA_44 is not supported",
		},
		{
			name:        "key agreement with an RSA spec",
			giveInput:   map[string]any{"KeySpec": "RSA_2048", "KeyUsage": "KEY_AGREEMENT"},
			wantCode:    "ValidationException",
			wantMessage: "KeyUsage KEY_AGREEMENT is not compatible with KeySpec RSA_2048",
		},
		{
			name: "duplicate tag keys before their lengths",
			giveInput: map[string]any{"Tags": []map[string]string{
				{"TagKey": long, "TagValue": "1"}, {"TagKey": long, "TagValue": "2"},
			}},
			wantCode:    "TagException",
			wantMessage: "Duplicate tag keys",
		},
		{
			name:        "more than 50 tags",
			giveInput:   map[string]any{"Tags": fiftyOne},
			wantCode:    "TagException",
			wantMessage: "Too many tags",
		},
		{
			name: "a long tag key, at its place in the request, before a bad spec",
			giveInput: map[string]any{"KeySpec": "HMAC_random", "Tags": []map[string]string{
				{"TagKey": "fine", "TagValue": "1"}, {"TagKey": long, "TagValue": "2"},
			}},
			wantCode: "ValidationException",
			wantMessage: "1 validation error detected: Value '" + long + "' at 'tags.2.member.tagKey' failed to " +
				"satisfy constraint: Member must have length less than or equal to 128",
		},
		{
			name:      "a long tag value",
			giveInput: map[string]any{"Tags": []map[string]string{{"TagKey": "tag1", "TagValue": strings.Repeat("v", 257)}}},
			wantCode:  "ValidationException",
			wantMessage: "1 validation error detected: Value '" + strings.Repeat("v", 257) + "' at " +
				"'tags.1.member.tagValue' failed to satisfy constraint: Member must have length less than or equal to 256",
		},
		{
			name:        "an aws: tag key in any case",
			giveInput:   map[string]any{"Tags": []map[string]string{{"TagKey": "AWS:key1", "TagValue": "value1"}}},
			wantCode:    "TagException",
			wantMessage: "Tags beginning with aws: are reserved",
		},
	}

	s := newStack(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.fails("CreateKey", tt.giveInput, tt.wantCode, tt.wantMessage)
		})
	}

	t.Run("secp256k1 waits for its dependency", func(t *testing.T) {
		r := s.call(usEast1, "CreateKey", map[string]any{"KeySpec": "ECC_SECG_P256K1", "KeyUsage": "SIGN_VERIFY"})
		require.Equal(t, http.StatusNotImplemented, r.status)
	})

	t.Run("refused keys are not listed", func(t *testing.T) {
		require.Equal(t, []any{}, s.ok("ListKeys", nil)["Keys"])
	})
}
