package kms_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/kms"
	"github.com/stretchr/testify/require"
)

const (
	testListen = "0.0.0.0:4566"
	account    = "000000000000"
	usEast1    = "us-east-1"
	usWest2    = "us-west-2"
	jsonType   = "application/x-amz-json-1.1"
)

// Member names, values and error codes the tests repeat.
const (
	fieldKeyID         = "KeyId"
	fieldDescription   = "Description"
	fieldKeySpec       = "KeySpec"
	fieldTags          = "Tags"
	fieldTagKey        = "TagKey"
	fieldAliasName     = "AliasName"
	fieldPolicyName    = "PolicyName"
	fieldNumberOfBytes = "NumberOfBytes"
	fieldTagValue      = "TagValue"
	fieldKeyUsage      = "KeyUsage"
	fieldTargetKeyID   = "TargetKeyId"
	fieldARN           = "Arn"
	fieldOrigin        = "Origin"
	fieldPolicy        = "Policy"

	usageGenerateMAC = "GENERATE_VERIFY_MAC"
	valueNew         = "new"
	aliasS3          = "alias/aws/s3"

	usageEncryptDecrypt = "ENCRYPT_DECRYPT"
	usageSignVerify     = "SIGN_VERIFY"
	rsa2048             = "RSA_2048"
	policyDefault       = "default"
	tag2                = "tag2"
	tag3                = "tag3"

	symmetricDefault = "SYMMETRIC_DEFAULT"
	hmac256          = "HMAC_256"
	hmac512          = "HMAC_512"
	aliasA           = "alias/a"
	tag1             = "tag1"
	tag4             = "tag4"
	value1           = "value1"

	codeValidation = "ValidationException"
	codeTag        = "TagException"
)

// stack is KMS behind the real gateway, called in process.
type stack struct {
	t       *testing.T
	handler http.Handler
	changes atomic.Int32
}

func newStack(t *testing.T) *stack {
	t.Helper()

	s := &stack{t: t}
	provider := kms.New(func(string) { s.changes.Add(1) })

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	s.handler = gw.Handler()

	return s
}

// reply is a parsed json response.
type reply struct {
	status int
	header http.Header
	body   map[string]any
}

// errorCode is the __type of an error reply.
func (r reply) errorCode() string {
	code, _ := r.body["__type"].(string)

	return code
}

func (r reply) message() string {
	msg, _ := r.body["message"].(string)

	return msg
}

// call sends one KMS operation as json 1.1 from region, signed by the access
// key "test".
func (s *stack) call(region, operation string, input map[string]any) reply {
	s.t.Helper()

	body, err := json.Marshal(input)
	require.NoError(s.t, err)

	req := httptest.NewRequestWithContext(s.t.Context(), http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Amz-Target", "TrentService."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/"+region+
		"/kms/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	resp := rec.Result()

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	out := reply{status: resp.StatusCode, header: resp.Header, body: map[string]any{}}
	if len(raw) > 0 {
		require.NoError(s.t, json.Unmarshal(raw, &out.body), string(raw))
	}

	return out
}

// ok sends a call in us-east-1 that must succeed.
func (s *stack) ok(operation string, input map[string]any) map[string]any {
	s.t.Helper()

	r := s.call(usEast1, operation, input)
	require.Equal(s.t, http.StatusOK, r.status, "%s: %v", operation, r.body)

	return r.body
}

// fails sends a call in us-east-1 that must fail with code and message.
func (s *stack) fails(operation string, input map[string]any, code, message string) {
	s.t.Helper()

	r := s.call(usEast1, operation, input)
	require.Equal(s.t, http.StatusBadRequest, r.status, "%s: %v", operation, r.body)
	require.Equal(s.t, code, r.errorCode())
	require.Equal(s.t, message, r.message())
}

// createKey makes a key in us-east-1 and returns its metadata.
func (s *stack) createKey(input map[string]any) map[string]any {
	s.t.Helper()

	meta, ok := s.ok("CreateKey", input)["KeyMetadata"].(map[string]any)
	require.True(s.t, ok)

	return meta
}

func keyID(t *testing.T, meta map[string]any) string {
	t.Helper()

	id, ok := meta[fieldKeyID].(string)
	require.True(t, ok)

	return id
}

func arn(t *testing.T, meta map[string]any) string {
	t.Helper()

	a, ok := meta[fieldARN].(string)
	require.True(t, ok)

	return a
}
