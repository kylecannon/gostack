package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonArg asks for the JSON output.
const jsonArg = "--format=json"

// statusArgs is the command line "status services" followed by extra.
func statusArgs(extra ...string) []string {
	return append([]string{statusCommandName, statusServicesName}, extra...)
}

func Test_parseStatus(t *testing.T) {
	tests := []struct {
		name       string
		give       []string
		wantFormat string
		wantStatus bool
		wantErr    bool
	}{
		{name: "no arguments is not a status command", give: nil},
		{name: "the server flag is not a status command", give: []string{"-version"}},
		{name: "another word is not a status command", give: []string{"start"}},
		{name: "status services with json", give: statusArgs(jsonArg), wantFormat: formatJSON, wantStatus: true},
		{
			name: "the format may be a separate argument", give: statusArgs("--format", "json"),
			wantFormat: formatJSON, wantStatus: true,
		},
		{name: "one dash works too", give: statusArgs("-format=plain"), wantFormat: formatPlain, wantStatus: true},
		{
			name: "LocalStack's default format is accepted", give: statusArgs("--format=table"),
			wantFormat: formatTable, wantStatus: true,
		},
		{name: "status services alone is plain", give: statusArgs(), wantFormat: formatPlain, wantStatus: true},
		{
			name: "status alone means status services", give: []string{statusCommandName},
			wantFormat: formatPlain, wantStatus: true,
		},
		{
			name: "status with a format means status services", give: []string{statusCommandName, jsonArg},
			wantFormat: formatJSON, wantStatus: true,
		},
		{name: "an unknown format fails", give: statusArgs("--format=yaml"), wantStatus: true, wantErr: true},
		{name: "an unknown flag fails", give: statusArgs("--verbose"), wantStatus: true, wantErr: true},
		{name: "a missing format value fails", give: statusArgs("--format"), wantStatus: true, wantErr: true},
		{name: "an extra word fails", give: statusArgs("docker"), wantStatus: true, wantErr: true},
		{
			name: "an unknown subcommand fails", give: []string{statusCommandName, "docker"},
			wantStatus: true, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			format, isStatus, err := parseStatus(tt.give)

			assert.Equal(t, tt.wantStatus, isStatus)
			assert.Equal(t, tt.wantFormat, format)

			if tt.wantErr {
				require.ErrorIs(t, err, errStatusUsage)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// healthServer answers GET /_gostack/health with the given status and body.
func healthServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			http.NotFound(w, r)

			return
		}

		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

const healthBody = `{"status":"ok","version":"1","services":{` +
	`"kms":{"state":"running","persisted":false},"sns":{"state":"available","persisted":true}}}`

func Test_runStatus(t *testing.T) {
	tests := []struct {
		name       string
		giveStatus int
		giveBody   string
		giveFormat string
		wantCode   int
		wantStdout map[string]any
		wantLines  []string
		wantStderr string
	}{
		{
			name: "json prints the services map", giveStatus: http.StatusOK, giveBody: healthBody, giveFormat: formatJSON,
			wantStdout: map[string]any{"kms": stateRunning, "sns": "available"},
		},
		{
			name: "json with no services prints an empty object", giveStatus: http.StatusOK,
			giveBody: `{"edition":"community"}`, giveFormat: formatJSON, wantStdout: map[string]any{},
		},
		{
			name: "plain prints a sorted table", giveStatus: http.StatusOK, giveBody: healthBody, giveFormat: formatPlain,
			wantLines: []string{"SERVICE  STATE", "kms      running", "sns      available"},
		},
		{
			name: "table prints the same table", giveStatus: http.StatusOK, giveBody: healthBody, giveFormat: formatTable,
			wantLines: []string{"SERVICE  STATE", "kms      running", "sns      available"},
		},
		{
			name: "a non-200 fails", giveStatus: http.StatusServiceUnavailable, giveBody: `{}`, giveFormat: formatJSON,
			wantCode: exitFailure, wantStderr: "503",
		},
		{
			name: "a body that is not JSON fails", giveStatus: http.StatusOK, giveBody: `not json`, giveFormat: formatJSON,
			wantCode: exitFailure, wantStderr: "health document",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := healthServer(t, tt.giveStatus, tt.giveBody)

			var stdout, stderr bytes.Buffer

			code := runStatus(t.Context(), srv.URL, tt.giveFormat, &stdout, &stderr)

			assert.Equal(t, tt.wantCode, code)
			assert.Contains(t, stderr.String(), tt.wantStderr)

			switch {
			case tt.wantStdout != nil:
				var got map[string]any

				require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
				assert.Equal(t, tt.wantStdout, got)
			case tt.wantLines != nil:
				assert.Equal(t, tt.wantLines, strings.Split(strings.TrimSpace(stdout.String()), "\n"))
			default:
				assert.Empty(t, stdout.String())
			}
		})
	}

	t.Run("a closed port fails and says so on stderr", func(t *testing.T) {
		srv := healthServer(t, http.StatusOK, healthBody)
		srv.Close()

		var stdout, stderr bytes.Buffer

		code := runStatus(t.Context(), srv.URL, formatJSON, &stdout, &stderr)

		assert.Equal(t, exitFailure, code)
		assert.Empty(t, stdout.String())
		assert.Contains(t, stderr.String(), "could not connect")
	})
}

func Test_statusMain(t *testing.T) {
	t.Run("a command line that is not status is left alone", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		code, isStatus := statusMain(t.Context(), []string{"-version"}, &stdout, &stderr)

		assert.False(t, isStatus)
		assert.Zero(t, code)
		assert.Empty(t, stdout.String())
		assert.Empty(t, stderr.String())
	})

	t.Run("a usage error exits 2", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		code, isStatus := statusMain(t.Context(), statusArgs("--format=yaml"), &stdout, &stderr)

		assert.True(t, isStatus)
		assert.Equal(t, exitUsage, code)
		assert.Contains(t, stderr.String(), "usage")
	})

	t.Run("the port comes from the first GATEWAY_LISTEN entry", func(t *testing.T) {
		srv := healthServer(t, http.StatusOK, healthBody)
		_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
		require.NoError(t, err)

		t.Setenv("GATEWAY_LISTEN", "0.0.0.0:"+port+",127.0.0.1:1")

		var stdout, stderr bytes.Buffer

		code, isStatus := statusMain(t.Context(), statusArgs(jsonArg), &stdout, &stderr)

		assert.True(t, isStatus)
		assert.Equal(t, 0, code, stderr.String())
		assert.JSONEq(t, `{"kms":"running","sns":"available"}`, stdout.String())
	})

	t.Run("the default port is 4566 when GATEWAY_LISTEN is unset", func(t *testing.T) {
		t.Setenv("GATEWAY_LISTEN", "")
		t.Setenv("LOCALSTACK_GATEWAY_LISTEN", "")

		// Nothing is asked to listen on 4566 here, so the request fails and the
		// error names the port the command picked.
		var stdout, stderr bytes.Buffer

		code, _ := statusMain(t.Context(), []string{statusCommandName}, &stdout, &stderr)

		if code == 0 {
			t.Skip("something on this machine answers on 127.0.0.1:4566")
		}

		assert.Contains(t, stderr.String(), "127.0.0.1:4566")
	})
}
