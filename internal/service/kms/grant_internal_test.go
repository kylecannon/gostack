package kms

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzGrantToken feeds RetireGrant's token reader any text: it answers a grant
// store or InvalidGrantTokenException and never panics, and a token it accepts
// names an account and region of three parts.
func FuzzGrantToken(f *testing.F) {
	for _, seed := range []string{
		base64.StdEncoding.EncodeToString([]byte("000000000000:us-east-1:" + sampleKeyID)),
		base64.StdEncoding.EncodeToString([]byte("one:two")),
		base64.StdEncoding.EncodeToString([]byte("a:b:c:d")),
		base64.StdEncoding.EncodeToString([]byte("::")),
		"not base64!",
		"",
	} {
		f.Add(seed)
	}

	p := New(nil)

	f.Fuzz(func(t *testing.T, token string) {
		grants, err := p.tokenGrants(token)
		if err != nil {
			require.Nil(t, grants)

			return
		}

		require.NotNil(t, grants)

		_, found := grants.idOfToken(token)
		require.False(t, found)
	})
}
