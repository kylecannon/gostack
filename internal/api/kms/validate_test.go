package kms_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_ServeValidated_SnapshotMessages sends the input of each test_kms.py
// test whose snapshot pins a constraint message, and checks the generated
// checks answer that message. The key names the snapshot entry.
func Test_ServeValidated_SnapshotMessages(t *testing.T) {
	t.Parallel()

	plaintext := base64.StdEncoding.EncodeToString(make([]byte, 4097))

	cases := []struct {
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

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()

			code, body := serveJSON(t, c.op, c.body)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "ValidationException", body["__type"])
			assert.Equal(t, c.want, body["message"])
		})
	}
}

// Test_ServeValidated_ValidRequestReachesHandler checks a request that breaks
// no constraint passes the checks: Unimplemented answers it, not the checks.
func Test_ServeValidated_ValidRequestReachesHandler(t *testing.T) {
	t.Parallel()

	_, body := serveJSON(t, "CreateKey", `{"KeySpec":"HMAC_256","KeyUsage":"GENERATE_VERIFY_MAC",`+
		`"Tags":[{"TagKey":"k","TagValue":""}]}`)
	assert.NotEqual(t, "ValidationException", body["__type"])
}

func serveJSON(t *testing.T, op, payload string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)

	rec := httptest.NewRecorder()
	kms.ServeValidated(&kms.Unimplemented{}, rec, &request.Context{
		Service: "kms", Operation: op, Protocol: protocol.KindJSON, RequestID: "req-1", Request: req,
	})

	var body map[string]any

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())

	return rec.Code, body
}
