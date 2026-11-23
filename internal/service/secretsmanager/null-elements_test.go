package secretsmanager_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A json null in a list is no entry (awsjson.nulls). The LocalStack image drops
// it for PutSecretValue's VersionStages and for UntagResource's TagKeys, and
// answers 500 (a TypeError) for BatchGetSecretValue's SecretIdList and
// ListSecrets' filter values; gostack answers those as for the list without the
// null (docs/compat.md, "Null elements").
func Test_a_null_in_a_list_is_no_entry(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"nulls","SecretString":"one","ClientRequestToken":"`+token1+`",`+
		`"Tags":[{"Key":"a","Value":"b"}]}`)

	t.Run("the image drops it", func(t *testing.T) {
		put := mustCall(t, h, "PutSecretValue", `{"SecretId":"nulls","SecretString":"two",`+
			`"ClientRequestToken":"22222222-2222-2222-2222-222222222222","VersionStages":["AWSCURRENT",null]}`)
		assert.Equal(t, []any{"AWSCURRENT"}, put["VersionStages"])

		mustCall(t, h, "UntagResource", `{"SecretId":"nulls","TagKeys":[null]}`)

		_, described := call(t, h, "DescribeSecret", `{"SecretId":"nulls"}`)
		assert.Len(t, described["Tags"], 1, "a null tag key removes no tag")

		mustCall(t, h, "UntagResource", `{"SecretId":"nulls","TagKeys":["a",null]}`)

		_, described = call(t, h, "DescribeSecret", `{"SecretId":"nulls"}`)
		assert.Empty(t, described["Tags"])
	})

	t.Run("the image answers 500", func(t *testing.T) {
		status, batch := call(t, h, "BatchGetSecretValue", `{"SecretIdList":["nulls",null]}`)
		require.Equal(t, http.StatusOK, status, batch)
		assert.Len(t, batch["SecretValues"], 1)
		assert.Empty(t, batch["Errors"])

		status, listed := call(t, h, "ListSecrets", `{"Filters":[{"Key":"name","Values":["nulls",null]}]}`)
		require.Equal(t, http.StatusOK, status, listed)
		assert.Len(t, listed["SecretList"], 1)
	})
}
