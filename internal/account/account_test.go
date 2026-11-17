package account_test

import (
	"encoding/base32"
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	alphabet      = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	accountOffset = 549755813888

	zeroAccount = "000000000000"
	usEast1     = "us-east-1"
)

// accessKey builds an access key ID the way moto's
// generate_access_key_id_from_account_id does, so a vector can be made for any
// account.
func accessKey(t *testing.T, prefix string, accountID string) string {
	t.Helper()

	n, err := strconv.ParseInt(accountID, 10, 64)
	require.NoError(t, err)

	encoded := n/2 + accountOffset

	var raw [5]byte
	for i := len(raw) - 1; i >= 0; i-- {
		raw[i] = byte(encoded & 0xff)
		encoded >>= 8
	}

	middle := alphabet[0]
	if n%2 == 1 {
		middle = alphabet[16]
	}

	return prefix + base32.StdEncoding.EncodeToString(raw[:]) + string(middle) + "XXXXXXX"
}

// sign returns a SigV4 Authorization header for sqs.
func sign(key, region string) string {
	return "AWS4-HMAC-SHA256 Credential=" + key + "/20130524/" + region +
		"/sqs/aws4_request, SignedHeaders=host, Signature=abc"
}

func Test_FromAccessKey(t *testing.T) {
	tests := []struct {
		name       string
		giveKey    string
		giveParity bool
		want       string
	}{
		// Vectors from the docstring of extract_account_id_from_access_key_id in
		// aws/accounts.py.
		{name: "ASIA zero account, parity", giveKey: "ASIAQAAAAAAAGMKEM7X5", giveParity: true, want: zeroAccount},
		{name: "AKIA 123456789012, parity", giveKey: "AKIARZPUZDIKGB2VALC4", giveParity: true, want: "123456789012"},
		{name: "AKIA zero account, parity", giveKey: "AKIAQAAAAAAALX6GRE2E", giveParity: true, want: zeroAccount},
		{name: "AKIA 123456789012, no parity", giveKey: "AKIARZPUZDIKGB2VALC4", want: zeroAccount},
		{name: "ASIA 123456789012, no parity", giveKey: accessKey(t, "ASIA", "123456789012"), want: zeroAccount},
		{name: "LKIA even account", giveKey: accessKey(t, "LKIA", "123456789012"), want: "123456789012"},
		{name: "LSIA odd account", giveKey: accessKey(t, "LSIA", "123456789013"), want: "123456789013"},
		{name: "LSIA highest account", giveKey: accessKey(t, "LSIA", "999999999999"), want: "999999999999"},
		{name: "LSIA account 1", giveKey: accessKey(t, "LSIA", "000000000001"), want: "000000000001"},
		{
			name:       "LKIA under parity is not decoded",
			giveKey:    accessKey(t, "LKIA", "123456789012"),
			giveParity: true,
			want:       zeroAccount,
		},
		{
			name:       "ASIA odd account, parity",
			giveKey:    accessKey(t, "ASIA", "424242424243"),
			giveParity: true,
			want:       "424242424243",
		},
		{name: "twelve digits", giveKey: "123456789012", want: "123456789012"},
		{name: "twelve digits, parity", giveKey: "123456789012", giveParity: true, want: "123456789012"},
		{name: "twelve zero digits", giveKey: zeroAccount, want: zeroAccount},
		{name: "eleven digits", giveKey: "12345678901", want: zeroAccount},
		{name: "twelve digits then letters is not an account", giveKey: "123456789012abc", want: zeroAccount},
		{name: "unicode digits are not digits", giveKey: "١٢٣٤٥٦٧٨٩٠١٢", want: zeroAccount},
		{name: "test", giveKey: "test", want: zeroAccount},
		{name: "no key", giveKey: "", want: zeroAccount},
		{name: "unknown prefix", giveKey: "ABCDQAAAAAAAGMKEM7X5", giveParity: true, want: zeroAccount},
		{name: "short LKIA", giveKey: "LKIAQAAAAAAAGMKEM7X", want: zeroAccount},
		{name: "base32 part with a bad character", giveKey: "LKIAQAAAA1AAGMKEM7X5", want: zeroAccount},
		{name: "lower case base32 part", giveKey: "LKIAqaaaaaaaGMKEM7X5", want: zeroAccount},
		{name: "bad character after the account part", giveKey: "LKIAQAAAAAAA1MKEM7X5", want: zeroAccount},
		{name: "below the account offset", giveKey: "LKIAAAAAAAAAGMKEM7X5", want: zeroAccount},
		{name: "above the highest account", giveKey: "LKIA77777777GMKEM7X5", want: zeroAccount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, account.FromAccessKey(tt.giveKey, tt.giveParity))
		})
	}
}

func Test_AccessKeyFromAuthorization(t *testing.T) {
	tests := []struct {
		name string
		give string
		want string
	}{
		{
			name: "sigv4",
			give: sign("AKIAIOSFODNN7EXAMPLE", usEast1),
			want: "AKIAIOSFODNN7EXAMPLE",
		},
		{name: "sigv4 account as key", give: sign("123456789012", "eu-west-1"), want: "123456789012"},
		{name: "sigv2", give: "AWS AKIAIOSFODNN7EXAMPLE:frJIUN8DYpKDtOLCwo//yllqDzg=", want: "AKIAIOSFODNN7EXAMPLE"},
		{name: "sigv2 without signature", give: "AWS 123456789012", want: "123456789012"},
		{name: "sigv4 without a credential", give: "AWS4-HMAC-SHA256 SignedHeaders=host", want: ""},
		{name: "sigv4 with a short scope", give: "AWS4-HMAC-SHA256 Credential=AKIA/20130524/us-east-1", want: ""},
		{name: "other scheme", give: "Bearer abc", want: ""},
		{name: "empty", give: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, account.AccessKeyFromAuthorization(tt.give))
		})
	}
}

func Test_RegionFromAuthorization(t *testing.T) {
	tests := []struct {
		name string
		give string
		want string
	}{
		{name: "sigv4", give: sign("K", "eu-west-1"), want: "eu-west-1"},
		{name: "sigv2 has no scope", give: "AWS K:sig", want: ""},
		{name: "empty region", give: "AWS4-HMAC-SHA256 Credential=K/20130524//sqs/aws4_request", want: ""},
		{name: "missing trailing slash", give: "AWS4-HMAC-SHA256 Credential=K/20130524/eu-west-1", want: ""},
		{name: "empty", give: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, account.RegionFromAuthorization(tt.give))
		})
	}
}

func Test_Resolve(t *testing.T) {
	tests := []struct {
		name       string
		give       string
		giveParity bool
		want       account.Identity
	}{
		{
			name: "no header",
			give: "",
			want: account.Identity{AccountID: zeroAccount, Region: usEast1, Authorization: ""},
		},
		{
			name: "known region and account key",
			give: sign("123456789012", "eu-west-1"),
			want: account.Identity{
				AccountID: "123456789012", Region: "eu-west-1", Authorization: sign("123456789012", "eu-west-1"),
			},
		},
		{
			name: "test key",
			give: sign("test", "us-west-2"),
			want: account.Identity{AccountID: zeroAccount, Region: "us-west-2", Authorization: sign("test", "us-west-2")},
		},
		{
			name:       "parity decodes AKIA",
			give:       sign("AKIARZPUZDIKGB2VALC4", "ap-south-1"),
			giveParity: true,
			want: account.Identity{
				AccountID: "123456789012", Region: "ap-south-1", Authorization: sign("AKIARZPUZDIKGB2VALC4", "ap-south-1"),
			},
		},
		{
			name: "unknown region is rewritten",
			give: sign("123456789012", "xx-south-1"),
			want: account.Identity{
				AccountID: "123456789012", Region: usEast1, Authorization: sign("123456789012", usEast1),
			},
		},
		{
			name: "gov region is outside the aws partition",
			give: sign("test", "us-gov-west-1"),
			want: account.Identity{AccountID: zeroAccount, Region: usEast1, Authorization: sign("test", usEast1)},
		},
		{
			name: "sigv2 has account and no region",
			give: "AWS 123456789012:sig",
			want: account.Identity{AccountID: "123456789012", Region: usEast1, Authorization: "AWS 123456789012:sig"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, account.Resolve(tt.give, tt.giveParity))
		})
	}
}

func Test_RewriteRegion(t *testing.T) {
	got := account.RewriteRegion(sign("K", "xx-south-1"), usEast1)

	assert.Equal(t, sign("K", usEast1), got)
	assert.Equal(t, "no scope", account.RewriteRegion("no scope", usEast1))
}

func Test_KnownRegion(t *testing.T) {
	assert.True(t, account.KnownRegion(usEast1))
	assert.True(t, account.KnownRegion("eu-central-2"))
	assert.False(t, account.KnownRegion("xx-south-1"))
	assert.False(t, account.KnownRegion(""))
}
