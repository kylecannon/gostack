package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	headerSSEAlgorithm = "X-Amz-Server-Side-Encryption"
	headerSSEKey       = "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"
	kmsAuthHeader      = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/kms/aws4_request, " +
		"SignedHeaders=host, Signature=x"
	kmsType = "application/x-amz-json-1.1"
	sseKMS  = "aws:kms"
	// managedKeyText is the description of the key S3 has KMS make for aws:kms
	// writes that name none.
	managedKeyText = "Default key that protects my S3 objects when no other key is defined"
)

// boundStack serves the real providers behind the real gateway, with S3 bound
// to them as in start.go, so S3's calls to KMS go through the in-process client
// to the real KMS provider.
type boundStack struct {
	t      *testing.T
	server *httptest.Server
}

// answer is a response reduced to what these tests read.
type answer struct {
	status int
	header http.Header
	body   []byte
}

func newBoundStack(t *testing.T) *boundStack {
	t.Helper()

	t.Setenv("S3_SKIP_KMS_KEY_VALIDATION", "0")

	cfg, err := config.Load()
	require.NoError(t, err)

	services := newProviders(cfg, func(string) {})

	gw, err := gateway.New(settingsFunc(cfg), services.regs, nil, nil)
	require.NoError(t, err)

	services.bind(gw.Handler())

	server := httptest.NewServer(gw.Handler())
	t.Cleanup(server.Close)

	return &boundStack{t: t, server: server}
}

func (b *boundStack) do(method, path string, header map[string]string, body string) answer {
	b.t.Helper()

	req, err := http.NewRequestWithContext(b.t.Context(), method, b.server.URL+path, strings.NewReader(body))
	require.NoError(b.t, err)

	for k, v := range header {
		req.Header.Set(k, v)
	}

	resp, err := b.server.Client().Do(req)
	require.NoError(b.t, err)

	data, err := io.ReadAll(resp.Body)
	require.NoError(b.t, err)
	require.NoError(b.t, resp.Body.Close())

	return answer{status: resp.StatusCode, header: resp.Header, body: data}
}

func (b *boundStack) s3(method, path string, header map[string]string) answer {
	b.t.Helper()

	h := map[string]string{headerAuthorization: s3AuthHeader}
	maps.Copy(h, header)

	return b.do(method, path, h, "body")
}

// kms makes one call to the real KMS and returns its answer.
func (b *boundStack) kms(operation, body string) answer {
	b.t.Helper()

	return b.do(http.MethodPost, "/", map[string]string{
		headerAuthorization: kmsAuthHeader, headerContentType: kmsType, headerTarget: "TrentService." + operation,
	}, body)
}

// kmsOK is kms for a call that must succeed, decoded into out.
func (b *boundStack) kmsOK(operation, body string, out any) {
	b.t.Helper()

	got := b.kms(operation, body)
	require.Equal(b.t, http.StatusOK, got.status, operation+": "+string(got.body))
	require.NoError(b.t, json.Unmarshal(got.body, out))
}

// keyCount is how many keys the account holds in us-east-1.
func (b *boundStack) keyCount() int {
	b.t.Helper()

	var out keyList

	b.kmsOK("ListKeys", `{}`, &out)

	return len(out.Keys)
}

// newKey makes a key of the test's own, returning its ARN.
func (b *boundStack) newKey() string {
	b.t.Helper()

	var out keyReply

	b.kmsOK("CreateKey", `{"Description":"a key of the test"}`, &out)

	return out.KeyMetadata.Arn
}

// putKMS writes an aws:kms object, naming key unless it is empty.
func (b *boundStack) putKMS(path, key string) answer {
	b.t.Helper()

	header := map[string]string{headerSSEAlgorithm: sseKMS}
	if key != "" {
		header[headerSSEKey] = key
	}

	return b.s3(http.MethodPut, path, header)
}

// keyList is the part of a ListKeys answer these tests read.
type keyList struct {
	Keys []keyEntry `json:"Keys"`
}

type keyEntry struct {
	KeyID string `json:"KeyId"`
}

// keyReply is the part of a CreateKey or DescribeKey answer these tests read.
type keyReply struct {
	KeyMetadata keyMetadata `json:"KeyMetadata"`
}

type keyMetadata struct {
	Arn         string `json:"Arn"`
	Description string `json:"Description"`
	Enabled     bool   `json:"Enabled"`
}

// refusal is the code and message of an S3 error answer.
func refusal(t *testing.T, got answer) (code, message string) {
	t.Helper()

	var failure struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}

	require.NoError(t, xml.Unmarshal(got.body, &failure), string(got.body))

	return failure.Code, failure.Message
}

// Test_s3_withTheRealKMS binds S3 to the real KMS provider, as start.go does,
// with S3_SKIP_KMS_KEY_VALIDATION off: the managed key an aws:kms write that
// names none gets is made once, and a key that is disabled or pending deletion
// is refused with what KMS's DescribeKey reports.
func Test_s3_withTheRealKMS(t *testing.T) {
	stack := newBoundStack(t)

	require.Equal(t, http.StatusOK, stack.s3(http.MethodPut, "/bucket", nil).status)
	require.Zero(t, stack.keyCount())

	t.Run("the managed key is made once", func(t *testing.T) {
		first := stack.putKMS("/bucket/a", "")
		require.Equal(t, http.StatusOK, first.status, string(first.body))

		managed := first.header.Get(headerSSEKey)
		require.NotEmpty(t, managed)
		assert.Equal(t, 1, stack.keyCount(), "CreateKey made the key")

		var described keyReply

		stack.kmsOK("DescribeKey", `{"KeyId":"`+managed+`"}`, &described)
		assert.Equal(t, managedKeyText, described.KeyMetadata.Description)
		assert.True(t, described.KeyMetadata.Enabled)

		second := stack.putKMS("/bucket/b", "")
		require.Equal(t, http.StatusOK, second.status, string(second.body))
		assert.Equal(t, managed, second.header.Get(headerSSEKey), "the same key for the next write")
		assert.Equal(t, 1, stack.keyCount(), "no second key")

		named := stack.putKMS("/bucket/c", managed)
		assert.Equal(t, http.StatusOK, named.status, "an enabled key KMS holds is taken: "+string(named.body))
	})

	t.Run("a key KMS does not have is refused", func(t *testing.T) {
		got := stack.putKMS("/bucket/d", "00000000-0000-0000-0000-000000000000")
		require.Equal(t, http.StatusBadRequest, got.status, string(got.body))

		code, _ := refusal(t, got)
		assert.Equal(t, "KMS.NotFoundException", code)
	})

	t.Run("a disabled key is refused by DescribeKey's answer, on a write and on GetObject", func(t *testing.T) {
		arn := stack.newKey()
		written := stack.putKMS("/bucket/e", arn)
		require.Equal(t, http.StatusOK, written.status, string(written.body))

		require.Equal(t, http.StatusOK, stack.kms("DisableKey", `{"KeyId":"`+arn+`"}`).status)

		put := stack.putKMS("/bucket/f", arn)
		require.Equal(t, http.StatusBadRequest, put.status, string(put.body))

		code, message := refusal(t, put)
		assert.Equal(t, "KMS.DisabledException", code)
		assert.Equal(t, arn+" is disabled.", message)

		read := stack.s3(http.MethodGet, "/bucket/e", nil)
		require.Equal(t, http.StatusBadRequest, read.status, string(read.body))

		code, message = refusal(t, read)
		assert.Equal(t, "KMS.DisabledException", code)
		assert.Equal(t, arn+" is disabled.", message)
	})

	t.Run("a key pending deletion is refused by DescribeKey's answer", func(t *testing.T) {
		arn := stack.newKey()
		require.Equal(t, http.StatusOK,
			stack.kms("ScheduleKeyDeletion", `{"KeyId":"`+arn+`","PendingWindowInDays":7}`).status)

		got := stack.putKMS("/bucket/g", arn)
		require.Equal(t, http.StatusBadRequest, got.status, string(got.body))

		code, message := refusal(t, got)
		assert.Equal(t, "KMS.KMSInvalidStateException", code)
		assert.Equal(t, arn+" is pending deletion.", message)
	})
}
