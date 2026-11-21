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
		"s1":                         {"s1", ""},
		"s1:AWSCURRENT":              {"s1", "AWSCURRENT"},
		"team/app:v:x":               {"team/app", "v:x"},
		"s1:":                        {"s1:", ""},
		":AWSCURRENT":                {":AWSCURRENT", ""},
		arn:                          {arn, ""},
		arn + ":AWSPREVIOUS":         {arn, "AWSPREVIOUS"},
		arn + ":":                    {arn + ":", ""},
		"arn:x":                      {"arn:x", ""},
		"arn:aws:secretsmanager:a:b": {"arn:aws:secretsmanager:a:b", ""},
	} {
		secretID, selector := splitSelector(id)
		require.Equal(t, want, [2]string{secretID, selector}, id)
	}
}

// A selector is a VersionId when it is 32 to 64 letters, digits and hyphens.
func Test_isVersionID(t *testing.T) {
	require.True(t, isVersionID(strings.Repeat("a", versionIDMin)))
	require.True(t, isVersionID(strings.Repeat("A-9", 21)))
	require.True(t, isVersionID(strings.Repeat("0", versionIDMax)))
	require.False(t, isVersionID(strings.Repeat("a", versionIDMin-1)))
	require.False(t, isVersionID(strings.Repeat("a", versionIDMax+1)))
	require.False(t, isVersionID(strings.Repeat("a", versionIDMin-1)+"_"))
	require.False(t, isVersionID("AWSCURRENT"))
}

// FuzzParameterOfSecret feeds the parser of a GetSecretValue answer whatever the
// bytes are: it must not panic, and an answer it accepts yields a SourceResult
// that is JSON, with no key but ARN starting with a capital.
func FuzzParameterOfSecret(f *testing.F) {
	f.Add([]byte(`{"ARN":"arn:aws:secretsmanager:us-east-1:000000000000:secret:s-AbCdEf","Name":"s",` +
		`"VersionId":"v","SecretString":"x","VersionStages":["AWSCURRENT"],"CreatedDate":1.7e9}`))
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
