package kms_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validating stands in for a handler that runs the generated checks first, as a
// real one does where LocalStack makes them. Nothing else answers: a request
// that passes reaches Unimplemented.
type validating struct{ kms.Unimplemented }

func (v validating) CreateKey(ctx context.Context, in *kms.CreateKeyInput) (*kms.CreateKeyOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.CreateKey(ctx, in)
}

func (v validating) DeriveSharedSecret(
	ctx context.Context, in *kms.DeriveSharedSecretInput,
) (*kms.DeriveSharedSecretOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.DeriveSharedSecret(ctx, in)
}

func (v validating) Encrypt(ctx context.Context, in *kms.EncryptInput) (*kms.EncryptOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.Encrypt(ctx, in)
}

func (v validating) GenerateMac(ctx context.Context, in *kms.GenerateMacInput) (*kms.GenerateMacOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.GenerateMac(ctx, in)
}

func (v validating) GenerateRandom(
	ctx context.Context, in *kms.GenerateRandomInput,
) (*kms.GenerateRandomOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.GenerateRandom(ctx, in)
}

func (v validating) VerifyMac(ctx context.Context, in *kms.VerifyMacInput) (*kms.VerifyMacOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.VerifyMac(ctx, in)
}

// dropsKeyMaterial is the CreateKey handler LocalStack has: it takes the
// _custom_key_material_ tag out of the request, whatever its value, and checks
// the tags that are left.
type dropsKeyMaterial struct{ validating }

func (d dropsKeyMaterial) CreateKey(ctx context.Context, in *kms.CreateKeyInput) (*kms.CreateKeyOutput, error) {
	in.Tags = slices.DeleteFunc(in.Tags, func(tag kms.Tag) bool {
		return tag.TagKey != nil && *tag.TagKey == "_custom_key_material_"
	})

	return d.validating.CreateKey(ctx, in)
}

// Test_Validate_SnapshotMessages sends the input of each test_kms.py test
// whose snapshot pins a constraint message through a handler that calls
// Validate, and checks the generated checks answer that message. The key names
// the snapshot entry.
func Test_Validate_SnapshotMessages(t *testing.T) {
	t.Parallel()

	plaintext := base64.StdEncoding.EncodeToString(make([]byte, 4097))

	tests := []struct {
		key  string
		op   string
		body string
		want string
	}{
		{
			key:  "test_generate_random_invalid_number_of_bytes[0] generate-random-exc",
			op:   "GenerateRandom",
			body: `{"NumberOfBytes":0}`,
			want: "1 validation error detected: Value '0' at 'numberOfBytes' failed to satisfy constraint: " +
				"Member must have value greater than or equal to 1",
		},
		{
			key:  "test_generate_random_invalid_number_of_bytes[1025] generate-random-exc",
			op:   "GenerateRandom",
			body: `{"NumberOfBytes":1025}`,
			want: "1 validation error detected: Value '1025' at 'numberOfBytes' failed to satisfy constraint: " +
				"Member must have value less than or equal to 1024",
		},
		{
			key:  "test_hmac_create_key_invalid_operations create-hmac-key-invalid-spec",
			op:   "CreateKey",
			body: `{"KeySpec":"HMAC_random","KeyUsage":"GENERATE_VERIFY_MAC"}`,
			want: "1 validation error detected: Value 'HMAC_random' at 'keySpec' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [RSA_2048, ECC_NIST_P384, ECC_NIST_P256, ECC_NIST_P521, " +
				"HMAC_384, RSA_3072, ECC_SECG_P256K1, RSA_4096, SYMMETRIC_DEFAULT, HMAC_256, HMAC_224, HMAC_512]",
		},
		{
			key:  "test_hmac_create_key_invalid_operations create-hmac-key-invalid-key-usage",
			op:   "CreateKey",
			body: `{"KeySpec":"HMAC_256","KeyUsage":"RANDOM"}`,
			want: "1 validation error detected: Value 'RANDOM' at 'keyUsage' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [ENCRYPT_DECRYPT, SIGN_VERIFY, GENERATE_VERIFY_MAC]",
		},
		{
			key:  "test_invalid_generate_mac[HMAC_256-INVALID] generate-mac",
			op:   "GenerateMac",
			body: `{"KeyId":"k","Message":"c29tZQ==","MacAlgorithm":"INVALID"}`,
			want: "1 validation error detected: Value 'INVALID' at 'macAlgorithm' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [HMAC_SHA_384, HMAC_SHA_256, HMAC_SHA_224, HMAC_SHA_512]",
		},
		{
			key:  "test_invalid_verify_mac[HMAC_256-INVALID-some important message] verify-mac",
			op:   "VerifyMac",
			body: `{"KeyId":"k","Message":"c29tZQ==","MacAlgorithm":"INVALID","Mac":"c29tZQ=="}`,
			want: "1 validation error detected: Value 'INVALID' at 'macAlgorithm' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [HMAC_SHA_384, HMAC_SHA_256, HMAC_SHA_224, HMAC_SHA_512]",
		},
		{
			key:  "test_plaintext_size_for_encrypt invalid-plaintext-size-encrypt",
			op:   "Encrypt",
			body: `{"KeyId":"k","Plaintext":"` + plaintext + `"}`,
			want: "1 validation error detected: Value at 'plaintext' failed to satisfy constraint: " +
				"Member must have length less than or equal to 4096",
		},
		{
			key:  "test_derive_shared_secret response-invalid-key-agreement-algo",
			op:   "DeriveSharedSecret",
			body: `{"KeyId":"k","KeyAgreementAlgorithm":"INVALID","PublicKey":"c29tZQ=="}`,
			want: "1 validation error detected: Value 'INVALID' at 'keyAgreementAlgorithm' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [ECDH]",
		},
		{
			key:  "test_key_with_long_tag_value_raises_error too-long-tag-value",
			op:   "CreateKey",
			body: `{"Tags":[{"TagKey":"tag1","TagValue":"` + strings.Repeat("v", 257) + `"}]}`,
			want: "1 validation error detected: Value '" + strings.Repeat("v", 257) + "' at 'tags.1.member.tagValue' " +
				"failed to satisfy constraint: Member must have length less than or equal to 256",
		},
		{
			key:  "test_create_key_with_invalid_tag_key[too_long_key] invalid-tag-key",
			op:   "CreateKey",
			body: `{"Tags":[{"TagKey":"` + strings.Repeat("a", 129) + `","TagValue":"value1"}]}`,
			want: "1 validation error detected: Value '" + strings.Repeat("a", 129) + "' at 'tags.1.member.tagKey' " +
				"failed to satisfy constraint: Member must have length less than or equal to 128",
		},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()

			code, body := serveJSON(t, validating{}, tt.op, tt.body)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "ValidationException", body["__type"])
			assert.Equal(t, tt.want, body["message"])
		})
	}
}

// Test_Validate_ValidRequestReachesHandler checks a request that breaks no
// constraint passes Validate: Unimplemented answers it, not the checks.
func Test_Validate_ValidRequestReachesHandler(t *testing.T) {
	t.Parallel()

	_, body := serveJSON(t, validating{}, "CreateKey", `{"KeySpec":"HMAC_256","KeyUsage":"GENERATE_VERIFY_MAC",`+
		`"Tags":[{"TagKey":"k","TagValue":""}]}`)
	assert.NotEqual(t, "ValidationException", body["__type"])
}

// Test_ServeValidated_NeverChecks sends requests that break listed
// constraints to a handler that does not call Validate: ServeValidated runs no
// check of its own, so each one reaches the handler.
func Test_ServeValidated_NeverChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, op, body string
	}{
		{"a number of bytes below the minimum", "GenerateRandom", `{"NumberOfBytes":0}`},
		{"a key usage outside the enum", "CreateKey", `{"KeyUsage":"RANDOM"}`},
		// The check a real handler makes is LocalStack's, and for ReplicateKey
		// it makes none: 129 characters pass.
		{"a 129-character tag key on ReplicateKey", "ReplicateKey",
			`{"KeyId":"mrk-1","ReplicaRegion":"us-west-2","Tags":[{"TagKey":"` + strings.Repeat("a", 129) + `"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, body := serveJSON(t, kms.Unimplemented{}, tt.op, tt.body)
			assert.Equal(t, http.StatusNotImplemented, code, body)
			assert.Contains(t, body["message"], "not yet implemented")
		})
	}
}

// Test_Validate_CustomKeyMaterialTag sends a CreateKey whose _custom_key_material_
// tag holds 1,600 characters, over the tag value max of 256. LocalStack takes
// that tag out before it checks, so the request is accepted; a handler that
// calls Validate without taking it out answers the length error.
func Test_Validate_CustomKeyMaterialTag(t *testing.T) {
	t.Parallel()

	body := `{"Tags":[{"TagKey":"_custom_key_material_","TagValue":"` + strings.Repeat("Q", 1600) + `"}]}`

	code, got := serveJSON(t, dropsKeyMaterial{}, "CreateKey", body)
	assert.Equal(t, http.StatusNotImplemented, code, got)
	assert.Contains(t, got["message"], "not yet implemented")

	code, got = serveJSON(t, validating{}, "CreateKey", body)
	assert.Equal(t, http.StatusBadRequest, code, got)
	assert.Contains(t, got["message"], "at 'tags.1.member.tagValue' failed to satisfy constraint: "+
		"Member must have length less than or equal to 256")
}

// Test_Validate_UnlistedConstraintsReachHandler sends requests that break a
// model constraint no snapshot pins, or that pass once length counts
// characters, and LocalStack accepts: the handler answers each one.
func Test_Validate_UnlistedConstraintsReachHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, op, body string
	}{
		// 65 characters, 130 UTF-16 units: within the tag key max of 128.
		{"a 65-emoji tag key", "CreateKey", `{"Tags":[{"TagKey":"` + strings.Repeat("😀", 65) + `","TagValue":""}]}`},
		{"a marker outside the model pattern", "ListAliases", `{"Marker":"Ā"}`},
		{"a grant name outside the model pattern", "CreateGrant",
			`{"KeyId":"k","GranteePrincipal":"p","Operations":["Decrypt"],"Name":"a b"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, body := serveJSON(t, validating{}, tt.op, tt.body)
			assert.Equal(t, http.StatusNotImplemented, code, body)
			assert.Contains(t, body["message"], "not yet implemented")
		})
	}
}

func serveJSON(t *testing.T, h kms.Handler, op, payload string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)

	rec := httptest.NewRecorder()
	kms.ServeValidated(h, rec, &request.Context{
		Service: "kms", Operation: op, Protocol: protocol.KindJSON, RequestID: "req-1", Request: req,
	})

	var body map[string]any

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())

	return rec.Code, body
}
