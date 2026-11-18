package config

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/require"
)

const (
	persistDefaultOff        = "PERSIST_DEFAULT=0"
	persistSecretsManagerOff = "PERSIST_SECRETSMANAGER=0"
	persistCognitoOn         = "PERSIST_COGNITO=1"
	persistSSMOn             = "PERSIST_SSM=1"
	dataDir                  = "/data"
)

func Test_parsePersist(t *testing.T) {
	tests := []struct {
		name         string
		give         []string
		wantDefault  bool
		wantServices map[string]bool
		wantFormats  []string
		wantFreq     time.Duration
		wantBaseDir  string
		wantConfig   bool
		wantWarnings int
	}{
		{
			name:         "nothing set",
			give:         []string{"PATH=/bin"},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
		},
		{
			name:         "every value form is accepted case-insensitively",
			give:         []string{"PERSIST_S3=TRUE", "PERSIST_SQS=0", "PERSIST_SNS=False", "PERSIST_SES=1"},
			wantDefault:  true,
			wantServices: map[string]bool{"s3": true, serviceSQS: false, "sns": false, "ses": true},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
			wantConfig:   true,
		},
		{
			name:         "anything else warns and is ignored",
			give:         []string{"PERSIST_S3=yes", "PERSIST_SQS=2", "PERSIST_DEFAULT=on"},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
			wantConfig:   true,
			wantWarnings: 3,
		},
		{
			name:         "default off with one service on",
			give:         []string{persistDefaultOff, "PERSIST_SQS=1"},
			wantDefault:  false,
			wantServices: map[string]bool{serviceSQS: true},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
			wantConfig:   true,
		},
		{
			name:         "names are normalized and elasticsearch maps to es",
			give:         []string{"PERSIST_COGNITO_IDP=0", "PERSIST_API-GATEWAY=0", "PERSIST_ELASTICSEARCH=0"},
			wantDefault:  true,
			wantServices: map[string]bool{"cognitoidp": false, svcAPIGateway: false, "es": false},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
			wantConfig:   true,
		},
		{
			name:        "enabling a service enables its dependencies transitively",
			give:        []string{persistDefaultOff, "PERSIST_DYNAMODB=1"},
			wantDefault: false,
			wantServices: map[string]bool{
				svcDynamoDB: true, svcDynamoDBStreams: true, svcKinesis: true,
			},
			wantFormats: []string{formatJSON},
			wantFreq:    10 * time.Second,
			wantBaseDir: DefaultPersistBaseDir,
			wantConfig:  true,
		},
		{
			name:        "an explicit false is kept and does not stop expansion past it",
			give:        []string{persistDefaultOff, persistSSMOn, persistSecretsManagerOff},
			wantDefault: false,
			wantServices: map[string]bool{
				svcSSM: true, svcSecretsManager: false, svcKMS: true, svcLambda: true, "s3": true, svcSTS: true,
			},
			wantFormats: []string{formatJSON},
			wantFreq:    10 * time.Second,
			wantBaseDir: DefaultPersistBaseDir,
			wantConfig:  true,
		},
		{
			name:         "a disabled service does not enable its dependencies",
			give:         []string{"PERSIST_LAMBDA=0"},
			wantDefault:  true,
			wantServices: map[string]bool{svcLambda: false},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
			wantConfig:   true,
		},
		{
			name:         "format, frequency and base dir",
			give:         []string{"PERSIST_FORMAT=json, binary", "PERSIST_FREQUENCY=30", "PERSIST_BASE_DIR=/data"},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantFormats:  []string{formatJSON, "binary"},
			wantFreq:     30 * time.Second,
			wantBaseDir:  dataDir,
			wantConfig:   true,
		},
		{
			name:         "bad format and bad frequency",
			give:         []string{"PERSIST_FORMAT=yaml", "PERSIST_FREQUENCY=soon"},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
			wantConfig:   true,
			wantWarnings: 2,
		},
		{
			name:         "an empty value counts as unset",
			give:         []string{"PERSIST_S3=", "PERSIST_DEFAULT=  "},
			wantDefault:  true,
			wantServices: map[string]bool{},
			wantFormats:  []string{formatJSON},
			wantFreq:     10 * time.Second,
			wantBaseDir:  DefaultPersistBaseDir,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings := parsePersist(tt.give)
			require.Len(t, warnings, tt.wantWarnings)
			require.Equal(t, tt.wantDefault, got.Default)
			require.Equal(t, tt.wantServices, got.Services)
			require.Equal(t, tt.wantFormats, got.Formats)
			require.Equal(t, tt.wantFreq, got.Frequency)
			require.Equal(t, tt.wantBaseDir, got.BaseDir)
			require.Equal(t, tt.wantConfig, got.Configured)
		})
	}
}

func Test_Persist_Enabled(t *testing.T) {
	settings, _ := parsePersist([]string{persistDefaultOff, "PERSIST_COGNITO_IDP=1", "PERSIST_ELASTICSEARCH=1"})
	tests := []struct {
		name string
		give string
		want bool
	}{
		{name: "hyphenated botocore name matches the underscored variable", give: svcCognitoIDP, want: true},
		{name: "es matches PERSIST_ELASTICSEARCH", give: "es", want: true},
		{name: "dependency of es", give: "opensearch", want: true},
		{name: "service without a setting takes the default", give: serviceSQS, want: false},
		{name: "case is ignored", give: "COGNITO-IDP", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, settings.Enabled(tt.give))
		})
	}
}

func Test_Config_PersistConfig(t *testing.T) {
	load := func(t *testing.T, environ ...string) *Config {
		t.Helper()
		isolate(t)
		cfg, err := newConfig(environ, dockerProbe{fsys: fstest.MapFS{}})
		require.NoError(t, err)

		return cfg
	}

	t.Run("nothing set", func(t *testing.T) {
		got := load(t).PersistConfig()

		require.False(t, got.Persistence)
		require.False(t, got.PersistVarSet)
		require.NotNil(t, got.Default)
		require.True(t, *got.Default)
		require.Empty(t, got.Services)
		require.Equal(t, []persist.Format{persist.FormatJSON}, got.Formats)
		require.Equal(t, 10*time.Second, got.Frequency)
		require.Empty(t, persist.New(got).BaseDir(), "no PERSIST_* variable and no mount: persistence is off")
	})

	t.Run("the variables carry over", func(t *testing.T) {
		got := load(t,
			persistDefaultOff, "PERSIST_SQS=1", "PERSIST_FORMAT=json,binary",
			"PERSIST_FREQUENCY=30", "PERSIST_BASE_DIR=/data",
		).PersistConfig()

		require.True(t, got.PersistVarSet)
		require.False(t, *got.Default)
		require.Equal(t, map[string]bool{serviceSQS: true}, got.Services)
		require.Equal(t, []persist.Format{persist.FormatJSON, persist.FormatBinary}, got.Formats)
		require.Equal(t, 30*time.Second, got.Frequency)
		require.Equal(t, dataDir, persist.New(got).BaseDir())
	})

	t.Run("a registered name is found under its own spelling", func(t *testing.T) {
		got := load(t, persistDefaultOff, "PERSIST_COGNITO_IDP=1", "PERSIST_ELASTICSEARCH=1").
			PersistConfig(svcCognitoIDP, "es", serviceSQS)
		engine := persist.New(got)

		require.True(t, engine.Enabled(svcCognitoIDP))
		require.True(t, engine.Enabled("es"))
		require.True(t, engine.Enabled("opensearch"), "a dependency of es")
		require.False(t, engine.Enabled(serviceSQS), "no setting, and PERSIST_DEFAULT is off")
	})

	t.Run("a frequency of zero reaches the engine as a setting, not as unset", func(t *testing.T) {
		got := load(t, "PERSIST_FREQUENCY=0").PersistConfig()

		require.Equal(t, persist.Immediate, got.Frequency)
	})

	t.Run("a lower-case variable name reaches the engine", func(t *testing.T) {
		got := load(t, "persist_default=0", "persist_sqs=1").PersistConfig(serviceSQS, "sts")
		engine := persist.New(got)

		require.True(t, got.PersistVarSet)
		require.True(t, engine.Enabled(serviceSQS))
		require.False(t, engine.Enabled("sts"))
	})

	t.Run("a hyphenated expansion is not found under a registered name", func(t *testing.T) {
		got := load(t, persistDefaultOff, persistCognitoOn).PersistConfig(svcCognitoIDP)

		require.False(t, persist.New(got).Enabled(svcCognitoIDP), "localstack-persist never finds it either")
	})

	t.Run("PERSISTENCE=1 alone uses the state directory", func(t *testing.T) {
		isolate(t)
		t.Setenv(varPersistence, "1")
		cfg, err := newConfig(nil, dockerProbe{fsys: fstest.MapFS{}})
		require.NoError(t, err)

		require.Equal(t, persist.StateDir, persist.New(cfg.PersistConfig()).BaseDir())
	})
}
