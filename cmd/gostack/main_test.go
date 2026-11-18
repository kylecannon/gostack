package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	smokeVersion  = "smoke"
	exitDeadline  = 5 * time.Second
	startDeadline = 20 * time.Second
	poll          = 20 * time.Millisecond
)

// listeningAddress picks the bound address out of the "gostack listening" log
// line, so the test can ask for port 0 and learn which port it got.
var listeningAddress = regexp.MustCompile(`addresses=\[([^\]\s]+)`)

// syncBuffer collects a process's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// gostack is a running binary.
type gostack struct {
	cmd    *exec.Cmd
	base   string
	stdout *syncBuffer
	stderr *syncBuffer
	exited chan error
}

// binary is the path of the executable TestMain built.
var binary string

// TestMain builds the binary once, the way make build does, with a version to
// look for.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gostack-smoke-")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	binary = filepath.Join(dir, "gostack")

	cmd := exec.CommandContext(
		context.Background(), "go", "build", "-ldflags", "-X main.version="+smokeVersion, "-o", binary, ".",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		panic("go build: " + err.Error() + "\n" + string(out))
	}

	m.Run()
}

// startGostack runs the binary on a free port, with a clean environment so that
// the developer's own PERSIST_* and LOCALSTACK_* variables stay out of it. env
// adds variables, such as the PERSIST_* settings of a test.
func startGostack(t *testing.T, env ...string) *gostack {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		envListenAnyPort,
		"ENABLE_CONFIG_UPDATES=1",
	}, env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGKILL) }

	g := &gostack{cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}, exited: make(chan error, 1)}
	stderr := g.stderr
	cmd.Stdout = g.stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())

	go func() { g.exited <- cmd.Wait() }()

	t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGKILL) })

	require.Eventually(t, func() bool {
		return listeningAddress.MatchString(stderr.String())
	}, startDeadline, poll, "gostack never logged its address:\n%s", stderr.String())

	g.base = "http://" + listeningAddress.FindStringSubmatch(stderr.String())[1]

	return g
}

func (g *gostack) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, g.base+path, strings.NewReader(body))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(data)
}

func Test_gostack_smoke(t *testing.T) {
	g := startGostack(t)

	require.Eventually(t, func() bool {
		return strings.Contains(g.stdout.String(), "Ready.\n")
	}, startDeadline, poll, "Ready. never appeared on stdout:\n%s", g.stdout.String())

	t.Run("the gostack health route answers", func(t *testing.T) {
		status, body := g.do(t, http.MethodGet, "/_gostack/health", "")

		assert.Equal(t, http.StatusOK, status)
		assert.JSONEq(t, `{"status":"ok","version":"`+smokeVersion+
			`","services":{"sqs":{"state":"running","persisted":false},`+
			`"sts":{"state":"running","persisted":false}}}`, body)
	})

	t.Run("the LocalStack health route has LocalStack's shape", func(t *testing.T) {
		status, body := g.do(t, http.MethodGet, "/_localstack/health", "")

		assert.Equal(t, http.StatusOK, status)
		assert.JSONEq(t, `{"services":{"sqs":"running","sts":"running"},"edition":"community",`+
			`"version":"`+smokeVersion+`"}`, body)
	})

	t.Run("the process reports ready once Ready. is printed", func(t *testing.T) {
		status, body := g.do(t, http.MethodGet, "/_gostack/ready", "")

		assert.Equal(t, http.StatusOK, status)
		assert.JSONEq(t, `{"status":"ready"}`, body)
	})

	t.Run("a config update reads back as it was posted", func(t *testing.T) {
		tests := []struct {
			name string
			give string
			want any
		}{
			{
				name: "a string stays a string",
				give: `{"variable":"SQS_ENDPOINT_STRATEGY","value":"path"}`,
				want: "path",
			},
			{
				name: "a bool stays a bool",
				give: `{"variable":"SQS_DELAY_RECENTLY_DELETED","value":true}`,
				want: true,
			},
			{
				name: "a number stays a number",
				give: `{"variable":"SHUTDOWN_TIMEOUT","value":3}`,
				want: float64(3),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				status, body := g.do(t, http.MethodPost, "/_localstack/config", tt.give)
				require.Equal(t, http.StatusOK, status, body)

				var got map[string]any
				require.NoError(t, json.Unmarshal([]byte(body), &got))
				assert.Equal(t, tt.want, got["value"])
			})
		}
	})

	t.Run("a bad config update is a 400", func(t *testing.T) {
		status, _ := g.do(t, http.MethodPost, "/_localstack/config", `{"variable":"A-B","value":1}`)

		assert.Equal(t, http.StatusBadRequest, status)
	})

	t.Run("status services reports the running server, as gostack and as localstack", func(t *testing.T) {
		listen := strings.TrimPrefix(g.base, "http://")

		for _, bin := range []string{binary, localstackLink(t)} {
			stdout, stderr, code := statusBinary(t, bin, listen, cmdStatus, cmdServices, jsonArg)

			require.Equal(t, 0, code, stderr)

			var services map[string]any

			require.NoError(t, json.Unmarshal([]byte(stdout), &services), stdout)
		}
	})

	t.Run("SIGTERM ends the process cleanly", func(t *testing.T) {
		require.NoError(t, g.cmd.Process.Signal(syscall.SIGTERM))

		select {
		case err := <-g.exited:
			require.NoError(t, err, "exit code 0 expected")
		case <-time.After(exitDeadline):
			t.Fatal("gostack did not exit within 5s of SIGTERM")
		}
	})
}

func Test_gostack_kill(t *testing.T) {
	g := startGostack(t)

	status, body := g.do(t, http.MethodPost, "/_gostack/health", `{"action":"kill"}`)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", body, "the answer is sent before the process stops")

	select {
	case err := <-g.exited:
		require.NoError(t, err, "exit code 0 expected")
	case <-time.After(exitDeadline):
		t.Fatal("gostack did not exit within 5s of the kill action")
	}
}

func Test_gostack_restartWithoutSupervisor(t *testing.T) {
	g := startGostack(t)

	status, body := g.do(t, http.MethodPost, "/_gostack/health", `{"action":"restart"}`)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", body)

	status, _ = g.do(t, http.MethodGet, "/_gostack/ready", "")
	assert.Equal(t, http.StatusOK, status, "with no supervisor the restart action changes nothing")

	require.NoError(t, g.cmd.Process.Signal(syscall.SIGTERM))
	require.NoError(t, <-g.exited)
}

func Test_gostack_version(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), binary, "-version").Output()
	require.NoError(t, err)
	assert.Equal(t, smokeVersion+"\n", string(out))
}
