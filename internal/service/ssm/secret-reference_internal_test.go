package ssm

import (
	"encoding/json"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

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
