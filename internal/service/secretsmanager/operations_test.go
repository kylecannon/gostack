package secretsmanager_test

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/require"
)

const (
	token2 = "22222222-2222-2222-2222-222222222222"
	token3 = "33333333-3333-3333-3333-333333333333"

	describeSecret = "DescribeSecret"
)

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()

	m, ok := v.(map[string]any)
	require.True(t, ok, "%v is not an object", v)

	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()

	l, ok := v.([]any)
	require.True(t, ok, "%v is not a list", v)

	return l
}

func handlerFor(t testing.TB, p *secretsmanager.Provider) http.Handler {
	t.Helper()

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAddr}} },
		[]service.Registration{p.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	return gw.Handler()
}

// mustCall is call that requires a 200.
func mustCall(t *testing.T, h http.Handler, operation, body string) map[string]any {
	t.Helper()

	status, out := call(t, h, operation, body)
	require.Equal(t, http.StatusOK, status, "%s %s: %v", operation, body, out)

	return out
}

func requireError(t *testing.T, h http.Handler, operation, body, code string) map[string]any {
	t.Helper()

	status, out := call(t, h, operation, body)
	require.Equal(t, http.StatusBadRequest, status, "%s %s: %v", operation, body, out)
	require.Equal(t, code, out["__type"], "%s %s: %v", operation, body, out)

	return out
}

func Test_batchGetSecretValue(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"b1","SecretString":"one","ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "CreateSecret", `{"Name":"b2","SecretString":"two","ClientRequestToken":"`+token2+`"}`)
	mustCall(t, h, "DeleteSecret", `{"SecretId":"b2"}`)

	byID := mustCall(t, h, "BatchGetSecretValue", `{"SecretIdList":["b1","b2","nope"]}`)
	values, ok := byID["SecretValues"].([]any)
	require.True(t, ok, byID)
	require.Len(t, values, 1)
	require.Equal(t, "one", asMap(t, values[0])["SecretString"])

	errs, ok := byID["Errors"].([]any)
	require.True(t, ok, byID)
	require.Len(t, errs, 2)
	require.Equal(t, "InvalidRequestException", asMap(t, errs[0])["ErrorCode"])
	require.Contains(t, asMap(t, errs[0])["Message"], "currently marked deleted")
	require.Equal(t, "ResourceNotFoundException", asMap(t, errs[1])["ErrorCode"])

	// moto reads every secret a filter matches through get_secret_value, which
	// fails on a deleted one, so the whole call fails with a 400.
	failed := requireError(t, h, "BatchGetSecretValue", `{"Filters":[{"Key":"name","Values":["b"]}]}`,
		"InvalidRequestException")
	require.Contains(t, failed["Message"], "currently marked deleted", failed)

	live := mustCall(t, h, "BatchGetSecretValue", `{"Filters":[{"Key":"name","Values":["b1"]}]}`)
	require.Len(t, live["SecretValues"], 1, live)

	requireError(t, h, "BatchGetSecretValue",
		`{"SecretIdList":["b1"],"Filters":[{"Key":"name","Values":["b"]}]}`, "InvalidParameterException")
	requireError(t, h, "BatchGetSecretValue", `{"SecretIdList":["b1"],"MaxResults":1}`, "InvalidParameterException")
}

// Test_empty_requests_never_answer_500 sends {} to every operation: a required
// member reaches the handler as nil, and a nil dereference would be a 500.
func Test_empty_requests_never_answer_500(t *testing.T) {
	h := newHandler(t)

	for _, operation := range []string{
		"CreateSecret", "GetSecretValue", "PutSecretValue", "UpdateSecret", describeSecret, "ListSecrets",
		"ListSecretVersionIds", "UpdateSecretVersionStage", "DeleteSecret", "RestoreSecret", "TagResource",
		"UntagResource", "GetRandomPassword", "BatchGetSecretValue", "ValidateResourcePolicy",
	} {
		status, out := call(t, h, operation, `{}`)
		require.NotEqual(t, http.StatusInternalServerError, status, "%s: %v", operation, out)
	}
}

func Test_stage_moves(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"st","SecretString":"one","ClientRequestToken":"`+token1+`"}`)

	// A put without AWSCURRENT leaves the current version alone.
	mustCall(t, h, "PutSecretValue",
		`{"SecretId":"st","SecretString":"two","ClientRequestToken":"`+token2+`","VersionStages":["custom"]}`)
	require.Equal(t, "one", mustCall(t, h, "GetSecretValue", `{"SecretId":"st"}`)["SecretString"])
	require.Equal(t, "two",
		mustCall(t, h, "GetSecretValue", `{"SecretId":"st","VersionStage":"custom"}`)["SecretString"])

	mustCall(t, h, "UpdateSecretVersionStage", `{"SecretId":"st","VersionStage":"AWSCURRENT",`+
		`"RemoveFromVersionId":"`+token1+`","MoveToVersionId":"`+token2+`"}`)
	require.Equal(t, "two", mustCall(t, h, "GetSecretValue", `{"SecretId":"st"}`)["SecretString"])
	require.Equal(t, "one",
		mustCall(t, h, "GetSecretValue", `{"SecretId":"st","VersionStage":"AWSPREVIOUS"}`)["SecretString"])

	requireError(t, h, "UpdateSecretVersionStage", `{"SecretId":"st","VersionStage":"AWSCURRENT",`+
		`"MoveToVersionId":"`+token1+`"}`, "InvalidParameterException")

	// Adding a stage a version already has adds no duplicate.
	for range 2 {
		mustCall(t, h, "UpdateSecretVersionStage",
			`{"SecretId":"st","VersionStage":"extra","MoveToVersionId":"`+token2+`"}`)
	}

	described := mustCall(t, h, describeSecret, `{"SecretId":"st"}`)
	stages := asMap(t, described["VersionIdsToStages"])
	require.ElementsMatch(t, []any{"custom", awsCurrent, "extra"}, stages[token2])
	require.Equal(t, []any{"AWSPREVIOUS"}, stages[token1])

	// AWSCURRENT taken off with no target: the secret is not stuck, the next
	// put becomes current.
	mustCall(t, h, "UpdateSecretVersionStage", `{"SecretId":"st","VersionStage":"AWSCURRENT",`+
		`"RemoveFromVersionId":"`+token2+`"}`)
	mustCall(t, h, "PutSecretValue", `{"SecretId":"st","SecretString":"three","ClientRequestToken":"`+token3+`"}`)
	require.Equal(t, "three", mustCall(t, h, "GetSecretValue", `{"SecretId":"st"}`)["SecretString"])
}

func Test_list_filters_and_pages(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"app/db","Description":"Some secret","SecretString":"x",`+
		`"ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "CreateSecret", `{"Name":"plain","SecretString":"y","ClientRequestToken":"`+token2+`",`+
		`"Tags":[{"Key":"team","Value":"red"}]}`)

	count := func(filters string) int {
		out := mustCall(t, h, "ListSecrets", `{"Filters":`+filters+`}`)

		return len(asList(t, out["SecretList"]))
	}

	require.Equal(t, 1, count(`[{"Key":"name","Values":["app/"]}]`))
	require.Equal(t, 1, count(`[{"Key":"name","Values":["!app/"]}]`))
	require.Equal(t, 1, count(`[{"Key":"description","Values":["some"]}]`))
	require.Equal(t, 0, count(`[{"Key":"description","Values":["!some"]}]`))
	require.Equal(t, 1, count(`[{"Key":"tag-key","Values":["te"]}]`))
	require.Equal(t, 0, count(`[{"Key":"tag-key","Values":["te"]},{"Key":"name","Values":["app"]}]`))

	requireError(t, h, "ListSecrets", `{"Filters":[{"Key":"bogus","Values":["a"]}]}`, "ValidationException")
	requireError(t, h, "ListSecrets", `{"Filters":[{"Key":"name"}]}`, "InvalidParameterException")

	first := mustCall(t, h, "ListSecrets", `{"MaxResults":1}`)
	require.Len(t, first["SecretList"], 1)
	require.Equal(t, "1", first["NextToken"])

	second := mustCall(t, h, "ListSecrets", `{"MaxResults":1,"NextToken":"1"}`)
	require.Len(t, second["SecretList"], 1)
	require.NotContains(t, second, "NextToken")

	past := mustCall(t, h, "ListSecrets", `{"NextToken":"`+strconv.Itoa(1<<62)+`"}`)
	require.Empty(t, past["SecretList"])

	requireError(t, h, "ListSecrets", `{"NextToken":"x"}`, "InvalidNextTokenException")
}

func Test_tags_absent_until_first_tag(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"tg","SecretString":"x","ClientRequestToken":"`+token1+`"}`)
	require.NotContains(t, mustCall(t, h, describeSecret, `{"SecretId":"tg"}`), "Tags")

	mustCall(t, h, "TagResource", `{"SecretId":"tg","Tags":[{"Key":"k","Value":"v"}]}`)
	require.Len(t, mustCall(t, h, describeSecret, `{"SecretId":"tg"}`)["Tags"], 1)

	mustCall(t, h, "UntagResource", `{"SecretId":"tg","TagKeys":["k"]}`)

	described := mustCall(t, h, describeSecret, `{"SecretId":"tg"}`)
	require.Contains(t, described, "Tags")
	require.Empty(t, described["Tags"])
}

func Test_snapshot_round_trip(t *testing.T) {
	p := secretsmanager.New(nil)
	h := handlerFor(t, p)

	mustCall(t, h, "CreateSecret", `{"Name":"keep","Description":"d","SecretString":"one",`+
		`"ClientRequestToken":"`+token1+`","Tags":[{"Key":"k","Value":"v"}]}`)
	mustCall(t, h, "PutSecretValue", `{"SecretId":"keep","SecretBinary":"AAEC","ClientRequestToken":"`+token2+`"}`)
	mustCall(t, h, "GetSecretValue", `{"SecretId":"keep"}`)
	mustCall(t, h, "CreateSecret", `{"Name":"doomed","SecretString":"x","ClientRequestToken":"`+token3+`"}`)
	mustCall(t, h, "DeleteSecret", `{"SecretId":"doomed","RecoveryWindowInDays":7}`)

	var saved bytes.Buffer
	require.NoError(t, p.Snapshot(&saved))

	q := secretsmanager.New(nil)
	require.NoError(t, q.Restore(bytes.NewReader(saved.Bytes())))

	var again bytes.Buffer
	require.NoError(t, q.Snapshot(&again))
	require.Equal(t, saved.String(), again.String())

	g := handlerFor(t, q)

	for _, read := range []struct{ operation, body string }{
		{describeSecret, `{"SecretId":"keep"}`},
		{describeSecret, `{"SecretId":"doomed"}`},
		{"ListSecretVersionIds", `{"SecretId":"keep","IncludeDeprecated":true}`},
		{"ListSecrets", `{"IncludePlannedDeletion":true}`},
		{"GetSecretValue", `{"SecretId":"keep","VersionStage":"AWSPREVIOUS"}`},
	} {
		require.Equal(t, mustCall(t, h, read.operation, read.body), mustCall(t, g, read.operation, read.body),
			"%s %s", read.operation, read.body)
	}

	requireError(t, g, "GetSecretValue", `{"SecretId":"doomed"}`, "InvalidRequestException")
	require.Error(t, q.Restore(bytes.NewReader([]byte(`{"v":99,"data":{"states":[]}}`))))
}
