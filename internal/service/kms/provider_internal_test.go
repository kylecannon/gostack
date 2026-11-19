package kms

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
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

	readOnly := []string{"Encrypt", "Decrypt", "Sign", "Verify", "GenerateRandom", "GenerateMac", "GenerateDataKey"}
	for _, op := range readOnly {
		require.False(t, mutating(op), op)
	}
}

// sampleKeyID is a key id of the UUID form.
const sampleKeyID = "134f2428-cec1-4b25-a1ae-9048164dba47"

// testRegion is the region the internal tests call from.
const testRegion = "us-east-1"

// Test_dataKey holds the two steps every data operation takes to its key:
// dataKey finds it and refuses a disabled key or one pending deletion, then
// Key.usable refuses another usage before a key pending import.
func Test_dataKey(t *testing.T) {
	const other = "111111111111"

	p := New(func(string) {})
	rc := &request.Context{AccountID: "000000000000", Region: testRegion}

	add := func(account string, req keyRequest, state string) *Key {
		t.Helper()

		k, err := p.newKey(account, rc.Region, req)
		require.NoError(t, err)

		k.state = state
		p.stateAt(account, rc.Region).addKey(k)

		return k
	}

	symmetric := keyRequest{spec: specSymmetric, usage: usageEncrypt, origin: originAWS}
	enabled := add(rc.AccountID, symmetric, stateEnabled)
	disabled := add(rc.AccountID, symmetric, stateDisabled)
	deleting := add(rc.AccountID, symmetric, statePendingDeletion)
	importing := add(rc.AccountID, symmetric, statePendingImport)
	foreign := add(other, symmetric, stateEnabled)

	tests := []struct {
		name      string
		giveID    string
		giveUsage string
		wantKey   *Key
		wantCode  string
		wantMsg   string
	}{
		{name: "an enabled key", giveID: enabled.ID, giveUsage: usageEncrypt, wantKey: enabled},
		{name: "a key ARN in another account", giveID: foreign.ARN, giveUsage: usageEncrypt, wantKey: foreign},
		{
			name: "a disabled key", giveID: disabled.ID, giveUsage: usageEncrypt,
			wantCode: "DisabledException", wantMsg: disabled.ARN + " is disabled.",
		},
		{
			name: "a key pending deletion", giveID: deleting.ID, giveUsage: usageEncrypt,
			wantCode: "KMSInvalidStateException", wantMsg: deleting.ARN + " is pending deletion.",
		},
		{
			name: "a key of another usage", giveID: enabled.ID, giveUsage: usageSign,
			wantCode: "InvalidKeyUsageException",
			wantMsg:  enabled.ARN + " key usage is ENCRYPT_DECRYPT which is not valid for Op.",
		},
		{
			name: "a key pending import", giveID: importing.ID, giveUsage: usageEncrypt,
			wantCode: "KMSInvalidStateException", wantMsg: importing.ARN + " is pending import.",
		},
		{
			name: "a key ARN in another region", giveID: keyARN(rc.AccountID, "us-west-2", enabled.ID),
			giveUsage: usageEncrypt, wantCode: "NotFoundException", wantMsg: "Invalid arn us-west-2",
		},
		{
			name: "an unknown key", giveID: sampleKeyID, giveUsage: usageEncrypt, wantCode: "NotFoundException",
			wantMsg: "Key '" + keyARN(rc.AccountID, rc.Region, sampleKeyID) + "' does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := p.dataKey(rc, &tt.giveID)
			if err == nil {
				err = k.usable(tt.giveUsage, "Op")
			}

			if tt.wantCode == "" {
				require.NoError(t, err)
				require.Same(t, tt.wantKey, k)

				return
			}

			var awsErr *awserr.Error
			require.ErrorAs(t, err, &awsErr)
			require.Equal(t, tt.wantCode, awsErr.Code)
			require.Equal(t, tt.wantMsg, awsErr.Message)
		})
	}
}

func Test_isKeyID(t *testing.T) {
	tests := []struct {
		name   string
		giveID string
		want   bool
	}{
		{"a UUID", sampleKeyID, true},
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
			parsedARN{region: testRegion, account: "111111111111", keyID: "abc"},
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
		{"a key id", sampleKeyID, parsedARN{}, false},
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
		sampleKeyID,
		"mrk-d3b95762d3b95762d3b95762d3b95762",
		"alias/aws/s3",
		"arn:aws:kms:us-east-1:111111111111:key/" + sampleKeyID,
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

		_, _ = splitARN(testRegion, id)
	})
}
