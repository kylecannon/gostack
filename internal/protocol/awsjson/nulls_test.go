package awsjson_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A handler tells VersionStages:[null] from VersionStages:[""]: the null is
// not a stage the client sent, and the empty string is.
func Test_Decode_NullListElementOfAGeneratedInput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		body string
		want []string
	}{
		{`{"VersionStages":[null]}`, []string{}},
		{`{"VersionStages":[null,"AWSCURRENT"]}`, []string{"AWSCURRENT"}},
		{`{"VersionStages":[""]}`, []string{""}},
		{`{"VersionStages":["","x",null]}`, []string{"", "x"}},
		{`{"VersionStages":null}`, nil},
	} {
		t.Run(tc.body, func(t *testing.T) {
			t.Parallel()

			got, failure := awsjson.Decode[secretsmanager.PutSecretValueInput]([]byte(tc.body))
			require.Nil(t, failure)
			assert.Equal(t, tc.want, got.VersionStages)
		})
	}
}

// json 1.0 and json 1.1 services read through the same decoder, in a list of
// strings and in a map of strings.
func Test_Decode_NullElementsAcrossJSONProtocols(t *testing.T) {
	t.Parallel()

	sq, failure := awsjson.Decode[sqs.ReceiveMessageInput]([]byte(`{"AttributeNames":[null,"All"]}`))
	require.Nil(t, failure)
	assert.Equal(t, []string{"All"}, sq.AttributeNames)

	enc, failure := awsjson.Decode[kms.EncryptInput]([]byte(`{"EncryptionContext":{"drop":null,"keep":"v","empty":""}}`))
	require.Nil(t, failure)
	assert.Equal(t, map[string]string{"keep": "v", "empty": ""}, enc.EncryptionContext)
}
