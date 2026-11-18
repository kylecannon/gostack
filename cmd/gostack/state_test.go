package main_test

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	accessKeyIDElement = regexp.MustCompile(`<AccessKeyId>([^<]+)</AccessKeyId>`)
	arnElement         = regexp.MustCompile(`<Arn>([^<]+)</Arn>`)
)

// stsCall sends an STS query call signed with accessKey and returns the body.
func (g *gostack) stsCall(t *testing.T, accessKey, body string) string {
	t.Helper()

	got := g.request(t, http.MethodPost, "/", "Version=2011-06-15&"+body, map[string]string{
		headerContentType: formContentType,
		"Authorization": "AWS4-HMAC-SHA256 Credential=" + accessKey +
			"/20130524/us-east-1/sts/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc",
	})

	require.NoError(t, got.err)
	require.Equal(t, http.StatusOK, got.status, got.body)

	return got.body
}

// Test_gostack_reset_clears_sts_sessions: STS keeps the session behind each
// credential it issues, which GetCallerIdentity reads, and persistence does not
// save it. /_gostack/state/reset clears it all the same, with or without
// ?service=.
func Test_gostack_reset_clears_sts_sessions(t *testing.T) {
	g := startGostack(t)

	for _, path := range []string{"/_gostack/state/reset", "/_gostack/state/reset?service=sts"} {
		t.Run(path, func(t *testing.T) {
			assumed := g.stsCall(t, "test",
				"Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/r&RoleSessionName=session")

			key := accessKeyIDElement.FindStringSubmatch(assumed)
			require.NotNil(t, key, assumed)

			identity := arnElement.FindStringSubmatch(g.stsCall(t, key[1], "Action=GetCallerIdentity"))
			require.NotNil(t, identity)
			assert.Equal(t, "arn:aws:sts::000000000000:assumed-role/r/session", identity[1], "the session is kept")

			reset := g.request(t, http.MethodPost, path, "", nil)
			require.NoError(t, reset.err)
			require.Equal(t, http.StatusOK, reset.status, reset.body)

			identity = arnElement.FindStringSubmatch(g.stsCall(t, key[1], "Action=GetCallerIdentity"))
			require.NotNil(t, identity)
			assert.Equal(t, "arn:aws:iam::000000000000:root", identity[1], "the session is gone")
		})
	}
}
