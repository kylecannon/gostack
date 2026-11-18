package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const loopback6 = "::1"

func Test_ParseHostPort(t *testing.T) {
	tests := []struct {
		name     string
		give     string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{name: "host and port", give: "0.0.0.0:4566", wantHost: dockerListenHost, wantPort: 4566},
		{name: "host only", give: "10.1.2.3", wantHost: "10.1.2.3", wantPort: 4566},
		{name: "port only", give: ":4510", wantHost: "def", wantPort: 4510},
		{name: "empty", give: "", wantHost: "def", wantPort: 4566},
		{name: "ipv6 and port", give: "[::]:4567", wantHost: "::", wantPort: 4567},
		{name: "ipv6 only", give: "[::1]", wantHost: loopback6, wantPort: 4566},
		{name: "spaces around host", give: " localhost :80", wantHost: "localhost", wantPort: 80},
		{name: "port zero", give: "127.0.0.1:0", wantHost: hostListenHost, wantPort: 0},
		{name: "port too large", give: "host:65536", wantErr: true},
		{name: "port not a number", give: "host:abc", wantErr: true},
		{name: "negative port", give: "host:-1", wantErr: true},
		{name: "ipv6 without closing bracket", give: "[::1", wantErr: true},
		{name: "ipv4 in brackets", give: "[127.0.0.1]:80", wantErr: true},
		{name: "text after brackets", give: "[::1]x", wantErr: true},
		{name: "ipv6 bad port", give: "[::1]:x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHostPort(tt.give, "def", DefaultPort)
			if tt.wantErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, HostPort{Host: tt.wantHost, Port: tt.wantPort}, got)
		})
	}
}

func Test_HostPort(t *testing.T) {
	t.Run("String brackets IPv6", func(t *testing.T) {
		require.Equal(t, "[::1]:4566", HostPort{Host: loopback6, Port: 4566}.String())
		require.Equal(t, "localhost:80", HostPort{Host: "localhost", Port: 80}.String())
	})
}

func Test_uniqueHostPorts(t *testing.T) {
	tests := []struct {
		name string
		give []HostPort
		want []HostPort
	}{
		{
			name: "single binding is kept",
			give: []HostPort{{Host: hostListenHost, Port: 4566}},
			want: []HostPort{{Host: hostListenHost, Port: 4566}},
		},
		{
			name: "ipv6 wildcard covers everything on its port",
			give: []HostPort{{Host: hostListenHost, Port: 4566}, {Host: loopback6, Port: 4566}, {Host: "::", Port: 4566}},
			want: []HostPort{{Host: "::", Port: 4566}},
		},
		{
			name: "ipv4 wildcard covers other ipv4 hosts",
			give: []HostPort{{Host: hostListenHost, Port: 4566}, {Host: dockerListenHost, Port: 4566}},
			want: []HostPort{{Host: dockerListenHost, Port: 4566}},
		},
		{
			name: "identical bindings collapse and ports stay apart",
			give: []HostPort{{Host: "a", Port: 1}, {Host: "b", Port: 2}, {Host: "a", Port: 1}, {Host: "c", Port: 1}},
			want: []HostPort{{Host: "a", Port: 1}, {Host: "c", Port: 1}, {Host: "b", Port: 2}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, uniqueHostPorts(tt.give))
		})
	}
}
