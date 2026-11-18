package main_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envListenAnyPort has gostack take a free loopback port, never 4566.
const envListenAnyPort = "GATEWAY_LISTEN=127.0.0.1:0"

// Test_gostack_badCertificateNeverPrintsReady: a CUSTOM_SSL_CERT_PATH that cannot
// be used stops gostack before the ready hooks run, so "Ready." is never printed
// and a container waiting for it sees the process fail instead.
func Test_gostack_badCertificateNeverPrintsReady(t *testing.T) {
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	require.NoError(t, os.WriteFile(garbage, []byte("not a certificate"), 0o600))

	tests := []struct {
		name     string
		givePath string
	}{
		{name: "a file that is not there", givePath: filepath.Join(t.TempDir(), "absent.pem")},
		{name: "a file that is not a certificate", givePath: garbage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr syncBuffer

			cmd := exec.CommandContext(t.Context(), binary)
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + t.TempDir(),
				envListenAnyPort,
				"CUSTOM_SSL_CERT_PATH=" + tt.givePath,
			}
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()

			exitErr, ok := errors.AsType[*exec.ExitError](err)
			require.True(t, ok, "gostack must exit non-zero, got %v", err)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.NotContains(t, stdout.String(), "Ready.")
			assert.Contains(t, stderr.String(), "tls certificate")
		})
	}
}
