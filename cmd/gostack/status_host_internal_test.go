package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_healthAddress(t *testing.T) {
	const loopback = "127.0.0.1:4566"

	tests := []struct {
		name       string
		giveHost   string
		wantTarget string
	}{
		{name: "an empty host is the IPv4 loopback", giveHost: "", wantTarget: loopback},
		{name: "0.0.0.0 is the IPv4 loopback", giveHost: "0.0.0.0", wantTarget: loopback},
		{name: ":: is the IPv4 loopback", giveHost: "::", wantTarget: loopback},
		{name: "127.0.0.1 stays", giveHost: "127.0.0.1", wantTarget: loopback},
		{name: "the IPv6 loopback is bracketed", giveHost: "::1", wantTarget: "[::1]:4566"},
		{name: "a documentation address stays", giveHost: "192.0.2.7", wantTarget: "192.0.2.7:4566"},
		{name: "a host name stays", giveHost: "gostack.test", wantTarget: "gostack.test:4566"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := healthAddress(config.HostPort{Host: tt.giveHost, Port: 4566})

			assert.Equal(t, tt.wantTarget, got)
		})
	}
}

// Test_statusMain_dialsTheListenHost is the healthcheck of a gateway that
// listens on ::1 only: it has to ask ::1, where 127.0.0.1 has nothing.
func Test_statusMain_dialsTheListenHost(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "[::1]:0")
	if err != nil {
		t.Skipf("this machine has no IPv6 loopback: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(healthBody))
	}))
	require.NoError(t, srv.Listener.Close())

	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	t.Setenv("GATEWAY_LISTEN", "[::1]:"+port)

	var stdout, stderr bytes.Buffer

	code, isStatus := statusMain(t.Context(), statusArgs(jsonArg), &stdout, &stderr)

	assert.True(t, isStatus)
	assert.Equal(t, 0, code, stderr.String())
	assert.JSONEq(t, `{"kms":"running","sns":"available"}`, stdout.String())
}
