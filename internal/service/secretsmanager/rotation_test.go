package secretsmanager_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	functionARN = "arn:aws:lambda:us-east-1:000000000000:function:rotator"
	token4      = "44444444-4444-4444-4444-444444444444"
	awsCurrent  = "AWSCURRENT"
)

// fakeLambda answers GetFunction for one function and 404 for any other.
func fakeLambda() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/functions/"+functionARN) {
			_, _ = w.Write([]byte(`{}`))

			return
		}

		w.WriteHeader(http.StatusNotFound)
	})
}

func Test_resource_policy(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"p","SecretString":"v","ClientRequestToken":"`+token1+`"}`)

	got := mustCall(t, h, "GetResourcePolicy", `{"SecretId":"p"}`)
	assert.NotContains(t, got, "ResourcePolicy")

	mustCall(t, h, "PutResourcePolicy", `{"SecretId":"p","ResourcePolicy":"{\"Version\":\"2012-10-17\"}"}`)
	got = mustCall(t, h, "GetResourcePolicy", `{"SecretId":"p"}`)
	policy, ok := got["ResourcePolicy"].(string)
	require.True(t, ok, "%v", got)
	assert.JSONEq(t, `{"Version":"2012-10-17"}`, policy)
	assert.Equal(t, "p", got["Name"])

	requireError(t, h, "PutResourcePolicy", `{"SecretId":"p"}`, "ValidationException")
	requireError(t, h, "PutResourcePolicy", `{"SecretId":"no such","ResourcePolicy":"{}"}`, "ValidationException")
	requireError(t, h, "GetResourcePolicy", `{"SecretId":"missing"}`, "ResourceNotFoundException")

	mustCall(t, h, "DeleteResourcePolicy", `{"SecretId":"p"}`)
	got = mustCall(t, h, "GetResourcePolicy", `{"SecretId":"p"}`)
	assert.NotContains(t, got, "ResourcePolicy")
}

func Test_rotate_secret_checks(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"r","SecretString":"v","ClientRequestToken":"`+token1+`"}`)

	// The raw body test_no_client_request_token sends: members RotateSecret
	// does not have, and no token.
	out := requireError(t, h, "RotateSecret",
		`{"SecretId":"r","SecretString":"thisisthesecret","Description":"My secret string"}`,
		"InvalidParameterException")
	assert.Equal(t, "You must provide a ClientRequestToken value. We recommend a UUID-type value.", out["Message"])

	tok := `"ClientRequestToken":"` + token2 + `"`
	requireError(t, h, "RotateSecret", `{"SecretId":"bad name",`+tok+`}`, "ValidationException")
	requireError(t, h, "RotateSecret", `{"SecretId":"missing",`+tok+`}`, "ResourceNotFoundException")

	out = requireError(t, h, "RotateSecret",
		`{"SecretId":"r","RotationRules":{"AutomaticallyAfterDays":1},`+tok+`}`, "InvalidRequestException")
	assert.Equal(t, "No Lambda rotation function ARN is associated with this secret.", out["Message"])

	long := `"RotationLambdaARN":"` + strings.Repeat("a", 2049) + `"`
	requireError(t, h, "RotateSecret", `{"SecretId":"r",`+long+`,`+tok+`}`, "InvalidParameterException")
	requireError(t, h, "RotateSecret",
		`{"SecretId":"r","RotationLambdaARN":"`+functionARN+`","RotationRules":{"AutomaticallyAfterDays":1001},`+tok+`}`,
		"InvalidParameterException")

	// Lambda is not running, so the function cannot be found and nothing changes.
	out = requireError(t, h, "RotateSecret",
		`{"SecretId":"r","RotationLambdaARN":"`+functionARN+`","RotationRules":{"AutomaticallyAfterDays":1},`+tok+`}`,
		"ResourceNotFoundException")
	assert.Equal(t, "Lambda does not exist or could not be accessed", out["Message"])

	described := mustCall(t, h, describeSecret, `{"SecretId":"r"}`)
	assert.NotContains(t, described, "RotationEnabled")
	assert.NotContains(t, described, "RotationRules")
	assert.NotContains(t, described, "RotationLambdaARN")

	requireError(t, h, "CancelRotateSecret", `{"SecretId":"r"}`, "InvalidRequestException")

	mustCall(t, h, "DeleteSecret", `{"SecretId":"r"}`)
	requireError(t, h, "RotateSecret", `{"SecretId":"r",`+tok+`}`, "InvalidRequestException")
}

func Test_rotate_secret_with_a_function(t *testing.T) {
	p := secretsmanager.New(nil)
	h := handlerFor(t, p)
	p.Registration().Bind(fakeLambda())

	mustCall(t, h, "CreateSecret", `{"Name":"r","SecretString":"v","ClientRequestToken":"`+token1+`"}`)

	out := mustCall(t, h, "RotateSecret", `{"SecretId":"r","RotationLambdaARN":"`+functionARN+`",`+
		`"RotationRules":{"AutomaticallyAfterDays":7},"ClientRequestToken":"`+token2+`"}`)
	assert.Equal(t, token2, out["VersionId"])

	described := mustCall(t, h, describeSecret, `{"SecretId":"r"}`)
	assert.Equal(t, true, described["RotationEnabled"])
	assert.Equal(t, functionARN, described["RotationLambdaARN"])
	assert.InDelta(t, 7, asMap(t, described["RotationRules"])["AutomaticallyAfterDays"], 0)
	assert.Contains(t, described, "NextRotationDate")
	assert.Contains(t, described, "LastRotatedDate")

	stages := asMap(t, described["VersionIdsToStages"])
	assert.Equal(t, []any{awsCurrent}, stages[token1])
	assert.Equal(t, []any{"AWSPENDING"}, stages[token2])

	// The pending version has no value until the function sets one.
	requireError(t, h, "GetSecretValue", `{"SecretId":"r","VersionStage":"AWSPENDING"}`, "ResourceNotFoundException")

	// A second rotation while that version is pending is refused, and so is
	// changing nothing.
	requireError(t, h, "RotateSecret", `{"SecretId":"r","ClientRequestToken":"`+token3+`"}`, "InvalidRequestException")

	// Not now: the stored function and period serve, and the current version
	// is named.
	out = mustCall(t, h, "RotateSecret", `{"SecretId":"r","RotateImmediately":false,"ClientRequestToken":"`+token4+`"}`)
	assert.Equal(t, token1, out["VersionId"])

	out = mustCall(t, h, "CancelRotateSecret", `{"SecretId":"r"}`)
	assert.Equal(t, token1, out["VersionId"])

	described = mustCall(t, h, describeSecret, `{"SecretId":"r"}`)
	assert.NotContains(t, described, "RotationEnabled")
	assert.Equal(t, functionARN, described["RotationLambdaARN"])

	requireError(t, h, "RotateSecret", `{"SecretId":"r","RotationLambdaARN":"arn:aws:lambda:us-east-1:0:function:x",`+
		`"ClientRequestToken":"`+token3+`"}`, "ResourceNotFoundException")
}
