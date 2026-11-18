package main_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cmdStatus   = "status"
	cmdServices = "services"
	// jsonArg asks for the JSON output.
	jsonArg = "--format=json"
)

// statusBinary runs the built binary with a clean environment and GATEWAY_LISTEN
// set to listen, and returns what it printed and how it exited.
func statusBinary(t *testing.T, path, listen string, args ...string) (string, string, int) {
	t.Helper()

	var stdout, stderr syncBuffer

	cmd := exec.CommandContext(t.Context(), path, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GATEWAY_LISTEN=" + listen,
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}

	require.NoError(t, err)

	return stdout.String(), stderr.String(), 0
}

// localstackLink makes a symlink named localstack to the built binary, the way
// the images do.
func localstackLink(t *testing.T) string {
	t.Helper()

	link := filepath.Join(t.TempDir(), "localstack")
	require.NoError(t, os.Symlink(binary, link))

	return link
}

// Test_status_againstGostack is the image's healthcheck against a real server:
// the status command asks /_gostack/health, which is mounted whether or not the
// LocalStack compatibility paths are, and prints the same states either way.
func Test_status_againstGostack(t *testing.T) {
	tests := []struct {
		name           string
		giveCompat     string
		wantLocalstack int
	}{
		{name: "compat paths on", giveCompat: "1", wantLocalstack: http.StatusOK},
		{name: "compat paths off", giveCompat: "0", wantLocalstack: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := startGostack(t, "GOSTACK_COMPAT_PATHS="+tt.giveCompat)

			status, _ := g.do(t, http.MethodGet, "/_localstack/health", "")
			require.Equal(t, tt.wantLocalstack, status, "the compat path is %s", tt.name)

			for _, name := range []string{binary, localstackLink(t)} {
				stdout, stderr, code := statusBinary(t, name, strings.TrimPrefix(g.base, "http://"),
					cmdStatus, cmdServices, jsonArg)

				assert.Zero(t, code, stderr)
				assert.JSONEq(t, `{"sqs":"running","sts":"running"}`, stdout)
			}
		})
	}
}

func Test_status_binary(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_gostack/health" {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(`{"status":"ok","version":"1","services":{` +
			`"kms":{"state":"running","persisted":false},"sns":{"state":"available","persisted":false}}}`))
	}))
	t.Cleanup(healthy.Close)

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unhealthy.Close)

	gone := httptest.NewServer(http.NotFoundHandler())
	goneListen := gone.Listener.Addr().String()
	gone.Close()

	listenOf := func(srv *httptest.Server) string { return srv.Listener.Addr().String() }

	tests := []struct {
		name       string
		giveBinary string
		giveListen string
		giveArgs   []string
		wantCode   int
		wantJSON   string
		wantStderr string
	}{
		{
			name: "gostack status services as json", giveBinary: binary, giveListen: listenOf(healthy),
			giveArgs: []string{cmdStatus, cmdServices, jsonArg},
			wantJSON: `{"kms":"running","sns":"available"}`,
		},
		{
			name: "localstack status services as json", giveBinary: localstackLink(t), giveListen: listenOf(healthy),
			giveArgs: []string{cmdStatus, cmdServices, jsonArg},
			wantJSON: `{"kms":"running","sns":"available"}`,
		},
		{
			name: "a non-200 exits non-zero", giveBinary: binary, giveListen: listenOf(unhealthy),
			giveArgs: []string{cmdStatus, cmdServices, jsonArg},
			wantCode: 1, wantStderr: "503",
		},
		{
			name: "no listener exits non-zero", giveBinary: binary, giveListen: goneListen,
			giveArgs: []string{cmdStatus, cmdServices, jsonArg},
			wantCode: 1, wantStderr: "could not connect",
		},
		{
			name: "a bad format is a usage error", giveBinary: binary, giveListen: listenOf(healthy),
			giveArgs: []string{cmdStatus, cmdServices, "--format=yaml"},
			wantCode: 2, wantStderr: "usage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := statusBinary(t, tt.giveBinary, tt.giveListen, tt.giveArgs...)

			assert.Equal(t, tt.wantCode, code, stderr)
			assert.Contains(t, stderr, tt.wantStderr)

			if tt.wantJSON != "" {
				assert.JSONEq(t, tt.wantJSON, stdout)
			} else {
				assert.Empty(t, stdout)
			}
		})
	}
}
