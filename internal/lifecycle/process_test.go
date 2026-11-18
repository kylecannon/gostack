package lifecycle_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func Test_Process(t *testing.T) {
	t.Run("Ready prints the marker after the ready hooks finish", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("ready.d/hook.sh", "#!/bin/sh\necho from-hook\n", 0o755)
		var out bytes.Buffer
		hooks := lifecycle.NewHooks(h.root)
		hooks.Stdout = &out
		process := &lifecycle.Process{Hooks: hooks, Out: &out}
		require.False(t, process.IsReady())

		process.Ready(t.Context())

		require.Equal(t, "from-hook\nReady.\n", out.String())
		require.True(t, process.IsReady())
		require.True(t, hooks.Completed(lifecycle.StageReady))
	})

	t.Run("Ready prints the marker even when a hook fails", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("ready.d/fail.sh", "#!/bin/sh\nexit 1\n", 0o755)
		var out bytes.Buffer
		process := &lifecycle.Process{Hooks: lifecycle.NewHooks(h.root), Out: &out}

		process.Ready(t.Context())

		require.Equal(t, "Ready.\n", out.String())
		require.True(t, process.IsReady())
	})

	t.Run("the stages run their own hooks in order", func(t *testing.T) {
		h := newHookRoot(t)
		for _, stage := range []string{"boot", "start", "ready", stageShutdown} {
			h.write(stage+".d/"+stage+".sh", appendBody, 0o755)
		}
		var out bytes.Buffer
		process := &lifecycle.Process{Hooks: lifecycle.NewHooks(h.root), Out: &out}

		process.Boot(t.Context())
		process.Start(t.Context())
		process.Ready(t.Context())
		require.True(t, process.IsReady())
		process.Shutdown(t.Context())

		require.False(t, process.IsReady(), "a shutting down process is not ready")
		require.Equal(t, []string{"boot.sh", "start.sh", "ready.sh", "shutdown.sh"}, h.ran())
	})

	t.Run("the zero value has no hooks", func(t *testing.T) {
		var process lifecycle.Process
		process.Out = &bytes.Buffer{}

		process.Boot(t.Context())
		process.Start(t.Context())
		process.Ready(t.Context())
		process.Shutdown(t.Context())

		require.False(t, process.IsReady())
	})
}

func Test_WriteStartupWarnings(t *testing.T) {
	tests := []struct {
		name          string
		give          map[string]string
		wantCommunity bool
		wantWrongPro  bool
	}{
		{name: "no token", give: map[string]string{}, wantCommunity: true},
		{
			name: "api key without token", give: map[string]string{"LOCALSTACK_API_KEY": "k"},
			wantCommunity: true, wantWrongPro: true,
		},
		{name: "auth token", give: map[string]string{"LOCALSTACK_AUTH_TOKEN": "t"}, wantWrongPro: true},
		{name: "both", give: map[string]string{"LOCALSTACK_AUTH_TOKEN": "t", "LOCALSTACK_API_KEY": "k"}, wantWrongPro: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			lifecycle.WriteStartupWarnings(&out, func(name string) string { return tt.give[name] })

			require.Equal(t, tt.wantCommunity, strings.Contains(out.String(), "LocalStack Community Docker image"))
			require.Equal(t, tt.wantWrongPro, strings.Contains(out.String(), "localstack/localstack-pro"))
			if out.Len() > 0 {
				require.True(t, strings.HasPrefix(out.String(), "WARNING\n===="))
				require.True(t, strings.HasSuffix(out.String(), "====\n\n"))
			}
		})
	}
}
