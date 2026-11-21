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
	referencePrefix   = "/aws/reference/secretsmanager/"
	secretValue       = "my_secret"
	regionEast        = "us-east-1"
	regionWest        = "eu-west-2"
	fieldSecretID     = "SecretId"
	fieldDecrypt      = "WithDecryption"
	keySourceResult   = "SourceResult"
	keyARN            = "ARN"
	keySelector       = "Selector"
	fieldVersionStage = "VersionStage"
	// idMin and idMax bound the length at which a selector may be a VersionId.
	idMin          = 32
	idMax          = 64
	fieldToken     = "ClientRequestToken"
	secretVanished = "vanished"

	fieldSecretString = "SecretString"
	valueOne          = "one"
	valueTwo          = "two"
	valueThree        = "three"
	idWithSpace       = "a b"
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

	out := j.secret("CreateSecret", obj{kName: name, fieldSecretString: value, fieldToken: newToken()})
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
	require.NotContains(t, got, "DataType", "no AWS source shows one on a reference")
	require.Equal(t, arn, got[keyARN], "the secret's ARN, as the AWS user guide shows")

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
	j.createSecret("rolling", valueOne)
	j.secret("PutSecretValue", obj{fieldSecretID: "rolling", fieldSecretString: valueTwo, fieldToken: newToken()})

	_, out := j.parameter(kGetParameter, regionEast, obj{kName: referencePrefix + "rolling", fieldDecrypt: true})
	require.Equal(t, valueTwo, param(out)[kValue])
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

// No AWS source refuses a reference without WithDecryption, so GetParameter and
// GetParameters both answer the secret either way, as LocalStack does.
func Test_secretReference_needsNoDecryption(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("mine", secretValue)

	name := referencePrefix + "mine"

	for _, body := range []obj{{kName: name}, {kName: name, fieldDecrypt: false}, {kName: name, fieldDecrypt: true}} {
		status, out := j.parameter(kGetParameter, regionEast, body)
		require.Equal(t, http.StatusOK, status, "%v", out)
		require.Equal(t, secretValue, param(out)[kValue])
	}

	for _, body := range []obj{
		{fieldNames: []string{name}},
		{fieldNames: []string{name}, fieldDecrypt: false},
		{fieldNames: []string{name}, fieldDecrypt: true},
	} {
		status, out := j.parameter(kGetParameters, regionEast, body)
		require.Equal(t, http.StatusOK, status, "%v", out)
		require.Equal(t, secretValue, entry(list(out, kParameters), 0)[kValue])
	}

	// A secret nothing holds is not found, with or without the flag.
	j.parameterFails(kGetParameter, regionEast, obj{kName: referencePrefix + "missing"}, kParameterNotFound)
}

// stagedSecret makes a secret with a version for AWSPREVIOUS, one for
// AWSCURRENT and one for a custom label, and returns their VersionIds, oldest
// first.
func stagedSecret(j *joined) (first, second, labeled string) {
	j.t.Helper()

	created := j.secret("CreateSecret", obj{kName: "s1", fieldSecretString: valueOne, fieldToken: newToken()})
	first, _ = created["VersionId"].(string)

	put := j.secret("PutSecretValue", obj{fieldSecretID: "s1", fieldSecretString: valueTwo, fieldToken: newToken()})
	second, _ = put["VersionId"].(string)

	put = j.secret("PutSecretValue", obj{
		fieldSecretID: "s1", fieldSecretString: valueThree, fieldToken: newToken(), "VersionStages": []string{"mine"},
	})
	labeled, _ = put["VersionId"].(string)

	return first, second, labeled
}

// A reference name takes a trailing selector, a staging label or a VersionId, as
// Parameter Store honors those of Secrets Manager.
func Test_secretReference_selectors(t *testing.T) {
	j := newJoined(t, bothServices()...)
	first, second, labeled := stagedSecret(j)

	for selector, want := range map[string]string{
		"AWSCURRENT":  valueTwo,
		"AWSPREVIOUS": valueOne,
		"mine":        valueThree,
		first:         valueOne,
		second:        valueTwo,
		labeled:       valueThree,
	} {
		name := referencePrefix + "s1:" + selector

		status, out := j.parameter(kGetParameter, regionEast, obj{kName: name, fieldDecrypt: true})
		require.Equal(t, http.StatusOK, status, "%s: %v", name, out)
		require.Equal(t, want, param(out)[kValue], name)
		require.Equal(t, referencePrefix+"s1", param(out)[kName], "the Name has no selector")
		require.Equal(t, ":"+selector, param(out)[keySelector], "the Selector has its colon")

		status, out = j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{name}, fieldDecrypt: true})
		require.Equal(t, http.StatusOK, status, "%s: %v", name, out)
		require.Equal(t, want, entry(list(out, kParameters), 0)[kValue], name)
		require.Equal(t, referencePrefix+"s1", entry(list(out, kParameters), 0)[kName])
		require.Equal(t, ":"+selector, entry(list(out, kParameters), 0)[keySelector])
		require.Empty(t, list(out, kInvalidParameters))
	}

	// A selector no version carries is no secret to read.
	for _, selector := range []string{"nolabel", "1", strings.Repeat("0", 32), "AWSPENDING"} {
		name := referencePrefix + "s1:" + selector

		j.parameterFails(kGetParameter, regionEast, obj{kName: name, fieldDecrypt: true}, kParameterNotFound)

		_, out := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{name}, fieldDecrypt: true})
		require.Equal(t, []any{name}, list(out, kInvalidParameters), name)
	}
}

// refRead reads reference name through GetParameter and GetParameters and
// requires both to answer want as its value, returning the GetParameter answer.
func (j *joined) refRead(name, want string) map[string]any {
	j.t.Helper()

	status, out := j.parameter(kGetParameter, regionEast, obj{kName: name})
	require.Equal(j.t, http.StatusOK, status, "%s: %v", name, out)
	require.Equal(j.t, want, param(out)[kValue], name)

	status, many := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{name}})
	require.Equal(j.t, http.StatusOK, status, "%s: %v", name, many)
	require.Empty(j.t, list(many, kInvalidParameters), name)
	require.Equal(j.t, want, entry(list(many, kParameters), 0)[kValue], name)

	return param(out)
}

// refMissing requires GetParameter to answer ParameterNotFound and GetParameters
// to list name in InvalidParameters.
func (j *joined) refMissing(name string) {
	j.t.Helper()

	j.parameterFails(kGetParameter, regionEast, obj{kName: name}, kParameterNotFound)

	status, out := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{name}})
	require.Equal(j.t, http.StatusOK, status, "%s: %v", name, out)
	require.Equal(j.t, []any{name}, list(out, kInvalidParameters), name)
}

// moveStage puts label on the version versionID of secret s1.
func (j *joined) moveStage(label, versionID string) {
	j.t.Helper()

	j.secret("UpdateSecretVersionStage", obj{fieldSecretID: "s1", fieldVersionStage: label, "MoveToVersionId": versionID})
}

// A selector of 32 to 64 characters is tried as a VersionId and, when no version
// has that id, as a staging label: a label of that length reads its version.
func Test_secretReference_longLabelsReadTheirVersion(t *testing.T) {
	j := newJoined(t, bothServices()...)
	first, _, _ := stagedSecret(j)

	for _, label := range []string{
		"3971cef0a1b2c3d4e5f60718293a4b5c6d7e8f90",
		"0f8fad5b-d9cb-469f-a165-70867728950e",
		"release-candidate-2026-11-20-build-0042",
		strings.Repeat("a", idMin),
		strings.Repeat("b", idMax),
		"v1.2.3.0000000000000000000000000000000",
		strings.Repeat("c", idMin-1),
		strings.Repeat("d", idMax+1),
	} {
		j.moveStage(label, first)

		got := j.refRead(referencePrefix+"s1:"+label, valueOne)
		require.Equal(t, referencePrefix+"s1", got[kName])
		require.Equal(t, ":"+label, got[keySelector])
	}
}

// A VersionId a caller chose with ClientRequestToken may hold underscores and
// periods, so it is not read by its characters.
func Test_secretReference_versionIDsOfAnyCharacters(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("ids", valueOne)

	for i, id := range []string{
		"build_2026_11_20_release_candidate_0042",
		"v1.2.3.0000000000000000000000000000000",
		strings.Repeat("A-9_.", 12) + "Z",
	} {
		value := fmt.Sprintf("value-%d", i)
		j.secret("PutSecretValue", obj{fieldSecretID: "ids", fieldSecretString: value, fieldToken: id})

		j.refRead(referencePrefix+"ids:"+id, value)
	}
}

// A label equal to the id of another version reads that version: the selector
// is tried as an id first.
func Test_secretReference_labelEqualToAnotherVersionsID(t *testing.T) {
	j := newJoined(t, bothServices()...)
	first, second, _ := stagedSecret(j)

	// Put the id of the first version on the second as a label.
	j.moveStage(first, second)

	status, out := j.do("secretsmanager", "secretsmanager.GetSecretValue", regionEast,
		obj{fieldSecretID: "s1", fieldVersionStage: first})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, valueTwo, out[fieldSecretString], "as a label it is the second version")

	j.refRead(referencePrefix+"s1:"+first, valueOne)
}

// A selector with a symbol outside letters, numbers, ".", "-" and "_" fails,
// though Secrets Manager holds a version with that label.
func Test_secretReference_symbolsFailTheReference(t *testing.T) {
	j := newJoined(t, bothServices()...)
	first, _, _ := stagedSecret(j)

	for _, selector := range []string{"rc+1", idWithSpace, "x/y", "a@b", "a=b", "a,b"} {
		status, _ := j.do("secretsmanager", "secretsmanager.UpdateSecretVersionStage", regionEast,
			obj{fieldSecretID: "s1", fieldVersionStage: selector, "MoveToVersionId": first})

		j.refMissing(referencePrefix + "s1:" + selector)

		require.Equal(t, http.StatusOK, status, "Secrets Manager takes the label %q", selector)
	}
}

// A reference with no selector has no Selector; its Name is the requested one.
func Test_secretReference_noSelectorNoSelectorField(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("plain", secretValue)

	got := j.refRead(referencePrefix+"plain", secretValue)
	require.Equal(t, referencePrefix+"plain", got[kName])
	require.NotContains(t, got, keySelector)
}

// A secret named arn, or one whose id starts with "arn:" and is not a whole ARN,
// keeps its selector.
func Test_secretReference_aSecretNamedARN(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("arn", valueOne)
	j.secret("PutSecretValue", obj{fieldSecretID: "arn", fieldSecretString: valueTwo, fieldToken: newToken()})

	got := j.refRead(referencePrefix+"arn:AWSCURRENT", valueTwo)
	require.Equal(t, referencePrefix+"arn", got[kName])
	require.Equal(t, ":AWSCURRENT", got[keySelector])

	j.refRead(referencePrefix+"arn:AWSPREVIOUS", valueOne)
	j.refRead(referencePrefix+"arn", valueTwo)
	j.refMissing(referencePrefix + "arn:nolabel")
	j.refMissing(referencePrefix + "arn:aws:secretsmanager:a:b")
}

// The ARN form of a name with a selector is named without it too.
func Test_secretReference_arnFormWithSelector(t *testing.T) {
	j := newJoined(t, bothServices()...)
	stagedSecret(j)

	byARN := "arn:aws:ssm:us-east-1:000000000000:parameter" + referencePrefix + "s1:AWSPREVIOUS"

	got := j.refRead(byARN, valueOne)
	require.Equal(t, referencePrefix+"s1", got[kName])
	require.Equal(t, ":AWSPREVIOUS", got[keySelector])
}

// The ARN of a secret takes a selector after its eighth field.
func Test_secretReference_selectorOnAnARN(t *testing.T) {
	j := newJoined(t, bothServices()...)
	stagedSecret(j)

	status, out := j.parameter(kGetParameter, regionEast, obj{kName: referencePrefix + "s1", fieldDecrypt: true})
	require.Equal(t, http.StatusOK, status, "%v", out)

	arn, _ := sourceResult(t, param(out))[keyARN].(string)
	require.NotEmpty(t, arn)

	name := referencePrefix + arn

	status, out = j.parameter(kGetParameter, regionEast, obj{kName: name, fieldDecrypt: true})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, valueTwo, param(out)[kValue])

	status, out = j.parameter(kGetParameter, regionEast, obj{kName: name + ":AWSPREVIOUS", fieldDecrypt: true})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, valueOne, param(out)[kValue])
}

// A reference whose id is an ARN reads the region the ARN names.
func Test_secretReference_anARNReadsItsOwnRegion(t *testing.T) {
	j := newJoined(t, bothServices()...)

	status, made := j.do("secretsmanager", "secretsmanager.CreateSecret", regionWest,
		obj{kName: "west", fieldSecretString: "w", fieldToken: newToken()})
	require.Equal(t, http.StatusOK, status, "%v", made)

	arn, _ := made[keyARN].(string)
	require.Contains(t, arn, regionWest)

	name := referencePrefix + arn

	j.parameterFails(kGetParameter, regionEast, obj{kName: referencePrefix + "west", fieldDecrypt: true},
		kParameterNotFound)

	status, out := j.parameter(kGetParameter, regionEast, obj{kName: name, fieldDecrypt: true})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "w", param(out)[kValue])
	require.Equal(t, arn, param(out)[keyARN])
}

// An id Secrets Manager rejects is a secret nothing holds, not a 500.
func Test_secretReference_invalidIDsAreNotFound(t *testing.T) {
	j := newJoined(t, bothServices()...)

	for _, id := range []string{idWithSpace, "a,b", "a!b", "a%b", "a*b", "s1:", ":AWSCURRENT", "a b:AWSCURRENT", "arn:x"} {
		name := referencePrefix + id

		j.parameterFails(kGetParameter, regionEast, obj{kName: name, fieldDecrypt: true}, kParameterNotFound)

		status, out := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{name}, fieldDecrypt: true})
		require.Equal(t, http.StatusOK, status, "%q: %v", id, out)
		require.Equal(t, []any{name}, list(out, kInvalidParameters), id)
	}
}

// GetParameters reads the ARN form of a reference name as GetParameter does.
func Test_secretReference_getParametersReadsTheARNForm(t *testing.T) {
	j := newJoined(t, bothServices()...)
	j.createSecret("mine", secretValue)

	byARN := "arn:aws:ssm:us-east-1:000000000000:parameter" + referencePrefix + "mine"
	plain := referencePrefix + "mine"

	status, single := j.parameter(kGetParameter, regionEast, obj{kName: byARN})
	require.Equal(t, http.StatusOK, status, "%v", single)

	status, out := j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{byARN}})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Empty(t, list(out, kInvalidParameters))
	require.Len(t, list(out, kParameters), 1)

	got := entry(list(out, kParameters), 0)
	require.Equal(t, secretValue, got[kValue])
	require.Equal(t, plain, got[kName], "GetParameter names it the same")
	require.Equal(t, param(single)[kName], got[kName])
	require.Equal(t, param(single)[keyARN], got[keyARN])

	// With ordinary parameters and the plain form in the same call.
	status, out = j.parameter(kPutParameter, regionEast, obj{kName: nameRooted, kValue: kLit3, kType: kString})
	require.Equal(t, http.StatusOK, status, "%v", out)

	_, out = j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{plain, byARN, nameRooted}})
	require.Len(t, list(out, kParameters), 3)
	require.Empty(t, list(out, kInvalidParameters))

	// An ARN of a secret nothing holds is invalid, as the plain form is.
	missing := "arn:aws:ssm:us-east-1:000000000000:parameter" + referencePrefix + "nothing"
	_, out = j.parameter(kGetParameters, regionEast, obj{fieldNames: []string{missing}})
	require.Equal(t, []any{missing}, list(out, kInvalidParameters))
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
