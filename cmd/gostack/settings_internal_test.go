package main

import (
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_s3Settings(t *testing.T) {
	t.Setenv("GATEWAY_LISTEN", "127.0.0.1:4570,127.0.0.1:4571")
	t.Setenv("S3_SKIP_SIGNATURE_VALIDATION", "1")

	cfg, err := config.Load()
	require.NoError(t, err)

	settings := liveSettings(cfg, s3Settings)
	got := settings()

	assert.False(t, got.ValidateSignatures)
	assert.False(t, got.ValidateKMSKeys, "KMS keys are not checked by default")
	assert.Equal(t, 4570, got.GatewayPort)

	t.Run("the config bridge's false turns validation on", func(t *testing.T) {
		require.NoError(t, cfg.Set("S3_SKIP_SIGNATURE_VALIDATION", false))
		require.NoError(t, cfg.Set("S3_SKIP_KMS_KEY_VALIDATION", false))

		assert.True(t, settings().ValidateSignatures)
		assert.True(t, settings().ValidateKMSKeys)
	})
}

func Test_snsSettings(t *testing.T) {
	t.Setenv("LOCALSTACK_HOST", "stack.test:4570")

	cfg, err := config.Load()
	require.NoError(t, err)

	settings := liveSettings(cfg, snsSettings)

	assert.Empty(t, settings().CertURLHost)
	assert.Equal(t, "stack.test:4570", settings().Host)

	t.Run("an update through the config endpoint reaches the next call", func(t *testing.T) {
		require.NoError(t, cfg.Set("SNS_CERT_URL_HOST", "sns.us-east-1.amazonaws.com"))

		assert.Equal(t, "sns.us-east-1.amazonaws.com", settings().CertURLHost)

		require.NoError(t, cfg.Set("SNS_CERT_URL_HOST", ""))

		assert.Empty(t, settings().CertURLHost, "the test's empty value puts LOCALSTACK_HOST back")
	})

	// envelope.BaseURL does not trim its host, so the settings must: as in
	// LocalStack, a value with spaces around it names the host without them.
	t.Run("the host is trimmed, from the environment and from an update", func(t *testing.T) {
		require.NoError(t, cfg.Set("SNS_CERT_URL_HOST", "  sns.example.com \t"))

		assert.Equal(t, "sns.example.com", settings().CertURLHost)

		t.Setenv("SNS_CERT_URL_HOST", " env.example.com ")

		fromEnv, err := config.Load()
		require.NoError(t, err)

		assert.Equal(t, "env.example.com", snsSettings(fromEnv.Settings()).CertURLHost)

		require.NoError(t, cfg.Set("SNS_CERT_URL_HOST", "   "))

		assert.Empty(t, settings().CertURLHost, "a blank value is no host")
	})
}

func Test_liveSettings_mapsEachSnapshotOnce(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	var mapped int

	settings := liveSettings(cfg, func(s *config.Settings) bool {
		mapped++

		return s.FailFast
	})

	assert.False(t, settings())
	assert.False(t, settings())
	assert.Equal(t, 1, mapped, "an unchanged snapshot is mapped once")

	require.NoError(t, cfg.Set("FAIL_FAST", true))

	assert.True(t, settings())
	assert.Equal(t, 2, mapped, "an update is mapped again")
}

func Test_settingsFunc(t *testing.T) {
	t.Setenv("GATEWAY_LISTEN", "127.0.0.1:4570,127.0.0.1:4571")
	t.Setenv("PARITY_AWS_ACCESS_KEY_ID", "1")
	t.Setenv("DISABLE_PREFLIGHT_PROCESSING", "1")
	t.Setenv("EXTRA_CORS_ALLOWED_ORIGINS", "https://a.example,https://b.example")
	t.Setenv("GOSTACK_LOCALSTACK_HOST", "stack.test:4570")
	t.Setenv("DISABLE_CUSTOM_CORS_S3", "1")

	cfg, err := config.Load()
	require.NoError(t, err)

	settings := settingsFunc(cfg)
	got := settings()

	assert.Equal(t, []string{"127.0.0.1:4570", "127.0.0.1:4571"}, got.Listen)
	assert.True(t, got.ParityAWSAccessKeyID)
	assert.True(t, got.DisablePreflightProcessing)
	assert.Equal(t, "https://a.example,https://b.example", got.ExtraCORSAllowedOrigins)
	assert.Equal(t, "stack.test", got.LocalstackHost)
	assert.True(t, got.ResponseHeaderEnabled)
	assert.Equal(t, []string{"s3"}, got.DisableCustomCORS)

	t.Run("an update through the config shows from the next call on", func(t *testing.T) {
		require.NoError(t, cfg.Set("FAIL_FAST", true))

		assert.True(t, settings().FailFast)
		assert.Equal(t, got.Listen, settings().Listen)
	})
}
