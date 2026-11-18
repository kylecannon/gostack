package main_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// s3Auth routes a request to S3 by its signing name; S3 does not check the
	// signature unless asked to.
	s3Auth        = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=x"
	authHeader    = "Authorization"
	taggingPrefix = `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<TagSet><Tag><Key>k</Key><Value>v</Value></Tag>`
	taggingSuffix = `</TagSet></Tagging>`
)

// taggingOfSize is a Tagging document of exactly size bytes.
func taggingOfSize(size int) string {
	return taggingPrefix + strings.Repeat(" ", size-len(taggingPrefix)-len(taggingSuffix)) + taggingSuffix
}

func (g *gostack) putTagging(t *testing.T, body string) raw {
	t.Helper()

	got := g.request(t, http.MethodPut, "/bucket-a?tagging", body, map[string]string{authHeader: s3Auth})
	require.NoError(t, got.err)

	return got
}

func (g *gostack) createBucket(t *testing.T) {
	t.Helper()

	got := g.request(t, http.MethodPut, "/bucket-a", "", map[string]string{authHeader: s3Auth})
	require.NoError(t, got.err)
	require.Equal(t, http.StatusOK, got.status, got.body)
}

// Test_gostack_maxRequestBody: GOSTACK_MAX_REQUEST_BODY reaches the body readers
// of the running binary. S3 answers a document of exactly the limit and refuses
// one byte more with MaxMessageLengthExceeded.
func Test_gostack_maxRequestBody(t *testing.T) {
	g := startGostack(t, "GOSTACK_MAX_REQUEST_BODY=2KiB")
	g.createBucket(t)

	require.Equal(t, http.StatusNoContent, g.putTagging(t, taggingOfSize(2048)).status)

	got := g.putTagging(t, taggingOfSize(2049))
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	assert.Contains(t, got.body, "<Code>MaxMessageLengthExceeded</Code>")
	assert.Contains(t, got.body, "<Message>Your request was too big.</Message>")
}

// Test_gostack_maxRequestBodyZero: 0 is no limit, as in LocalStack.
func Test_gostack_maxRequestBodyZero(t *testing.T) {
	g := startGostack(t, "GOSTACK_MAX_REQUEST_BODY=0")
	g.createBucket(t)

	const seventeenMiB = 17 << 20

	require.Equal(t, http.StatusNoContent, g.putTagging(t, taggingOfSize(seventeenMiB)).status)
}

// Test_gostack_maxRequestBodyInvalid: a value that does not parse is logged and
// the default stays.
func Test_gostack_maxRequestBodyInvalid(t *testing.T) {
	g := startGostack(t, "GOSTACK_MAX_REQUEST_BODY=banana")
	g.createBucket(t)

	require.Equal(t, http.StatusNoContent, g.putTagging(t, taggingOfSize(4096)).status)
	assert.Contains(t, g.stderr.String(), `GOSTACK_MAX_REQUEST_BODY: "banana" is not a number of bytes`)
}
