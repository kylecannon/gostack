package secretsmanager_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/require"
)

const token1 = "11111111-1111-1111-1111-111111111111"

// call sends one Secrets Manager json call to h and returns the status and the
// decoded body.
func call(t *testing.T, h http.Handler, operation, body string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/us-east-1/secretsmanager/aws4_request, "+
		"SignedHeaders=host;x-amz-date, Signature=abc")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	raw, err := io.ReadAll(rec.Result().Body)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded), string(raw))

	return rec.Code, decoded
}

func newHandler(t *testing.T) http.Handler {
	t.Helper()

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{secretsmanager.New(nil).Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	return gw.Handler()
}

func Test_valueLifecycle(t *testing.T) {
	h := newHandler(t)

	status, created := call(t, h, "CreateSecret", `{"Name":"s1","SecretString":"one","ClientRequestToken":"`+token1+`"}`)
	require.Equal(t, http.StatusOK, status, created)
	require.Equal(t, token1, created["VersionId"])

	status, put := call(t, h, "PutSecretValue",
		`{"SecretId":"s1","SecretString":"two","ClientRequestToken":"22222222-2222-2222-2222-222222222222"}`)
	require.Equal(t, http.StatusOK, status, put)
	require.Equal(t, []any{awsCurrent}, put["VersionStages"])

	_, previous := call(t, h, "GetSecretValue", `{"SecretId":"s1","VersionStage":"AWSPREVIOUS"}`)
	require.Equal(t, "one", previous["SecretString"])

	_, described := call(t, h, "DescribeSecret", `{"SecretId":"s1"}`)
	require.Contains(t, described, "LastAccessedDate")
	require.NotContains(t, described, "Tags")

	status, missing := call(t, h, "CreateSecret", `{"Name":"s2"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "InvalidRequestException", missing["__type"])

	status, deleted := call(t, h, "DeleteSecret", `{"SecretId":"s1"}`)
	require.Equal(t, http.StatusOK, status, deleted)

	_, listed := call(t, h, "ListSecrets", `{}`)
	require.Empty(t, listed["SecretList"])

	status, gone := call(t, h, "GetSecretValue", `{"SecretId":"s1"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "InvalidRequestException", gone["__type"])
}
