package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The expected values below are what gresau/localstack-persist 2c15f6f does with
// the same environment. config.py is src/localstack_persist/config.py, state.py is
// src/localstack_persist/state.py, and bootstrap.py is LocalStack 8b9a79f
// localstack-core/localstack/utils/bootstrap.py.

func Test_parsePersist_Frequency(t *testing.T) {
	tests := []struct {
		name         string
		give         string
		wantFreq     time.Duration
		wantWarnings int
	}{
		{
			name:     "a decimal is a fraction of a second",
			give:     "0.5", // config.py:79 float("0.5"); state.py:157 cond.wait(0.5)
			wantFreq: 500 * time.Millisecond,
		},
		{
			name:     "zero waits no time",
			give:     "0", // float("0") = 0.0; Condition.wait(0) does not block, so a change is saved at once
			wantFreq: 0,
		},
		{
			name:     "a negative number waits no time either",
			give:     "-1", // float("-1") = -1.0; Condition.wait(-1) does not block
			wantFreq: 0,
		},
		{
			name:         "text is ignored with a warning and the default stays",
			give:         "abc", // config.py:79 float("abc") raises, line 81 warns, PERSIST_FREQUENCY stays 10
			wantFreq:     DefaultPersistFrequency,
			wantWarnings: 1,
		},
		{
			name:     "a whole number of seconds",
			give:     "10", // float("10") = 10.0
			wantFreq: 10 * time.Second,
		},
		{
			name:     "the value is stripped",
			give:     " 2 ", // config.py:79 float(value.strip())
			wantFreq: 2 * time.Second,
		},
		{
			name:     "an exponent",
			give:     "1e-1", // float("1e-1") = 0.1
			wantFreq: 100 * time.Millisecond,
		},
		{
			name:         "nan is not a frequency",
			give:         "nan", // Python parses it and wait(nan) does not block, a spin; refused here
			wantFreq:     DefaultPersistFrequency,
			wantWarnings: 1,
		},
		{
			name:         "infinity is not a frequency",
			give:         "inf", // Python parses it and wait(inf) raises OverflowError in the saving thread; refused here
			wantFreq:     DefaultPersistFrequency,
			wantWarnings: 1,
		},
		{
			name:         "beyond a float is not a frequency",
			give:         "1e400", // float("1e400") = inf
			wantFreq:     DefaultPersistFrequency,
			wantWarnings: 1,
		},
		{
			name:     "a huge but finite number is capped",
			give:     "1e300",
			wantFreq: time.Duration(1<<63 - 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings := parsePersist([]string{"PERSIST_FREQUENCY=" + tt.give})

			require.Len(t, warnings, tt.wantWarnings)
			require.Equal(t, tt.wantFreq, got.Frequency)
		})
	}
}

func Test_parsePersist_Names(t *testing.T) {
	serverless := map[string]bool{
		// bootstrap.py:124-134 API_COMPOSITES["serverless"], and what each member needs
		// (bootstrap.py:60-75): dynamodb -> dynamodbstreams -> kinesis, lambda and
		// cloudformation -> s3 and sts. The composite itself is only the entry config.py:96 sets.
		svcServerless: true, svcCloudFormation: true, "cloudwatch": true, "iam": true, svcSTS: true,
		svcLambda: true, svcDynamoDB: true, svcAPIGateway: true, "s3": true,
		"dynamodbstreams": true, "kinesis": true,
	}
	tests := []struct {
		name         string
		give         []string
		wantDefault  bool
		wantServices map[string]bool
		wantWarnings int
	}{
		{
			name:        "an explicit false does not stop the services behind it",
			give:        []string{persistDefaultOff, persistSSMOn, persistSecretsManagerOff},
			wantDefault: false,
			// config.py:97-98 setdefaults every name resolve_apis returns for ssm, which
			// goes through secretsmanager to kms and lambda, and lambda to s3 and sts. Only
			// secretsmanager itself keeps its own False (config.py:96, 100).
			wantServices: map[string]bool{
				svcSSM: true, svcSecretsManager: false, svcKMS: true, svcLambda: true, "s3": true, svcSTS: true,
			},
		},
		{
			name:        "the order of the variables makes no difference",
			give:        []string{persistSecretsManagerOff, persistSSMOn, "PERSIST_DEFAULT=0"},
			wantDefault: false,
			wantServices: map[string]bool{
				svcSSM: true, svcSecretsManager: false, svcKMS: true, svcLambda: true, "s3": true, svcSTS: true,
			},
		},
		{
			name:         "a group name is a service name of its own",
			give:         []string{"PERSIST_SERVERLESS=1"},
			wantDefault:  true,
			wantServices: serverless,
		},
		{
			name:         "a group turned off turns off nothing else",
			give:         []string{"PERSIST_SERVERLESS=0"},
			wantDefault:  true,
			wantServices: map[string]bool{svcServerless: false},
		},
		{
			name:        "a composite's members keep LocalStack's hyphens",
			give:        []string{persistCognitoOn},
			wantDefault: true,
			// bootstrap.py:135 lists "cognito-idp", and nothing normalizes what resolve_apis returns.
			wantServices: map[string]bool{svcCognito: true, svcCognitoIDP: true, svcCognitoIdentity: true},
		},
		{
			name:         "the prefix is matched in any case",
			give:         []string{"persist_sqs=0", "Persist_S3=1"},
			wantDefault:  true,
			wantServices: map[string]bool{"sqs": false, "s3": true}, // config.py:56 key.lower().startswith("persist_")
		},
		{
			name:         "a service name is stripped of case, underscores and hyphens",
			give:         []string{"PERSIST_Dynamo_DB-Streams=1"},
			wantDefault:  true,
			wantServices: map[string]bool{"dynamodbstreams": true, "kinesis": true}, // config.py:9-24
		},
		{
			name:         "default is a service name like the others",
			give:         []string{"PERSIST_DE_FAULT=0"},
			wantDefault:  false,
			wantServices: map[string]bool{}, // config.py:93 normalises the name to "default"
		},
		{
			name:         "default in lower case",
			give:         []string{"persist_default=false"},
			wantDefault:  false,
			wantServices: map[string]bool{},
		},
		{
			name:         "true and false in any case",
			give:         []string{"PERSIST_S3=True", "PERSIST_SQS=FALSE", "PERSIST_SNS=tRuE"},
			wantDefault:  true,
			wantServices: map[string]bool{"s3": true, "sqs": false, "sns": true}, // config.py:95,99 value.lower()
		},
		{
			name:         "a value with spaces around it is not valid",
			give:         []string{"PERSIST_S3= 1", "PERSIST_SQS=0 "},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantWarnings: 2, // config.py:95,99 compare value, not value.strip()
		},
		{
			name:         "yes, 2 and on are not valid",
			give:         []string{"PERSIST_S3=yes", "PERSIST_SQS=2", "PERSIST_DEFAULT=on"},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantWarnings: 3,
		},
		{
			name:         "a value of spaces is skipped without a warning",
			give:         []string{"PERSIST_S3=   "},
			wantDefault:  true,
			wantServices: map[string]bool{}, // config.py:56 `not value.strip()`
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings := parsePersist(tt.give)

			require.Len(t, warnings, tt.wantWarnings)
			require.Equal(t, tt.wantDefault, got.Default)
			require.Equal(t, tt.wantServices, got.Services)
		})
	}
}

func Test_parsePersist_Settings(t *testing.T) {
	tests := []struct {
		name         string
		give         []string
		wantFormats  []string
		wantFreq     time.Duration
		wantBaseDir  string
		wantWarnings int
	}{
		{
			name:        "the setting names are matched in any case",
			give:        []string{"persist_format=binary", "Persist_Frequency=3", "PERSIST_base_dir=/data"},
			wantFormats: []string{formatBinary},
			wantFreq:    3 * time.Second,
			wantBaseDir: dataDir, // config.py:59,77,88 key.lower() == "persist_..."
		},
		{
			name:        "the order of the formats is kept",
			give:        []string{"PERSIST_FORMAT=binary,json"},
			wantFormats: []string{formatBinary, formatJSON}, // config.py:61-66
			wantFreq:    DefaultPersistFrequency,
			wantBaseDir: DefaultPersistBaseDir,
		},
		{
			name:        "each format is stripped and read in any case, and repeats go",
			give:        []string{"PERSIST_FORMAT= JSON , Binary,json"},
			wantFormats: []string{formatJSON, formatBinary},
			wantFreq:    DefaultPersistFrequency,
			wantBaseDir: DefaultPersistBaseDir,
		},
		{
			name:         "one bad format does not cancel the good ones",
			give:         []string{"PERSIST_FORMAT=yaml,binary"},
			wantFormats:  []string{formatBinary}, // config.py:62-74 warns per token and keeps new_formats
			wantFreq:     DefaultPersistFrequency,
			wantBaseDir:  DefaultPersistBaseDir,
			wantWarnings: 1,
		},
		{
			name:         "only commas separate formats",
			give:         []string{"PERSIST_FORMAT=json binary"},
			wantFormats:  []string{formatJSON}, // config.py:61 value.split(","): one token, not a format
			wantFreq:     DefaultPersistFrequency,
			wantBaseDir:  DefaultPersistBaseDir,
			wantWarnings: 1,
		},
		{
			name:        "the base dir is stripped",
			give:        []string{"PERSIST_BASE_DIR= /data "},
			wantFormats: []string{formatJSON}, // config.py:89 value.strip()
			wantFreq:    DefaultPersistFrequency,
			wantBaseDir: dataDir,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings := parsePersist(tt.give)

			require.Len(t, warnings, tt.wantWarnings)
			require.Equal(t, tt.wantFormats, got.Formats)
			require.Equal(t, tt.wantFreq, got.Frequency)
			require.Equal(t, tt.wantBaseDir, got.BaseDir)
			require.True(t, got.Configured)
		})
	}
}

func Test_Persist_Enabled_NeverFindsHyphenatedExpansion(t *testing.T) {
	// is_persistence_enabled (config.py:109-111) normalises the name it is asked
	// about, which drops the hyphen, so the "cognito-idp" entry PERSIST_COGNITO adds is
	// never found and the service falls back to PERSIST_DEFAULT.
	settings, _ := parsePersist([]string{persistDefaultOff, persistCognitoOn, "PERSIST_COGNITO_IDP=1"})
	require.True(t, settings.Enabled(svcCognitoIDP), "its own variable is found")
	require.False(t, settings.Enabled(svcCognitoIdentity), "the expansion is not")

	settings, _ = parsePersist([]string{persistDefaultOff, persistCognitoOn})
	require.False(t, settings.Enabled(svcCognitoIDP))
}
