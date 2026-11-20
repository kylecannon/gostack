package secretsmanager_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tagOf is a tag as DescribeSecret answers it.
func tagOf(key, value string) map[string]any {
	return map[string]any{keyTagKey: key, keyTagValue: value}
}

// stagesOf is the VersionIdsToStages DescribeSecret answers for name.
func stagesOf(t *testing.T, h http.Handler, name string) map[string]any {
	t.Helper()

	return asMap(t, mustCall(t, h, describeSecret, `{"SecretId":"`+name+`"}`)["VersionIdsToStages"])
}

// A stage given twice is one stage. moto keeps both, so a version holds
// AWSCURRENT twice and a later move leaves two versions holding it.
func Test_a_stage_listed_twice_is_one_stage(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"dup","SecretString":"one","ClientRequestToken":"`+token1+`"}`)

	put := mustCall(t, h, "PutSecretValue", `{"SecretId":"dup","SecretString":"two","ClientRequestToken":"`+token2+`",`+
		`"VersionStages":["AWSCURRENT","AWSCURRENT"]}`)
	assert.Equal(t, []any{awsCurrent}, put["VersionStages"])

	mustCall(t, h, "PutSecretValue", `{"SecretId":"dup","SecretString":"three","ClientRequestToken":"`+token3+`",`+
		`"VersionStages":["x","y","x"]}`)
	assert.Equal(t, []any{"x", "y"}, stagesOf(t, h, "dup")[token3])

	mustCall(t, h, "UpdateSecretVersionStage", `{"SecretId":"dup","VersionStage":"AWSCURRENT",`+
		`"RemoveFromVersionId":"`+token2+`","MoveToVersionId":"`+token1+`"}`)

	stages := stagesOf(t, h, "dup")
	assert.Equal(t, []any{awsCurrent}, stages[token1])
	assert.Equal(t, []any{"AWSPREVIOUS"}, stages[token2])

	mustCall(t, h, "PutSecretValue", `{"SecretId":"dup","SecretString":"four","ClientRequestToken":"`+token4+`"}`)

	current := 0

	for _, versionStages := range stagesOf(t, h, "dup") {
		for _, stage := range asList(t, versionStages) {
			if stage == awsCurrent {
				current++
			}
		}
	}

	assert.Equal(t, 1, current, "one version holds AWSCURRENT")
}

// With no version holding AWSCURRENT, moving it without naming the version it
// leaves is still refused, as a 400, and the message does not name an empty id.
func Test_moving_AWSCURRENT_with_no_holder_is_refused_without_an_empty_id(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"none","SecretString":"one","ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "UpdateSecretVersionStage", `{"SecretId":"none","VersionStage":"AWSCURRENT",`+
		`"RemoveFromVersionId":"`+token1+`"}`)

	refused := requireError(t, h, "UpdateSecretVersionStage",
		`{"SecretId":"none","VersionStage":"AWSCURRENT","MoveToVersionId":"`+token1+`"}`, "InvalidParameterException")
	assert.Equal(t, "The parameter RemoveFromVersionId can't be empty. "+
		"Staging label AWSCURRENT is not attached to any version of this secret.", refused["Message"])

	// With a holder the message names it.
	mustCall(t, h, "PutSecretValue", `{"SecretId":"none","SecretString":"two","ClientRequestToken":"`+token2+`"}`)

	named := requireError(t, h, "UpdateSecretVersionStage",
		`{"SecretId":"none","VersionStage":"AWSCURRENT","MoveToVersionId":"`+token1+`"}`, "InvalidParameterException")
	assert.Contains(t, named["Message"], "currently attached to version "+token2+",")
}

// An UpdateSecretVersionStage without a VersionStage changes nothing, as on
// LocalStack: no stage named "" appears on the version it moves to.
func Test_UpdateSecretVersionStage_without_a_stage_changes_nothing(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"bare","SecretString":"one","ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "PutSecretValue", `{"SecretId":"bare","SecretString":"two","ClientRequestToken":"`+token2+`"}`)

	before := stagesOf(t, h, "bare")

	mustCall(t, h, "UpdateSecretVersionStage", `{"SecretId":"bare","MoveToVersionId":"`+token1+`"}`)
	mustCall(t, h, "UpdateSecretVersionStage", `{"SecretId":"bare","VersionStage":null,"MoveToVersionId":"`+token2+`"}`)
	assert.Equal(t, before, stagesOf(t, h, "bare"))

	// The versions are still checked, and a stage None is never on the one to
	// remove it from.
	missing := requireError(t, h, "UpdateSecretVersionStage",
		`{"SecretId":"bare","RemoveFromVersionId":"`+token1+`"}`, "InvalidParameterException")
	assert.Equal(t, "Version stage None not found in version "+token1, missing["Message"])

	requireError(t, h, "UpdateSecretVersionStage", `{"SecretId":"bare","MoveToVersionId":"`+token3+`"}`,
		"InvalidParameterException")
}

// A null in VersionStages is no stage, as on LocalStack: it does not become a
// stage named "".
func Test_a_null_version_stage_is_no_stage(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"nul","SecretString":"one","ClientRequestToken":"`+token1+`"}`)

	put := mustCall(t, h, "PutSecretValue", `{"SecretId":"nul","SecretString":"two","ClientRequestToken":"`+token2+`",`+
		`"VersionStages":["AWSPENDING",null]}`)
	assert.Equal(t, []any{stagePending}, put["VersionStages"])
	assert.Equal(t, []any{stagePending}, stagesOf(t, h, "nul")[token2])
}

// The BYPASS_SECRET_ID_VALIDATION tag is stored with the others, as LocalStack
// stores it, and can be removed like any other.
func Test_the_bypass_tag_is_kept(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"tagged","SecretString":"one","ClientRequestToken":"`+token1+`",`+
		`"Tags":[{"Key":"BYPASS_SECRET_ID_VALIDATION","Value":"1"},{"Key":"k","Value":"v"}]}`)

	assert.Equal(t, []any{tagOf(bypass, "1"), tagOf("k", "v")},
		mustCall(t, h, describeSecret, `{"SecretId":"tagged"}`)["Tags"])

	listed := mustCall(t, h, "ListSecrets",
		`{"Filters":[{"Key":"tag-key","Values":["BYPASS_SECRET_ID_VALIDATION"]}]}`)
	assert.Len(t, listed["SecretList"], 1)

	mustCall(t, h, "UntagResource", `{"SecretId":"tagged","TagKeys":["BYPASS_SECRET_ID_VALIDATION"]}`)
	assert.Equal(t, []any{tagOf("k", "v")},
		mustCall(t, h, describeSecret, `{"SecretId":"tagged"}`)["Tags"])
}

// BatchGetSecretValue over a secret with no version fails the whole call as
// moto does: a 404 for VersionId AWSCURRENT, not the 400 "staging label: " the
// single read gives.
func Test_BatchGetSecretValue_of_a_secret_with_no_value(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"empty","ClientRequestToken":"`+token1+`"}`)

	for _, body := range []string{`{"SecretIdList":["empty"]}`, `{"Filters":[{"Key":"name","Values":["empty"]}]}`} {
		status, out := call(t, h, "BatchGetSecretValue", body)
		require.Equal(t, http.StatusNotFound, status, "%s: %v", body, out)
		assert.Equal(t, "ResourceNotFoundException", out["__type"])
		assert.Equal(t, "Secrets Manager can't find the specified secret value for VersionId: AWSCURRENT", out["Message"])
	}

	// The single read is unchanged.
	requireError(t, h, "GetSecretValue", `{"SecretId":"empty"}`, "ResourceNotFoundException")
}

// BatchGetSecretValue over a secret none of whose versions holds AWSCURRENT
// fails the call as the no-version case does, where it used to read the
// version the secret last made current.
func Test_BatchGetSecretValue_when_no_version_holds_AWSCURRENT(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"nocur","SecretString":"one","ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "UpdateSecretVersionStage",
		`{"SecretId":"nocur","VersionStage":"AWSCURRENT","RemoveFromVersionId":"`+token1+`"}`)

	status, out := call(t, h, "BatchGetSecretValue", `{"SecretIdList":["nocur"]}`)
	require.Equal(t, http.StatusNotFound, status, "%v", out)
	assert.Equal(t, "Secrets Manager can't find the specified secret value for VersionId: AWSCURRENT", out["Message"])
}
