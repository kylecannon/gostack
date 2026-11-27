package kms_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

func Test_CreateKey(t *testing.T) {
	t.Run("a symmetric key shows what DescribeKey shows, on the 1.1 wire", func(t *testing.T) {
		s := newStack(t)

		r := s.call(usEast1, "CreateKey", map[string]any{
			fieldDescription: "test key 123", fieldKeyUsage: usageEncryptDecrypt,
		})
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, []string{jsonType}, r.header.Values("Content-Type"))

		meta, ok := r.body["KeyMetadata"].(map[string]any)
		require.True(t, ok)

		id := keyID(t, meta)
		require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id)
		require.Regexp(t, `^[0-9a-f]{64}$`, meta["CurrentKeyMaterialId"])
		require.NotNil(t, meta["CreationDate"])

		for _, k := range []string{fieldKeyID, "CurrentKeyMaterialId", "CreationDate"} {
			delete(meta, k)
		}

		require.Equal(t, map[string]any{
			"AWSAccountId":          account,
			fieldARN:                "arn:aws:kms:us-east-1:" + account + ":key/" + id,
			"CustomerMasterKeySpec": symmetricDefault,
			fieldDescription:        "test key 123",
			"Enabled":               true,
			"EncryptionAlgorithms":  []any{symmetricDefault},
			"KeyManager":            "CUSTOMER",
			fieldKeySpec:            symmetricDefault,
			"KeyState":              "Enabled",
			fieldKeyUsage:           usageEncryptDecrypt,
			"MultiRegion":           false,
			fieldOrigin:             "AWS_KMS",
		}, meta)
	})

	t.Run("a multi-Region key has an mrk- id and an empty replica list", func(t *testing.T) {
		s := newStack(t)

		meta := s.createKey(map[string]any{"MultiRegion": true})
		id := keyID(t, meta)
		require.Regexp(t, `^mrk-[0-9a-f]{32}$`, id)
		require.Equal(t, map[string]any{
			"MultiRegionKeyType": "PRIMARY",
			"PrimaryKey":         map[string]any{fieldARN: arn(t, meta), "Region": usEast1},
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
				name:      hmac256,
				giveInput: map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: usageGenerateMAC},
				wantKey:   "MacAlgorithms",
				wantAlgs:  []any{"HMAC_SHA_256"},
			},
			{
				name:      "ECC_NIST_P256 through CustomerMasterKeySpec",
				giveInput: map[string]any{"CustomerMasterKeySpec": "ECC_NIST_P256", fieldKeyUsage: usageSignVerify},
				wantKey:   "SigningAlgorithms",
				wantAlgs:  []any{"ECDSA_SHA_256"},
			},
			{
				name:      "RSA_2048 for encryption",
				giveInput: map[string]any{fieldKeySpec: rsa2048},
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

		meta := s.createKey(map[string]any{fieldOrigin: "EXTERNAL"})
		require.Equal(t, "PendingImport", meta["KeyState"])
		require.Equal(t, false, meta["Enabled"])
		require.NotContains(t, meta, "CurrentKeyMaterialId")
	})

	t.Run("the custom tags set the id and the material", func(t *testing.T) {
		s := newStack(t)

		const custom = "5d6f1c0e-8a43-4b3e-9a0b-1f7f3b2e9c41"

		material := base64.StdEncoding.EncodeToString(make([]byte, 32))
		meta := s.createKey(map[string]any{fieldTags: []map[string]string{
			{fieldTagKey: "_custom_id_", fieldTagValue: custom},
			{fieldTagKey: "_custom_key_material_", fieldTagValue: material},
		}})
		require.Equal(t, custom, keyID(t, meta))
		require.Contains(t, meta, "CurrentKeyMaterialId")

		tags := s.ok("ListResourceTags", map[string]any{fieldKeyID: custom})[fieldTags]
		require.Equal(t, []any{map[string]any{fieldTagKey: "_custom_id_", fieldTagValue: custom}}, tags)
	})
}

func Test_CreateKey_errors(t *testing.T) {
	long := strings.Repeat("a", 129)
	fiftyOne := make([]map[string]string, 0, 51)

	for i := range 51 {
		fiftyOne = append(fiftyOne, map[string]string{fieldTagKey: "key" + strconv.Itoa(i), fieldTagValue: "v"})
	}

	tests := []struct {
		name        string
		giveInput   map[string]any
		wantCode    string
		wantMessage string
	}{
		{
			name:        "an HMAC key without a usage",
			giveInput:   map[string]any{fieldKeySpec: hmac256},
			wantCode:    codeValidation,
			wantMessage: "You must specify a KeyUsage value for all KMS keys except for symmetric encryption keys.",
		},
		{
			name:      "an unknown HMAC spec",
			giveInput: map[string]any{fieldKeySpec: "HMAC_random"},
			wantCode:  codeValidation,
			wantMessage: "1 validation error detected: Value 'HMAC_random' at 'keySpec' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [RSA_2048, ECC_NIST_P384, ECC_NIST_P256, ECC_NIST_P521, HMAC_384, " +
				"RSA_3072, ECC_SECG_P256K1, RSA_4096, SYMMETRIC_DEFAULT, HMAC_256, HMAC_224, HMAC_512]",
		},
		{
			name:      "an HMAC key with an unknown usage",
			giveInput: map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: "RANDOM"},
			wantCode:  codeValidation,
			wantMessage: "1 validation error detected: Value 'RANDOM' at 'keyUsage' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [ENCRYPT_DECRYPT, SIGN_VERIFY, GENERATE_VERIFY_MAC]",
		},
		{
			name:        "a spec of no known family",
			giveInput:   map[string]any{fieldKeySpec: "ML_DSA_44"},
			wantCode:    "UnsupportedOperationException",
			wantMessage: "KeySpec ML_DSA_44 is not supported",
		},
		{
			name:        "key agreement with an RSA spec",
			giveInput:   map[string]any{fieldKeySpec: rsa2048, fieldKeyUsage: "KEY_AGREEMENT"},
			wantCode:    codeValidation,
			wantMessage: "KeyUsage KEY_AGREEMENT is not compatible with KeySpec RSA_2048",
		},
		{
			name: "duplicate tag keys before their lengths",
			giveInput: map[string]any{fieldTags: []map[string]string{
				{fieldTagKey: long, fieldTagValue: "1"}, {fieldTagKey: long, fieldTagValue: "2"},
			}},
			wantCode:    codeTag,
			wantMessage: "Duplicate tag keys",
		},
		{
			name:        "more than 50 tags",
			giveInput:   map[string]any{fieldTags: fiftyOne},
			wantCode:    codeTag,
			wantMessage: "Too many tags",
		},
		{
			name: "a long tag key, at its place in the request, before a bad spec",
			giveInput: map[string]any{fieldKeySpec: "HMAC_random", fieldTags: []map[string]string{
				{fieldTagKey: "fine", fieldTagValue: "1"}, {fieldTagKey: long, fieldTagValue: "2"},
			}},
			wantCode: codeValidation,
			wantMessage: "1 validation error detected: Value '" + long + "' at 'tags.2.member.tagKey' failed to " +
				"satisfy constraint: Member must have length less than or equal to 128",
		},
		{
			name: "a long tag value",
			giveInput: map[string]any{
				fieldTags: []map[string]string{{fieldTagKey: tag1, fieldTagValue: strings.Repeat("v", 257)}},
			},
			wantCode: codeValidation,
			wantMessage: "1 validation error detected: Value '" + strings.Repeat("v", 257) + "' at " +
				"'tags.1.member.tagValue' failed to satisfy constraint: Member must have length less than or equal to 256",
		},
		{
			name:        "an aws: tag key in any case",
			giveInput:   map[string]any{fieldTags: []map[string]string{{fieldTagKey: "AWS:key1", fieldTagValue: value1}}},
			wantCode:    codeTag,
			wantMessage: "Tags beginning with aws: are reserved",
		},
	}

	s := newStack(t)

	for _, tt := range tests {
		t.Run(tt.name, func(_ *testing.T) {
			s.fails("CreateKey", tt.giveInput, tt.wantCode, tt.wantMessage)
		})
	}

	t.Run("an unknown usage of a non-HMAC spec is kept, as LocalStack keeps it", func(t *testing.T) {
		meta := newStack(t).createKey(map[string]any{fieldKeyUsage: "RANDOM"})
		require.Equal(t, "RANDOM", meta[fieldKeyUsage])
		require.NotContains(t, meta, "EncryptionAlgorithms")
	})

	t.Run("refused keys are not listed", func(t *testing.T) {
		require.Equal(t, []any{}, s.ok("ListKeys", nil)["Keys"])
	})

	t.Run("secp256k1 is made with the dependency", func(t *testing.T) {
		meta := s.createKey(map[string]any{fieldKeySpec: "ECC_SECG_P256K1", fieldKeyUsage: usageSignVerify})
		require.Equal(t, []any{"ECDSA_SHA_256"}, meta["SigningAlgorithms"])
	})
}

// Test_CreateKey_awsManaged is LocalStack's set_key_managed, which DynamoDB
// calls on the key it makes: an internal call that names the AWS manager makes
// the key AWS managed, and a client that sends the same header does not.
func Test_CreateKey_awsManaged(t *testing.T) {
	const manager = "X-Gostack-Key-Manager"

	t.Run("an internal call that names the AWS manager makes an AWS managed key", func(t *testing.T) {
		s := newStack(t)
		outer := s.handler
		s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set(manager, "AWS")
			outer.ServeHTTP(w, r.WithContext(request.MarkInternal(r.Context())))
		})

		meta := s.createKey(map[string]any{fieldDescription: "managed"})
		require.Equal(t, "AWS", meta["KeyManager"])
		require.Equal(t, "managed", meta[fieldDescription])

		described, ok := s.ok("DescribeKey", map[string]any{fieldKeyID: keyID(t, meta)})["KeyMetadata"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "AWS", described["KeyManager"])
	})

	t.Run("an internal call that names no manager, or another, makes a customer key", func(t *testing.T) {
		s := newStack(t)
		outer := s.handler
		value := ""
		s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if value != "" {
				r.Header.Set(manager, value)
			}

			outer.ServeHTTP(w, r.WithContext(request.MarkInternal(r.Context())))
		})

		for _, value = range []string{"", "CUSTOMER", "aws"} {
			require.Equal(t, "CUSTOMER", s.createKey(map[string]any{})["KeyManager"], value)
		}
	})

	t.Run("a call from outside the process that sends the header makes a customer key", func(t *testing.T) {
		s := newStack(t)
		outer := s.handler
		s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set(manager, "AWS")
			outer.ServeHTTP(w, r)
		})

		require.Equal(t, "CUSTOMER", s.createKey(map[string]any{})["KeyManager"])
	})
}
