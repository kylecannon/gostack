package ssm_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

const (
	referencePrefix = "/aws/reference/secretsmanager/"
	secretValue     = "my_secret"
	regionEast      = "us-east-1"
	regionWest      = "eu-west-2"
	fieldSecretID   = "SecretId"
	fieldDecrypt    = "WithDecryption"
	keySourceResult = "SourceResult"
	keyARN          = "ARN"
	fieldToken      = "ClientRequestToken"
	secretVanished  = "vanished"
)

// tokens makes the ClientRequestToken every Secrets Manager write needs.
var tokens atomic.Int64

// newToken is a token of 32 characters, one for each call.
func newToken() string {
	return fmt.Sprintf("%032d", tokens.Add(1))
}

// joined is one gateway that serves SSM and Secrets Manager and binds them to
// each other, as the server binds them.
type joined struct {
	t   *testing.T
	url string
}

func newJoined(t *testing.T, services ...service.Registration) *joined {
	t.Helper()

	settings := func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} }

	g, err := gateway.New(settings, services, nil, nil)
	require.NoError(t, err)

	for i := range services {
		if services[i].Bind != nil {
			services[i].Bind(g.InternalHandler())
		}
	}

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)

	return &joined{t: t, url: server.URL}
}

// bothServices is SSM and Secrets Manager, the registrations of a server.
func bothServices() []service.Registration {
	return []service.Registration{ssm.New().Registration(), secretsmanager.New(nil).Registration()}
}

// do sends a json 1.1 call to service in region and returns the status and the
// decoded body.
func (j *joined) do(signing, target, region string, body any) (int, map[string]any) {
	j.t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(j.t, err)

	req, err := http.NewRequestWithContext(j.t.Context(), http.MethodPost, j.url+"/", bytes.NewReader(payload))
	require.NoError(j.t, err)

	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20240101/"+region+"/"+signing+
		"/aws4_request, SignedHeaders=host, Signature=x")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(j.t, err)

	defer func() { require.NoError(j.t, resp.Body.Close()) }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(j.t, err)

	out := map[string]any{}
	if len(raw) > 0 {
		require.NoError(j.t, json.Unmarshal(raw, &out), string(raw))
	}

	return resp.StatusCode, out
}

// secret sends a Secrets Manager call in us-east-1 that must succeed.
func (j *joined) secret(operation string, body any) map[string]any {
	j.t.Helper()

	status, out := j.do("secretsmanager", "secretsmanager."+operation, regionEast, body)
	require.Equal(j.t, http.StatusOK, status, "%s: %v", operation, out)

	return out
}

// parameter sends an SSM call in region and returns the status and the body.
func (j *joined) parameter(operation, region string, body any) (int, map[string]any) {
	j.t.Helper()

	return j.do("ssm", "AmazonSSM."+operation, region, body)
}

// parameterFails sends an SSM call that must fail with code and returns its message.
func (j *joined) parameterFails(operation, region string, body any, code string) string {
	j.t.Helper()

	status, out := j.parameter(operation, region, body)
	require.Equal(j.t, http.StatusBadRequest, status, "%s: %v", operation, out)
	require.Contains(j.t, out["__type"], code)

	msg, _ := out["message"].(string)
	if msg == "" {
		msg, _ = out["Message"].(string)
	}

	return msg
}

// createSecret makes a secret with value in us-east-1 and returns its ARN.
func (j *joined) createSecret(name, value string) string {
	j.t.Helper()

	out := j.secret("CreateSecret", obj{kName: name, "SecretString": value, fieldToken: newToken()})
	arn, _ := out[keyARN].(string)
	require.NotEmpty(j.t, arn)

	return arn
}

// sourceResult decodes the SourceResult of a parameter.
func sourceResult(t *testing.T, p map[string]any) map[string]any {
	t.Helper()

	text, ok := p[keySourceResult].(string)
	require.True(t, ok, "SourceResult in %v", p)

	var out map[string]any

	require.NoError(t, json.Unmarshal([]byte(text), &out))

	return out
}

func Test_secretReference_getParameter(t *testing.T) {
	j := newJoined(t, bothServices()...)
	arn := j.createSecret("test_secret-a", secretValue)
	name := referencePrefix + "test_secret-a"

	status, out := j.parameter(kGetParameter, regionEast, obj{kName: name, fieldDecrypt: true})
	require.Equal(t, http.StatusOK, status, "%v", out)

	got := param(out)
	require.Equal(t, name, got[kName])
	require.Equal(t, secretValue, got[kValue])
	require.Equal(t, "SecureString", got[kType])
	require.InDelta(t, 0, got[kVersion], 0)
	require.Equal(t, "text", got["DataType"])
	require.Equal(t, "arn:aws:ssm:us-east-1:000000000000:parameter/aws/reference/secretsmanager/test_secret-a",
		got[keyARN])

	modified, isNumber := got["LastModifiedDate"].(float64)
	require.True(t, isNumber, "LastModifiedDate in %v", got)
	require.InDelta(t, math.Trunc(modified), modified, 0, "whole seconds")

	source := sourceResult(t, got)
	require.Equal(t, "test_secret-a", source["name"])
	require.Equal(t, arn, source[keyARN])
	require.Equal(t, secretValue, source["secretString"])
	require.Contains(t, source, "versionId")
	require.Equal(t, []any{"AWSCURRENT"}, source["versionStages"])
	require.Equal(t, got["LastModifiedDate"], source["createdDate"])
	require.NotContains(t, source, "Name", "keys are lower case except ARN")
	require.NotContains(t, source, "CreatedDate")

	// The plain name is a parameter nothing made.
	j.parameterFails(kGetParameter, regionEast, obj{kName: "test_secret-a", fieldDecrypt: true}, kParameterNotFound)
}

func Test_secretReference_readsTheCurrentVersion(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("rolling", "one")
	j.secret("PutSecretValue", obj{fieldSecretID: "rolling", "SecretString": "two", fieldToken: newToken()})

	_, out := j.parameter(kGetParameter, regionEast, obj{kName: referencePrefix + "rolling", fieldDecrypt: true})
	require.Equal(t, "two", param(out)[kValue])
}

func Test_secretReference_nestedNamesAndARNs(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("team/app/db", "hunter2")

	_, out := j.parameter(kGetParameter, regionEast, obj{kName: referencePrefix + "team/app/db", fieldDecrypt: true})
	require.Equal(t, "hunter2", param(out)[kValue])

	byARN := "arn:aws:ssm:us-east-1:000000000000:parameter" + referencePrefix + "team/app/db"
	_, out = j.parameter(kGetParameter, regionEast, obj{kName: byARN, fieldDecrypt: true})
	require.Equal(t, "hunter2", param(out)[kValue])
	require.Equal(t, referencePrefix+"team/app/db", param(out)[kName])
}

func Test_secretReference_needsDecryption(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("guarded", secretValue)

	for _, body := range []obj{
		{kName: referencePrefix + "guarded"},
		{kName: referencePrefix + "guarded", fieldDecrypt: false},
		{kName: referencePrefix + "missing"},
	} {
		msg := j.parameterFails(kGetParameter, regionEast, body, kValidationException)
		require.Equal(t, "WithDecryption flag must be True for retrieving a Secret Manager secret.", msg)
	}
}

func Test_secretReference_missingAndDeletedSecrets(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("doomed", secretValue)
	j.secret("DeleteSecret", obj{fieldSecretID: "doomed", "RecoveryWindowInDays": 7})

	for _, name := range []string{"inexistent", "doomed", "team/missing"} {
		full := referencePrefix + name
		msg := j.parameterFails(kGetParameter, regionEast, obj{kName: full, fieldDecrypt: true}, kParameterNotFound)
		require.Equal(t, "An error occurred (ParameterNotFound) when referencing Secrets Manager: Secret "+
			full+" not found.", msg)
	}

	// The prefix alone is a parameter name like any other.
	j.parameterFails(kGetParameter, regionEast, obj{kName: strings.TrimSuffix(referencePrefix, "/")}, kParameterNotFound)
}

func Test_secretReference_callersAccountAndRegion(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("east-only", secretValue)

	_, out := j.parameter(kGetParameter, regionEast, obj{kName: referencePrefix + "east-only", fieldDecrypt: true})
	require.Equal(t, secretValue, param(out)[kValue])

	j.parameterFails(kGetParameter, regionWest, obj{kName: referencePrefix + "east-only", fieldDecrypt: true},
		kParameterNotFound)
}

func Test_secretReference_getParametersMixesParametersAndSecrets(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("test_secret_param", secretValue)

	status, _ := j.parameter(kPutParameter, regionEast, obj{kName: nameRooted, kValue: kLit3, kType: kString})
	require.Equal(t, http.StatusOK, status)

	status, _ = j.parameter(kPutParameter, regionEast, obj{
		kName: "/db/password", kValue: "plain", kType: "SecureString",
	})
	require.Equal(t, http.StatusOK, status)

	complete := referencePrefix + "test_secret_param"
	status, out := j.parameter(kGetParameters, regionEast, obj{
		fieldNames:   []string{nameX, complete, "inexistent_param", referencePrefix + "inexistent_secret", "/db/password"},
		fieldDecrypt: true,
	})
	require.Equal(t, http.StatusOK, status, "%v", out)

	found := list(out, kParameters)
	require.Len(t, found, 3)

	require.Equal(t, nameX, entry(found, 0)[kName], "the requested spelling")
	require.Equal(t, kLit3, entry(found, 0)[kValue])

	require.Equal(t, complete, entry(found, 1)[kName])
	require.Equal(t, secretValue, entry(found, 1)[kValue])
	require.Equal(t, "test_secret_param", sourceResult(t, entry(found, 1))["name"])

	require.Equal(t, "/db/password", entry(found, 2)[kName])
	require.Equal(t, "plain", entry(found, 2)[kValue], "WithDecryption reaches the parameters of the same call")

	require.Equal(t, []any{"inexistent_param", referencePrefix + "inexistent_secret"}, list(out, kInvalidParameters))
}

func Test_secretReference_getParametersOfADeletedSecretIsInvalid(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret(secretVanished, secretValue)
	j.secret("DeleteSecret", obj{fieldSecretID: secretVanished, "ForceDeleteWithoutRecovery": true})

	_, out := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{referencePrefix + secretVanished}})
	require.Empty(t, list(out, kParameters))
	require.Equal(t, []any{referencePrefix + secretVanished}, list(out, kInvalidParameters))
}

func Test_secretReference_getParametersChecksNamesAndTheLimit(t *testing.T) {
	j := newJoined(t, bothServices()...)

	msg := j.parameterFails(kGetParameters, regionEast,
		obj{fieldNames: []string{referencePrefix + "x", "a//b"}}, kValidationException)
	require.Contains(t, msg, "sub-paths divided by slash symbol")

	names := make([]string, 0, 11)
	names = append(names, referencePrefix+"x")

	for range 10 {
		names = append(names, nameX)
	}

	msg = j.parameterFails(kGetParameters, regionEast, obj{fieldNames: names}, kValidationException)
	require.Contains(t, msg, "Member must have length less than or equal to 10.")
}

func Test_secretReference_withoutSecretsManager(t *testing.T) {
	j := newJoined(t, ssm.New().Registration())

	j.parameterFails(kGetParameter, regionEast, obj{kName: referencePrefix + "any", fieldDecrypt: true},
		kParameterNotFound)

	_, out := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{referencePrefix + "any"}})
	require.Equal(t, []any{referencePrefix + "any"}, list(out, kInvalidParameters))
}

func Test_secretReference_unboundProviderFindsNoSecret(t *testing.T) {
	c := newClient(t, ssm.New())

	c.fails(kGetParameter, obj{kName: referencePrefix + "any", fieldDecrypt: true}, kParameterNotFound)
}

func Test_secretReference_aParameterNamedLikeASecretPathIsAParameter(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("x", secretValue)

	// Only /aws/reference/secretsmanager/ makes a reference.
	status, _ := j.parameter(kPutParameter, regionEast,
		obj{kName: "/ref/a/secretsmanager/x", kValue: kLit3, kType: kString})
	require.Equal(t, http.StatusOK, status)

	_, out := j.parameter(kGetParameter, regionEast, obj{kName: "/ref/a/secretsmanager/x"})
	require.Equal(t, kLit3, param(out)[kValue])
}

func Test_secretReference_isNotMutating(t *testing.T) {
	reg := ssm.New().Registration()
	require.False(t, reg.Mutating(kGetParameter))
	require.False(t, reg.Mutating(kGetParameters))
	require.NotNil(t, reg.Bind, "SSM declares the call through Bind")
}
