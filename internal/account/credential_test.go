package account_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/stretchr/testify/assert"
)

const euWest1 = "eu-west-1"

// presignedV4 is the query string of a presigned SigV4 URL, URL-encoded as
// botocore writes it.
func presignedV4(scope string) string {
	return "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + url.QueryEscape(scope) +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=abc"
}

// presignedV2 is the query string of a presigned SigV2 URL.
func presignedV2(key string) string {
	return "AWSAccessKeyId=" + url.QueryEscape(key) + "&Expires=1700000000&Signature=abc"
}

func credentialRequest(t *testing.T, authorization, query string) *http.Request {
	t.Helper()

	target := "http://localhost:4566/bucket/key"
	if query != "" {
		target += "?" + query
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}

	return r
}

func Test_FromRequest(t *testing.T) {
	const (
		key      = "AKIAIOSFODNN7EXAMPLE"
		fullV4   = key + "/20130524/" + euWest1 + "/s3/aws4_request"
		sigV2Hdr = "AWS " + key + ":frJIUN8DYpKDtOLCwo//yllqDzg="
	)

	tests := []struct {
		name     string
		giveAuth string
		giveQS   string
		want     account.Credential
		wantOK   bool
	}{
		// A SigV4 Authorization header.
		{
			name:     "sigv4",
			giveAuth: sign(key, usEast1),
			want:     account.Credential{AccessKey: key, SigningName: "sqs", Region: usEast1},
			wantOK:   true,
		},
		{
			name:     "sigv4 account as key",
			giveAuth: sign("123456789012", euWest1),
			want:     account.Credential{AccessKey: "123456789012", SigningName: "sqs", Region: euWest1},
			wantOK:   true,
		},
		{
			name:     "sigv4 scheme in lower case",
			giveAuth: "aws4-hmac-sha256 Credential=" + fullV4 + ", SignedHeaders=host, Signature=abc",
			want:     account.Credential{AccessKey: key, SigningName: "s3", Region: euWest1},
			wantOK:   true,
		},
		{
			name:     "sigv4 without spaces after the commas",
			giveAuth: "AWS4-HMAC-SHA256 SignedHeaders=host,Credential=" + fullV4 + ",Signature=abc",
			want:     account.Credential{AccessKey: key, SigningName: "s3", Region: euWest1},
			wantOK:   true,
		},
		{
			name:     "sigv4 with a quoted credential",
			giveAuth: `AWS4-HMAC-SHA256 Credential="` + fullV4 + `", SignedHeaders=host, Signature=abc`,
			want:     account.Credential{AccessKey: key, SigningName: "s3", Region: euWest1},
			wantOK:   true,
		},
		{
			// LocalStack's account and region regexes need only key/date/region/,
			// its service router all five fields.
			name:     "sigv4 scope of four fields has no signing name",
			giveAuth: "AWS4-HMAC-SHA256 Credential=K/20130524/" + euWest1 + "/sqs",
			want:     account.Credential{AccessKey: "K", Region: euWest1},
			wantOK:   true,
		},
		{
			name:     "sigv4 scope of six fields has no signing name",
			giveAuth: "AWS4-HMAC-SHA256 Credential=K/20130524/" + euWest1 + "/sqs/aws4_request/x",
			want:     account.Credential{AccessKey: "K", Region: euWest1},
			wantOK:   true,
		},
		{name: "sigv4 without a credential", giveAuth: "AWS4-HMAC-SHA256 SignedHeaders=host"},
		{name: "sigv4 with a short scope", giveAuth: "AWS4-HMAC-SHA256 Credential=AKIA/20130524/us-east-1"},
		{name: "sigv4 with an empty region", giveAuth: "AWS4-HMAC-SHA256 Credential=K/20130524//sqs/aws4_request"},
		{name: "sigv4 with an empty key", giveAuth: "AWS4-HMAC-SHA256 Credential=/20130524/us-east-1/sqs/aws4_request"},
		{name: "sigv4 scheme alone", giveAuth: "AWS4-HMAC-SHA256"},

		// A SigV2 Authorization header.
		{name: "sigv2", giveAuth: sigV2Hdr, want: account.Credential{AccessKey: key}, wantOK: true},
		{
			name: "sigv2 without signature", giveAuth: "AWS 123456789012",
			want: account.Credential{AccessKey: "123456789012"}, wantOK: true,
		},
		{name: "sigv2 with an empty key", giveAuth: "AWS :sig"},
		{name: "sigv2 prefix alone", giveAuth: "AWS "},

		// Neither.
		{name: "other scheme", giveAuth: "Bearer abc"},
		{name: "nothing"},

		// The query string of a presigned SigV4 URL.
		{
			name:   "presigned sigv4",
			giveQS: presignedV4(fullV4),
			want:   account.Credential{AccessKey: key, SigningName: "s3", Region: euWest1},
			wantOK: true,
		},
		{name: "presigned sigv4 with a short scope", giveQS: presignedV4(key + "/20130524/" + euWest1)},
		{name: "presigned sigv4 with an empty region", giveQS: presignedV4(key + "/20130524//s3/aws4_request")},
		{name: "presigned sigv4 with an empty credential", giveQS: "X-Amz-Credential=&AWSAccessKeyId=" + key},

		// The query string of a presigned SigV2 URL.
		{name: "presigned sigv2", giveQS: presignedV2(key), want: account.Credential{AccessKey: key}, wantOK: true},
		{name: "presigned sigv2 with an empty key", giveQS: presignedV2("")},
		{name: "a query without a credential", giveQS: "versioning"},

		// The order of the sources.
		{
			name:   "the sigv4 query wins over the sigv2 one",
			giveQS: presignedV2("V2KEY") + "&" + presignedV4(fullV4),
			want:   account.Credential{AccessKey: key, SigningName: "s3", Region: euWest1},
			wantOK: true,
		},
		{
			name:     "a header wins over the query",
			giveAuth: sigV2Hdr,
			giveQS:   presignedV4("OTHER/20130524/us-west-2/s3/aws4_request"),
			want:     account.Credential{AccessKey: key},
			wantOK:   true,
		},
		{
			// LocalStack's S3 reads the presigned query only when the request has
			// no Authorization header.
			name:     "a header that is not a credential still keeps the query out",
			giveAuth: "Bearer abc",
			giveQS:   presignedV4(fullV4),
		},
		{
			name:     "a malformed header still keeps the query out",
			giveAuth: "AWS4-HMAC-SHA256 Credential=K",
			giveQS:   presignedV4(fullV4),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := account.FromRequest(credentialRequest(t, tt.giveAuth, tt.giveQS))

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_Resolve(t *testing.T) {
	tests := []struct {
		name       string
		giveAuth   string
		giveQS     string
		giveParity bool
		want       account.Identity
	}{
		{
			name: "no credential",
			want: account.Identity{AccountID: zeroAccount, Region: usEast1, Authorization: ""},
		},
		{
			name:     "known region and account key",
			giveAuth: sign("123456789012", euWest1),
			want: account.Identity{
				AccountID: "123456789012", Region: euWest1, Authorization: sign("123456789012", euWest1),
			},
		},
		{
			name:     "test key",
			giveAuth: sign("test", "us-west-2"),
			want:     account.Identity{AccountID: zeroAccount, Region: "us-west-2", Authorization: sign("test", "us-west-2")},
		},
		{
			name:       "parity decodes AKIA",
			giveAuth:   sign("AKIARZPUZDIKGB2VALC4", "ap-south-1"),
			giveParity: true,
			want: account.Identity{
				AccountID: "123456789012", Region: "ap-south-1", Authorization: sign("AKIARZPUZDIKGB2VALC4", "ap-south-1"),
			},
		},
		{
			name:     "unknown region is rewritten",
			giveAuth: sign("123456789012", "xx-south-1"),
			want: account.Identity{
				AccountID: "123456789012", Region: usEast1, Authorization: sign("123456789012", usEast1),
			},
		},
		{
			name:     "gov region is outside the aws partition",
			giveAuth: sign("test", "us-gov-west-1"),
			want:     account.Identity{AccountID: zeroAccount, Region: usEast1, Authorization: sign("test", usEast1)},
		},
		{
			// botocore's get_available_regions("s3", "aws") has no aws-global, so
			// LocalStack rewrites it as it does any other unknown region.
			name:     "aws-global is rewritten",
			giveAuth: sign("123456789012", "aws-global"),
			want: account.Identity{
				AccountID: "123456789012", Region: usEast1, Authorization: sign("123456789012", usEast1),
			},
		},
		{
			name:     "sigv2 has account and no region",
			giveAuth: "AWS 123456789012:sig",
			want:     account.Identity{AccountID: "123456789012", Region: usEast1, Authorization: "AWS 123456789012:sig"},
		},
		{
			name:     "a malformed header gives the defaults",
			giveAuth: "AWS4-HMAC-SHA256 Credential=123456789012/20130524",
			want: account.Identity{
				AccountID: zeroAccount, Region: usEast1, Authorization: "AWS4-HMAC-SHA256 Credential=123456789012/20130524",
			},
		},
		{
			name:   "a presigned sigv4 URL gives its key's account and its region",
			giveQS: presignedV4(accessKey(t, "LKIA", "123456789012") + "/20130524/" + euWest1 + "/s3/aws4_request"),
			want:   account.Identity{AccountID: "123456789012", Region: euWest1},
		},
		{
			name:   "a presigned sigv4 URL in an unknown region is not rewritten",
			giveQS: presignedV4(accessKey(t, "LSIA", "123456789013") + "/20130524/xx-south-1/s3/aws4_request"),
			want:   account.Identity{AccountID: "123456789013", Region: usEast1},
		},
		{
			name:   "a presigned sigv2 URL gives its key's account and the default region",
			giveQS: presignedV2(accessKey(t, "LSIA", "424242424243")),
			want:   account.Identity{AccountID: "424242424243", Region: usEast1},
		},
		{
			name:       "parity decodes the AKIA key of a presigned URL",
			giveQS:     presignedV2("AKIARZPUZDIKGB2VALC4"),
			giveParity: true,
			want:       account.Identity{AccountID: "123456789012", Region: usEast1},
		},
		{
			name:       "an LKIA key of a presigned URL is not decoded under parity",
			giveQS:     presignedV4(accessKey(t, "LKIA", "123456789012") + "/20130524/" + euWest1 + "/s3/aws4_request"),
			giveParity: true,
			want:       account.Identity{AccountID: zeroAccount, Region: euWest1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, account.Resolve(credentialRequest(t, tt.giveAuth, tt.giveQS), tt.giveParity))
		})
	}
}
