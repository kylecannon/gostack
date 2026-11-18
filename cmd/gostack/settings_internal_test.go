package main

import (
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
