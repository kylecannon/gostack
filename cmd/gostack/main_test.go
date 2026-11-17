package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const anyPort = "0.0.0.0:4566"

func Test_listenAddresses(t *testing.T) {
	defaultHost := "127.0.0.1"
	if inContainer() {
		defaultHost = "0.0.0.0"
	}

	tests := []struct {
		name string
		give string
		want []string
	}{
		{name: "unset", give: "", want: []string{defaultHost + ":4566"}},
		{name: "host and port", give: anyPort, want: []string{anyPort}},
		{name: "port only", give: ":8080", want: []string{defaultHost + ":8080"}},
		{name: "host only", give: "localhost", want: []string{"localhost:4566"}},
		{
			name: "list with blanks",
			give: " 0.0.0.0:4566, ,0.0.0.0:443 ",
			want: []string{anyPort, "0.0.0.0:443"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, listenAddresses(tt.give))
		})
	}
}

func Test_loadSettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got := loadSettings()

		assert.True(t, got.ResponseHeaderEnabled, "LOCALSTACK_RESPONSE_HEADER_ENABLED is on unless set false")
		assert.False(t, got.FailFast)
		assert.False(t, got.ParityAWSAccessKeyID)
		assert.False(t, got.DisableCORSChecks)
		assert.Empty(t, got.CustomSSLCertPath)
		assert.Len(t, got.Listen, 1)
	})

	t.Run("set", func(t *testing.T) {
		t.Setenv("GATEWAY_LISTEN", "127.0.0.1:4570")
		t.Setenv("FAIL_FAST", "1")
		t.Setenv("LOCALSTACK_RESPONSE_HEADER_ENABLED", "false")
		t.Setenv("PARITY_AWS_ACCESS_KEY_ID", "True")
		t.Setenv("DISABLE_CORS_CHECKS", " 1 ")
		t.Setenv("DISABLE_CORS_HEADERS", "yes")
		t.Setenv("CUSTOM_SSL_CERT_PATH", " /certs/server.pem ")
		t.Setenv("EXTRA_CORS_ALLOWED_ORIGINS", "https://a.example")

		got := loadSettings()

		assert.Equal(t, []string{"127.0.0.1:4570"}, got.Listen)
		assert.True(t, got.FailFast)
		assert.False(t, got.ResponseHeaderEnabled)
		assert.True(t, got.ParityAWSAccessKeyID)
		assert.True(t, got.DisableCORSChecks)
		assert.False(t, got.DisableCORSHeaders, "only 1 and true count")
		assert.Equal(t, "/certs/server.pem", got.CustomSSLCertPath)
		assert.Equal(t, "https://a.example", got.ExtraCORSAllowedOrigins)
	})
}

func Test_registrations(t *testing.T) {
	assert.Empty(t, registrations())
}

func Test_serve(t *testing.T) {
	g, err := gateway.New(loadSettings, registrations(), mountInternal, nil)
	require.NoError(t, err)

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	stdout := &bytes.Buffer{}
	done := make(chan error, 1)

	go func() { done <- serve(ctx, g, []net.Listener{listener}, stdout) }()

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no service is registered yet")
	assert.JSONEq(t, `{"message": "Not Found"}`, string(body))
	assert.Equal(t, "Ready.\n", stdout.String())

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
}
