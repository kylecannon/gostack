package config

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	strategyPath   = "path"
	strategyDomain = "domain"
	serviceSQS     = "sqs"
)

// isolate unsets every environment variable Load reads, in every form, so the
// test sees only what it sets. The t.Setenv call registers the restore.
func isolate(t *testing.T) {
	t.Helper()
	for _, variable := range variables {
		for _, name := range variable.envNames() {
			t.Setenv(name, "")
			require.NoError(t, os.Unsetenv(name))
		}
	}
	t.Setenv(varOverrideInDocker, "0")
}

// loadConfig calls Load. Load exports LOCALSTACK_<NAME> variables as bare names,
// so the test first registers each bare name with t.Setenv, which puts the
// environment back afterwards.
func loadConfig(t *testing.T) (*Config, error) {
	t.Helper()
	for _, variable := range strippedVariables(os.Environ()) {
		t.Setenv(variable.name, os.Getenv(variable.name))
	}

	return Load()
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := loadConfig(t)
	require.NoError(t, err)

	return cfg
}

func Test_Load_defaults(t *testing.T) {
	isolate(t)
	got := mustLoad(t).Settings()

	require.Equal(t, []HostPort{{Host: hostListenHost, Port: 4566}}, got.GatewayListen)
	require.Equal(t, HostPort{Host: DefaultLocalstackHost, Port: 4566}, got.LocalstackHost)
	require.Empty(t, got.Services)
	require.Equal(t, "standard", got.SQSEndpointStrategy)
	require.Equal(t, 5*time.Second, got.ShutdownTimeout)
	require.True(t, got.CompatPaths)
	require.True(t, got.ResponseHeaderEnabled)
	require.False(t, got.Debug)
	require.False(t, got.FailFast)
	require.False(t, got.EnableConfigUpdates)
	require.False(t, got.Persistence)
	require.True(t, got.Persist.Default)
	require.Empty(t, got.LogLevel)
}

func Test_Load_gatewayFlags(t *testing.T) {
	isolate(t)
	require.False(t, mustLoad(t).Settings().ParityAWSAccessKeyID)
	require.False(t, mustLoad(t).Settings().DisablePreflightProcessing)

	t.Setenv("PARITY_AWS_ACCESS_KEY_ID", "1")
	t.Setenv("LOCALSTACK_DISABLE_PREFLIGHT_PROCESSING", "true")
	cfg := mustLoad(t)

	require.True(t, cfg.Settings().ParityAWSAccessKeyID)
	require.True(t, cfg.Settings().DisablePreflightProcessing)
	require.Equal(t, true, cfg.Values()[varParityAWSAccessKeyID])
}

func Test_Load_listenDefaultFollowsInDocker(t *testing.T) {
	tests := []struct {
		name         string
		giveOverride string
		giveListen   string
		wantListen   []HostPort
		wantHost     HostPort
	}{
		{
			name:         "in a container the host defaults to 0.0.0.0",
			giveOverride: "1",
			wantListen:   []HostPort{{Host: dockerListenHost, Port: 4566}},
			wantHost:     HostPort{Host: DefaultLocalstackHost, Port: 4566},
		},
		{
			name:         "on a host it defaults to 127.0.0.1",
			giveOverride: "0",
			wantListen:   []HostPort{{Host: hostListenHost, Port: 4566}},
			wantHost:     HostPort{Host: DefaultLocalstackHost, Port: 4566},
		},
		{
			name:         "a missing host takes the default and the first port feeds LOCALSTACK_HOST",
			giveOverride: "1",
			giveListen:   ":4510, 127.0.0.1:4511",
			wantListen:   []HostPort{{Host: dockerListenHost, Port: 4510}, {Host: hostListenHost, Port: 4511}},
			wantHost:     HostPort{Host: DefaultLocalstackHost, Port: 4510},
		},
		{
			name:         "a missing port takes 4566",
			giveOverride: "0",
			giveListen:   "[::]",
			wantListen:   []HostPort{{Host: "::", Port: 4566}},
			wantHost:     HostPort{Host: DefaultLocalstackHost, Port: 4566},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv(varOverrideInDocker, tt.giveOverride)
			t.Setenv(varGatewayListen, tt.giveListen)
			got := mustLoad(t).Settings()
			require.Equal(t, tt.wantListen, got.GatewayListen)
			require.Equal(t, tt.wantHost, got.LocalstackHost)
			require.Equal(t, tt.giveOverride == valueOne, got.InDocker)
		})
	}
}

func Test_Load_lookupOrder(t *testing.T) {
	tests := []struct {
		name string
		give map[string]string
		want string
	}{
		{name: "bare name", give: map[string]string{varSQSEndpointStrategy: strategyPath}, want: strategyPath},
		{
			name: "LOCALSTACK_ beats bare",
			give: map[string]string{varSQSEndpointStrategy: strategyPath, "LOCALSTACK_SQS_ENDPOINT_STRATEGY": strategyDomain},
			want: strategyDomain,
		},
		{
			name: "GOSTACK_ beats LOCALSTACK_ and bare",
			give: map[string]string{
				varSQSEndpointStrategy:             strategyPath,
				"LOCALSTACK_SQS_ENDPOINT_STRATEGY": strategyDomain,
				"GOSTACK_SQS_ENDPOINT_STRATEGY":    "off",
			},
			want: "off",
		},
		{name: "empty values are skipped", give: map[string]string{"GOSTACK_SQS_ENDPOINT_STRATEGY": ""}, want: "standard"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			for name, value := range tt.give {
				t.Setenv(name, value)
			}
			require.Equal(t, tt.want, mustLoad(t).Settings().SQSEndpointStrategy)
		})
	}
}

func Test_Load_variableForms(t *testing.T) {
	tests := []struct {
		name  string
		give  map[string]string
		check func(t *testing.T, got *Settings)
	}{
		{
			name: "LOCALSTACK_HOST takes the GOSTACK_ form first, then the plain form",
			give: map[string]string{"GOSTACK_LOCALSTACK_HOST": "first.test:1", varLocalstackHost: "example.test:5000"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.Equal(t, HostPort{Host: "first.test", Port: 1}, got.LocalstackHost)
			},
		},
		{
			name: "COMPAT_PATHS has only the GOSTACK_ form",
			give: map[string]string{varCompatPaths: "0", "LOCALSTACK_COMPAT_PATHS": "0"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.True(t, got.CompatPaths)
			},
		},
		{
			name: "GOSTACK_COMPAT_PATHS=0 turns the aliases off",
			give: map[string]string{"GOSTACK_COMPAT_PATHS": "0"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.False(t, got.CompatPaths)
			},
		},
		{
			name: "ENABLE_CONFIG_UPDATES through the GOSTACK_ form",
			give: map[string]string{"GOSTACK_ENABLE_CONFIG_UPDATES": "1"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.True(t, got.EnableConfigUpdates)
			},
		},
		{
			name: "ENABLE_CONFIG_UPDATES through the LocalStack form",
			give: map[string]string{"LOCALSTACK_ENABLE_CONFIG_UPDATES": valueTrue},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.True(t, got.EnableConfigUpdates)
			},
		},
		{
			name: "booleans accept 1 and true only",
			give: map[string]string{varFailFast: "yes", varEagerServiceLoading: "TRUE", varPersistence: "1"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.False(t, got.FailFast)
				require.True(t, got.EagerServiceLoading)
				require.True(t, got.Persistence)
			},
		},
		{
			name: "response headers are on unless the value is false",
			give: map[string]string{varResponseHeaderEnabled: "False"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.False(t, got.ResponseHeaderEnabled)
			},
		},
		{
			name: "trace log level turns on DEBUG",
			give: map[string]string{varLSLog: "Trace"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.True(t, got.Debug)
				require.Equal(t, "trace", got.LogLevel)
			},
		},
		{
			name: "LOG backs up LS_LOG and unknown levels are dropped",
			give: map[string]string{varLSLog: "loud", varLog: "warn"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.False(t, got.Debug)
				require.Equal(t, "warn", got.LogLevel)
			},
		},
		{
			name: "SERVICES drops ports and blanks",
			give: map[string]string{varServices: "sqs , sts:4566,s3=4567,"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.Equal(t, []string{serviceSQS, svcSTS, "s3"}, got.Services)
			},
		},
		{
			name: "SQS flags and CORS lists",
			give: map[string]string{
				varSQSDelayRecentlyDeleted:         "1",
				varSQSEnableMessageRetentionPeriod: valueTrue,
				varSQSDelayPurgeRetry:              "1",
				varSQSDisableMaxMessageLimit:       "1",
				varDisableCORSChecks:               "1",
				varDisableCORSHeaders:              "1",
				varExtraCORSAllowedOrigins:         "https://a.test, https://b.test,,",
				varExtraCORSAllowedHeaders:         "x-a,x-b",
				varExtraCORSExposeHeaders:          "x-c",
				varCustomSSLCertPath:               "/certs/server.test.pem",
			},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.True(t, got.SQSDelayRecentlyDeleted)
				require.True(t, got.SQSEnableMessageRetentionPeriod)
				require.True(t, got.SQSDelayPurgeRetry)
				require.True(t, got.SQSDisableMaxNumberOfMessageLimit)
				require.True(t, got.DisableCORSChecks)
				require.True(t, got.DisableCORSHeaders)
				require.Equal(t, []string{"https://a.test", "https://b.test"}, got.ExtraCORSAllowedOrigins)
				require.Equal(t, []string{"x-a", "x-b"}, got.ExtraCORSAllowedHeaders)
				require.Equal(t, []string{"x-c"}, got.ExtraCORSExposeHeaders)
				require.Equal(t, "/certs/server.test.pem", got.CustomSSLCertPath)
			},
		},
		{
			name: "SHUTDOWN_TIMEOUT in seconds, zero for no limit",
			give: map[string]string{varShutdownTimeout: "0"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.Zero(t, got.ShutdownTimeout)
			},
		},
		{
			name: "PERSIST_* variables reach the settings",
			give: map[string]string{"PERSIST_DEFAULT": "0", "PERSIST_SQS": "1"},
			check: func(t *testing.T, got *Settings) {
				t.Helper()
				require.True(t, got.Persist.Configured)
				require.True(t, got.Persist.Enabled(serviceSQS))
				require.False(t, got.Persist.Enabled("s3"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			for name, value := range tt.give {
				t.Setenv(name, value)
			}
			tt.check(t, mustLoad(t).Settings())
		})
	}
}

func Test_Load_invalidValues(t *testing.T) {
	tests := []struct {
		name  string
		giveK string
		giveV string
	}{
		{name: "bad GATEWAY_LISTEN port", giveK: varGatewayListen, giveV: "0.0.0.0:http"},
		{name: "bad LOCALSTACK_HOST port", giveK: varLocalstackHost, giveV: "host:99999"},
		{name: "bad SHUTDOWN_TIMEOUT", giveK: varShutdownTimeout, giveV: "soon"},
		{name: "negative SHUTDOWN_TIMEOUT", giveK: varShutdownTimeout, giveV: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv(tt.giveK, tt.giveV)
			_, err := loadConfig(t)
			require.ErrorContains(t, err, tt.giveK)
		})
	}
}

func Test_Load_exportsStrippedNames(t *testing.T) {
	isolate(t)
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("HOSTNAME", "container")
	t.Setenv("LOCALSTACK_AWS_DEFAULT_REGION", "eu-west-1")
	t.Setenv("LOCALSTACK_HOSTNAME", "never-mapped")

	mustLoad(t)

	require.Equal(t, "eu-west-1", os.Getenv("AWS_DEFAULT_REGION"), "the LOCALSTACK_ value replaces the bare one")
	require.Equal(t, "container", os.Getenv("HOSTNAME"), "LOCALSTACK_HOSTNAME is excluded")
}

func Test_Config_Set(t *testing.T) {
	t.Run("a reader holding the config sees the update", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)
		reader := func() string { return cfg.Settings().SQSEndpointStrategy }
		require.Equal(t, "standard", reader())

		require.NoError(t, cfg.Set(varSQSEndpointStrategy, strategyPath))
		require.Equal(t, strategyPath, reader())
		require.Equal(t, strategyPath, cfg.Value(varSQSEndpointStrategy))
	})

	t.Run("an update wins over every environment form", func(t *testing.T) {
		isolate(t)
		t.Setenv("GOSTACK_SQS_ENDPOINT_STRATEGY", "off")
		cfg := mustLoad(t)

		require.NoError(t, cfg.Set(varSQSEndpointStrategy, strategyDomain))
		require.Equal(t, strategyDomain, cfg.Settings().SQSEndpointStrategy)
	})

	t.Run("the parity variables are accepted and applied live", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)

		require.NoError(t, cfg.Set(varSQSDelayRecentlyDeleted, true))
		require.NoError(t, cfg.Set(varIncludeStackTraces, "1"))
		require.NoError(t, cfg.Set(varShutdownTimeout, float64(9)))

		got := cfg.Settings()
		require.True(t, got.SQSDelayRecentlyDeleted)
		require.True(t, got.IncludeStackTracesInHTTPResponse)
		require.Equal(t, 9*time.Second, got.ShutdownTimeout)
		require.Equal(t, true, cfg.Value(varSQSDelayRecentlyDeleted))
	})

	t.Run("an unknown variable is stored and reported", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)

		require.NoError(t, cfg.Set("SOME_NEW_THING", "x"))
		require.Equal(t, "x", cfg.Value("SOME_NEW_THING"))
		require.Equal(t, "x", cfg.Values()["SOME_NEW_THING"])
		require.Nil(t, cfg.Value("NOT_SET_ANYWHERE"))
	})

	t.Run("a value reads back as it was set", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)

		require.NoError(t, cfg.Set(varGatewayListen, "127.0.0.1:4570"))
		require.NoError(t, cfg.Set(varDebug, true))
		require.NoError(t, cfg.Set(varShutdownTimeout, float64(9)))

		require.Equal(t, "127.0.0.1:4570", cfg.Value(varGatewayListen), "a string stays a string, not the []string of GET")
		require.Equal(t, true, cfg.Value(varDebug), "a bool stays a bool")
		require.InDelta(t, 9, cfg.Value(varShutdownTimeout), 0, "a number stays a number")
		require.Equal(t, []HostPort{{Host: "127.0.0.1", Port: 4570}}, cfg.Settings().GatewayListen)
		require.Equal(t, "127.0.0.1:4570", cfg.Values()[varGatewayListen])
	})

	t.Run("a nil value changes nothing", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)

		require.NoError(t, cfg.Set(varSQSEndpointStrategy, nil))
		require.Equal(t, "standard", cfg.Settings().SQSEndpointStrategy)
	})

	t.Run("an invalid name is refused", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)

		for _, key := range []string{"", "A-B", "a.b", "a b"} {
			require.ErrorIs(t, cfg.Set(key, "x"), ErrInvalidKey)
		}
	})

	t.Run("an unparsable value is refused and the old settings stay", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)
		require.NoError(t, cfg.Set(varSQSEndpointStrategy, strategyPath))

		require.Error(t, cfg.Set(varShutdownTimeout, "soon"))
		require.Error(t, cfg.Set(varGatewayListen, "host:99999"))
		require.Equal(t, 5*time.Second, cfg.Settings().ShutdownTimeout)
		require.Equal(t, strategyPath, cfg.Settings().SQSEndpointStrategy)
		require.Nil(t, cfg.Value("SOME_UNKNOWN"))
	})

	t.Run("concurrent readers and writers do not race", func(t *testing.T) {
		isolate(t)
		cfg := mustLoad(t)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 50 {
					_ = cfg.Set(varSQSEndpointStrategy, strategyPath)
					_ = cfg.Settings().SQSEndpointStrategy
					_ = cfg.Values()
				}
			})
		}
		wg.Wait()
		require.Equal(t, strategyPath, cfg.Settings().SQSEndpointStrategy)
	})
}

func Test_Config_Values(t *testing.T) {
	isolate(t)
	cfg := mustLoad(t)
	got := cfg.Values()

	require.Equal(t, []string{"127.0.0.1:4566"}, got[varGatewayListen])
	require.Equal(t, "standard", got[varSQSEndpointStrategy])
	require.Equal(t, 5, got[varShutdownTimeout])
	require.Equal(t, false, got[varDebug])
}
