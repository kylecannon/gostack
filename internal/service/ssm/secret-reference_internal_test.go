package ssm

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/require"
)

// secretFailure answers a client error as a secret that is not there, and
// anything else as a failure of the call.
func Test_secretFailure(t *testing.T) {
	for status, body := range map[int]string{
		http.StatusBadRequest:            `{"__type":"ValidationException"}`,
		http.StatusForbidden:             `{"__type":"AccessDeniedException"}`,
		http.StatusNotFound:              `{"__type":"com.amazonaws.secretsmanager#ResourceNotFoundException"}`,
		http.StatusRequestEntityTooLarge: `not json`,
	} {
		require.NoError(t, secretFailure(inproc.Response{Status: status, Body: []byte(body)}), "status %d", status)
	}

	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotImplemented} {
		err := secretFailure(inproc.Response{Status: status, Body: []byte(`{"__type":"InternalFailure"}`)})
		require.Error(t, err, "status %d", status)
	}
}

// splitSelector reads the selector of an id by the shape of the id.
func Test_splitSelector(t *testing.T) {
	const arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:mine-RuXUMx"

	for id, want := range map[string][2]string{
		"s1":                 {"s1", ""},
		"s1:AWSCURRENT":      {"s1", tCurrent},
		"team/app:v:x":       {"team/app", "v:x"},
		"s1:":                {"s1:", ""},
		":AWSCURRENT":        {":AWSCURRENT", ""},
		arn:                  {arn, ""},
		arn + ":AWSPREVIOUS": {arn, "AWSPREVIOUS"},
		arn + ":":            {arn + ":", ""},
		// An id that starts with "arn:" and is not a whole ARN, seven fields, is a
		// name: a secret named arn keeps its selector.
		"arn:AWSCURRENT":               {tARN, tCurrent},
		"arn:x":                        {tARN, "x"},
		"arn:aws:secretsmanager:a:b":   {tARN, "aws:secretsmanager:a:b"},
		"arn:a:b:c:d:secret":           {tARN, "a:b:c:d:secret"},
		tShortARN:                      {tShortARN, ""},
		"arn:a:b:c:d:secret:n:AWSPREV": {tShortARN, "AWSPREV"},
	} {
		secretID, selector := splitSelector(id)
		require.Equal(t, want, [2]string{secretID, selector}, id)
	}
}

// A selector is read when it has letters, digits, periods, hyphens and
// underscores only.
func Test_validSelector(t *testing.T) {
	for _, selector := range []string{tCurrent, "v1.2.3", "a-b_c", strings.Repeat("a", versionIDMax)} {
		require.True(t, validSelector(selector), selector)
	}

	for _, selector := range []string{"rc+1", "a b", "x/y", "a@b", "a=b", "a,b", "a:b", "a!b", "é"} {
		require.False(t, validSelector(selector), selector)
	}
}

// A selector of 32 to 64 characters may be a VersionId, whatever its characters.
func Test_couldBeVersionID(t *testing.T) {
	require.True(t, couldBeVersionID(strings.Repeat("a", versionIDMin)))
	require.True(t, couldBeVersionID(strings.Repeat("A-9", 21)))
	require.True(t, couldBeVersionID(strings.Repeat("0", versionIDMax)))
	require.True(t, couldBeVersionID("v1.2.3.0000000000000000000000000000000"))
	require.False(t, couldBeVersionID(strings.Repeat("a", versionIDMin-1)))
	require.False(t, couldBeVersionID(strings.Repeat("a", versionIDMax+1)))
	require.False(t, couldBeVersionID(tCurrent))
}

// A selector of VersionId length is sent as an id and then as a label; any other
// only as a label.
func Test_secretRequests(t *testing.T) {
	long := strings.Repeat("a", versionIDMin)

	require.Equal(t, []map[string]string{{keySecretID: "s"}}, secretRequests("s", ""))
	require.Equal(t, []map[string]string{{keySecretID: "s", keyVersionStage: tCurrent}},
		secretRequests("s", tCurrent))
	require.Equal(t, []map[string]string{
		{keySecretID: "s", keyVersionID: long}, {keySecretID: "s", keyVersionStage: long},
	}, secretRequests("s", long))
}

const (
	tCurrent  = "AWSCURRENT"
	tARN      = "arn"
	tShortARN = "arn:a:b:c:d:secret:n"
)

// stubSecrets is a Secrets Manager that answers each GetSecretValue with the
// next of its answers and records the bodies it was sent.
type stubSecrets struct {
	answers []stubAnswer
	bodies  []map[string]string
}

type stubAnswer struct {
	status int
	body   string
}

func (s *stubSecrets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]string

	_ = json.NewDecoder(r.Body).Decode(&body)

	s.bodies = append(s.bodies, body)

	answer := stubAnswer{status: http.StatusBadRequest, body: `{"__type":"ResourceNotFoundException"}`}
	if len(s.bodies) <= len(s.answers) {
		answer = s.answers[len(s.bodies)-1]
	}

	w.WriteHeader(answer.status)
	_, _ = w.Write([]byte(answer.body))
}

func stubbed(answers ...stubAnswer) (*Provider, *stubSecrets) {
	secrets := &stubSecrets{answers: answers}
	p := New()
	p.client.Bind(secrets)

	return p, secrets
}

// readStubbed reads secret s1 with selector through a stub Secrets Manager.
func readStubbed(t *testing.T, selector string, answers ...stubAnswer) (found bool, sent []map[string]string) {
	t.Helper()

	p, secrets := stubbed(answers...)

	_, found, err := p.secretParameter(t.Context(), &call{account: "000000000000", region: "us-east-1"}, "s1", selector)
	require.NoError(t, err)

	return found, secrets.bodies
}

// A selector with a symbol the AWS user guide refuses is never sent to Secrets
// Manager, though Secrets Manager would answer it.
func Test_secretParameter_refusedSymbolsMakeNoCall(t *testing.T) {
	for _, selector := range []string{"rc+1", "a b", "x/y", "a@b", "a=b", "a,b", strings.Repeat("a", 31) + "+"} {
		found, sent := readStubbed(t, selector, stubAnswer{http.StatusOK, `{"SecretString":"x"}`})
		require.False(t, found, selector)
		require.Empty(t, sent, selector)
	}
}

// A selector of VersionId length is an id first and a label when Secrets
// Manager holds no such version, and only then.
func Test_secretParameter_idThenLabel(t *testing.T) {
	selector := strings.Repeat("a", versionIDMin)
	missing := stubAnswer{http.StatusBadRequest, `{"__type":"ResourceNotFoundException"}`}
	hit := stubAnswer{http.StatusOK, `{"SecretString":"x"}`}

	found, sent := readStubbed(t, selector, hit)
	require.True(t, found)
	require.Equal(t, []map[string]string{{keySecretID: "s1", keyVersionID: selector}}, sent, "an id is not tried again")

	found, sent = readStubbed(t, selector, missing, hit)
	require.True(t, found)
	require.Equal(t, []map[string]string{
		{keySecretID: "s1", keyVersionID: selector}, {keySecretID: "s1", keyVersionStage: selector},
	}, sent)

	found, sent = readStubbed(t, selector, missing, missing)
	require.False(t, found)
	require.Len(t, sent, 2)

	// Any other client error of the id is the answer: the label is not tried.
	found, sent = readStubbed(t, selector, stubAnswer{http.StatusBadRequest, `{"__type":"InvalidRequestException"}`}, hit)
	require.False(t, found)
	require.Len(t, sent, 1)

	// A label of another length is a label only.
	found, sent = readStubbed(t, strings.Repeat("a", versionIDMin-1), hit)
	require.True(t, found)
	require.Equal(t, []map[string]string{{keySecretID: "s1", keyVersionStage: strings.Repeat("a", versionIDMin-1)}}, sent)

	// A failure of the id's call is a failure, not a reason to try a label.
	p, secrets := stubbed(stubAnswer{http.StatusInternalServerError, `{"__type":"InternalFailure"}`}, hit)
	_, _, err := p.secretParameter(t.Context(), &call{}, "s1", selector)
	require.Error(t, err)
	require.Len(t, secrets.bodies, 1)
}

// FuzzParameterOfSecret feeds the parser of a GetSecretValue answer whatever the
// bytes are: it must not panic, and an answer it accepts yields a SourceResult
// that is JSON, with no key but ARN starting with a capital.
func FuzzParameterOfSecret(f *testing.F) {
	f.Add([]byte(`{"ARN":"arn:aws:secretsmanager:us-east-1:000000000000:secret:s-AbCdEf","Name":"s",` +
		`"VersionId":"v","SecretString":"x","VersionStages":[tCurrent],"CreatedDate":1.7e9}`))
	f.Add([]byte(`{"SecretBinary":"AAEC","CreatedDate":1700000000.5}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"SecretString":null,"CreatedDate":"soon"}`))
	f.Add([]byte(`{"Élan":1,"":2,"ARN":3}`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, body []byte) {
		param, err := parameterOfSecret(body)
		if err != nil {
			return
		}

		require.NotNil(t, param.SourceResult)

		var source map[string]json.RawMessage

		require.NoError(t, json.Unmarshal([]byte(*param.SourceResult), &source))

		for key := range source {
			first, _ := utf8.DecodeRuneInString(key)
			if key != arnKey {
				require.False(t, unicode.IsUpper(first), "key %q starts with a capital", key)
			}
		}
	})
}
