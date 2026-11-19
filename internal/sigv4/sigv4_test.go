package sigv4_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/sigv4"
	"github.com/stretchr/testify/require"
)

// Test inputs and expectations shared by several cases.
const (
	host            = "Host"
	unreservedBytes = "AZaz09-_.~"
	reservedEncoded = "%2B%3D%26%C3%A9"
)

func Test_Escape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		give     string
		want     string
		wantPath string
	}{
		{name: "unreserved bytes stay", give: unreservedBytes, want: unreservedBytes, wantPath: unreservedBytes},
		{name: "slash stays only in a path", give: "/a/b c", want: "%2Fa%2Fb%20c", wantPath: "/a/b%20c"},
		{name: "reserved bytes use upper-case hex", give: "+=&é", want: reservedEncoded, wantPath: reservedEncoded},
		{name: "percent is encoded again", give: "%2F", want: "%252F", wantPath: "%252F"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, sigv4.Escape(tt.give))
			require.Equal(t, tt.wantPath, sigv4.EscapePath(tt.give))
		})
	}
}

func Test_CanonicalQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		give []sigv4.Pair
		want string
	}{
		{name: "empty", give: nil, want: ""},
		{
			name: "sorted by encoded key then value",
			give: []sigv4.Pair{{Key: "b", Value: "2"}, {Key: "a", Value: "z"}, {Key: "a", Value: "y"}, {Key: "B", Value: ""}},
			want: "B=&a=y&a=z&b=2",
		},
		{
			name: "keys and values are encoded",
			give: []sigv4.Pair{{Key: "x y", Value: "a/b+c"}},
			want: "x%20y=a%2Fb%2Bc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, sigv4.CanonicalQuery(tt.give))
		})
	}
}

func Test_CanonicalHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		give       []sigv4.Header
		wantBlock  string
		wantSigned string
	}{
		{name: "empty", give: nil, wantBlock: "", wantSigned: ""},
		{
			name: "lower-cased, sorted, trimmed and joined",
			give: []sigv4.Header{
				{Name: "X-Amz-Meta-A", Value: "  one   two "},
				{Name: host, Value: "example.com"},
				{Name: "x-amz-meta-a", Value: "three"},
			},
			wantBlock:  "host:example.com\nx-amz-meta-a:one two,three\n",
			wantSigned: "host;x-amz-meta-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			block, signed := sigv4.CanonicalHeaders(tt.give)
			require.Equal(t, tt.wantBlock, block)
			require.Equal(t, tt.wantSigned, signed)
		})
	}
}

// Test_Signature_localstackVectors pins presigned GETs that LocalStack 8b9a79f
// computed for the secret "test" (SQ-109 probe), path style and virtual host.
func Test_Signature_localstackVectors(t *testing.T) {
	t.Parallel()

	query := []sigv4.Pair{
		{Key: "X-Amz-SignedHeaders", Value: "host"},
		{Key: "X-Amz-Expires", Value: "60"},
		{Key: "X-Amz-Date", Value: "20261118T121823Z"},
		{Key: "X-Amz-Credential", Value: "test/20261118/us-east-1/s3/aws4_request"},
		{Key: "X-Amz-Algorithm", Value: sigv4.Algorithm},
	}
	const wantQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=test%2F20261118%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20261118T121823Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host"

	tests := []struct {
		name          string
		givePath      string
		giveHost      string
		wantStringSHA string
		wantSignature string
	}{
		{
			name:          "path style",
			givePath:      "/probe-bucket/k",
			giveHost:      "s3.localhost.localstack.cloud:4566",
			wantStringSHA: "ab5b0ce12fb03277066954f84708591f098cf626f172e3a0d9e1344ecedc40d0",
			wantSignature: "c89d4b75d3983c367889fb47776e11ec088986e3fdd49ac609a67c3641389c8f",
		},
		{
			name:          "virtual host",
			givePath:      "/k",
			giveHost:      "probe-bucket.s3.localhost.localstack.cloud:4566",
			wantStringSHA: "cf477baaf6c05e1fd90fc67cf11fcccb47b5e2ce1734ade682fe00a24e75b194",
			wantSignature: "7914f2493c42ff45da17ee8c28ec59ebcf245c2b535169a1b541e0981c4a500e",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			canonicalQuery := sigv4.CanonicalQuery(query)
			require.Equal(t, wantQuery, canonicalQuery)

			headers, signed := sigv4.CanonicalHeaders([]sigv4.Header{{Name: host, Value: tt.giveHost}})
			request := sigv4.CanonicalRequest("get", sigv4.EscapePath(tt.givePath), canonicalQuery, headers, signed,
				sigv4.UnsignedPayload)
			require.Equal(t, "GET\n"+tt.givePath+"\n"+wantQuery+"\nhost:"+tt.giveHost+"\n\nhost\nUNSIGNED-PAYLOAD", request)

			scope := sigv4.Scope("20261118", "us-east-1", "s3")
			toSign := sigv4.StringToSign("20261118T121823Z", scope, request)
			require.Equal(t, "AWS4-HMAC-SHA256\n20261118T121823Z\n20261118/us-east-1/s3/aws4_request\n"+tt.wantStringSHA, toSign)

			got := sigv4.Signature(sigv4.SigningKey("test", "20261118", "us-east-1", "s3"), toSign)
			require.Equal(t, tt.wantSignature, got)
			require.True(t, sigv4.Equal(tt.wantSignature, got))
			require.False(t, sigv4.Equal(tt.wantSignature, "AAAA"+got))
		})
	}
}
