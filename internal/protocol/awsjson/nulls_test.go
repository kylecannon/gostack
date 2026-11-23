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

	sm, failure := awsjson.Decode[secretsmanager.PutSecretValueInput]([]byte(`{"VersionStages":[null,"AWSCURRENT"]}`))
	require.Nil(t, failure)
	assert.Equal(t, []string{"AWSCURRENT"}, sm.VersionStages)

	attrs, failure := awsjson.Decode[sqs.SetQueueAttributesInput](
		[]byte(`{"Attributes":{"drop":null,"keep":"v","empty":""}}`))
	require.Nil(t, failure)
	assert.Equal(t, map[string]string{"keep": "v", "empty": ""}, attrs.Attributes)
}

// A service whose real implementation tells a null apart decodes with
// awsjson.KeepNulls, as the generated dispatch of DynamoDB, DynamoDB Streams
// and KMS does, and keeps the zero value v2 reads.
func Test_Decode_KeepNullsOfAGeneratedInput(t *testing.T) {
	t.Parallel()

	enc, failure := awsjson.Decode[kms.EncryptInput](
		[]byte(`{"EncryptionContext":{"k":null,"keep":"v"}}`), awsjson.KeepNulls)
	require.Nil(t, failure)
	assert.Equal(t, map[string]string{"k": "", "keep": "v"}, enc.EncryptionContext)

	grant, failure := awsjson.Decode[kms.CreateGrantInput]([]byte(`{"Operations":[null]}`), awsjson.KeepNulls)
	require.Nil(t, failure)
	assert.Equal(t, []string{""}, grant.Operations)
}
