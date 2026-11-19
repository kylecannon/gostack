package kms

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/stretchr/testify/require"
)

// Test_mutating holds every operation of the API to one side of the persistence
// rule, so an operation added to the model has to be placed.
func Test_mutating(t *testing.T) {
	for _, op := range api.Operations {
		require.NotEqual(t, mutatingOps[op], readOnlyOps[op], "%s must be in exactly one of the two lists", op)
		require.Equal(t, mutatingOps[op], mutating(op), op)
	}

	require.Len(t, api.Operations, len(mutatingOps)+len(readOnlyOps))

	for _, op := range []string{"Encrypt", "Decrypt", "Sign", "Verify", "GenerateRandom", "GenerateMac", "GenerateDataKey"} {
		require.False(t, mutating(op), op)
	}
}

func Test_isKeyID(t *testing.T) {
	tests := []struct {
		name   string
		giveID string
		want   bool
	}{
		{"a UUID", "134f2428-cec1-4b25-a1ae-9048164dba47", true},
		{"an upper-case UUID", "134F2428-CEC1-4B25-A1AE-9048164DBA47", true},
		{"a UUID without dashes", "134f2428cec14b25a1ae9048164dba47", false},
		{"a UUID with a dash moved", "134f242-8cec1-4b25-a1ae-9048164dba47", false},
		{"an mrk- id", "mrk-d3b95762d3b95762d3b95762d3b95762", true},
		{"a short mrk- id", "mrk-d3b95762", false},
		{"an mrk- id that is not hex", "mrk-fake-key-idfake-key-idfake-key", false},
		{"nothing", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isKeyID(tt.giveID))
		})
	}
}

func Test_parseKeyARN(t *testing.T) {
	tests := []struct {
		name    string
		giveARN string
		want    parsedARN
		wantOK  bool
	}{
		{
			"a key ARN", "arn:aws:kms:us-east-1:111111111111:key/abc",
			parsedARN{region: "us-east-1", account: "111111111111", keyID: "abc"},
			true,
		},
		{
			"an alias ARN keeps alias/", "arn:aws-cn:kms:cn-north-1:111111111111:alias/x",
			parsedARN{region: "cn-north-1", account: "111111111111", keyID: "alias/x"},
			true,
		},
		{"an unknown partition", "arn:aws-x:kms:us-east-1:111111111111:key/abc", parsedARN{}, false},
		{"another service", "arn:aws:sqs:us-east-1:111111111111:key/abc", parsedARN{}, false},
		{"a short account", "arn:aws:kms:us-east-1:1111:key/abc", parsedARN{}, false},
		{"an empty key id", "arn:aws:kms:us-east-1:111111111111:key/", parsedARN{}, false},
		{"a colon in the id", "arn:aws:kms:us-east-1:111111111111:key/a:b", parsedARN{}, false},
		{"a key id", "134f2428-cec1-4b25-a1ae-9048164dba47", parsedARN{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseKeyARN(tt.giveARN)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// FuzzResolveID holds the id parsers to their contract: they never panic, and
// an ARN parseKeyARN accepts names a region, a 12-digit account and a key id
// or alias with no colon.
func FuzzResolveID(f *testing.F) {
	for _, seed := range []string{
		"134f2428-cec1-4b25-a1ae-9048164dba47",
		"mrk-d3b95762d3b95762d3b95762d3b95762",
		"alias/aws/s3",
		"arn:aws:kms:us-east-1:111111111111:key/134f2428-cec1-4b25-a1ae-9048164dba47",
		"arn:aws:kms:us-east-1:111111111111:alias/a",
		"arn:aws:kms:us-east-1:111111111111",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, id string) {
		_ = isKeyID(id)

		parsed, ok := parseKeyARN(id)
		if ok {
			require.NotEmpty(t, parsed.region)
			require.Len(t, parsed.account, accountIDLen)
			require.NotEmpty(t, parsed.keyID)
			require.NotContains(t, parsed.keyID, ":")
		}

		_, _, _, _ = splitARN("us-east-1", id)
	})
}
