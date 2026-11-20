package main_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// shutdownSeconds is SHUTDOWN_TIMEOUT for these tests. The child's own
	// deadline is KillMargin short of it.
	shutdownSeconds = 3
	shutdownTimeout = shutdownSeconds * time.Second

	queueSaved   = "saved-at-shutdown"
	messageSaved = "survives-the-stop"
)

// shutdownEnv persists SQS under dir and never flushes on a timer within the
// test, so a state file on disk is the shutdown's save.
func shutdownEnv(dir string) []string {
	return []string{
		"PERSIST_BASE_DIR=" + dir,
		"PERSIST_FREQUENCY=3600",
		fmt.Sprintf("SHUTDOWN_TIMEOUT=%d", shutdownSeconds),
	}
}

// stateFile is where SQS writes its state under dir.
func stateFile(dir string) string {
	return filepath.Join(dir, "sqs", "store.json")
}

// supervised is a supervisor in this process, running the built binary as its
// child, as PID 1 does in the image.
type supervised struct {
	stderr *syncBuffer
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan supervisorResult
}

type supervisorResult struct {
	code int
	err  error
	at   time.Time
}

// startSupervised runs the supervisor with SHUTDOWN_TIMEOUT as its kill timer.
// Each child it starts gets env, and SUPERVISOR_PID names this test process, so
// a restart request arrives here as a SIGUSR1 that the supervisor handles.
func startSupervised(t *testing.T, env ...string) *supervised {
	t.Helper()

	s := &supervised{stderr: &syncBuffer{}, logs: &syncBuffer{}, done: make(chan supervisorResult, 1)}

	home := t.TempDir()
	supervisor := &lifecycle.Supervisor{
		Command: func() *exec.Cmd {
			cmd := exec.Command(binary) //nolint:noctx // the supervisor ends the child; a context would SIGKILL it
			cmd.Env = append([]string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + home,
				envListenAnyPort,
				"ENABLE_CONFIG_UPDATES=1",
			}, env...)
			cmd.Stderr = s.stderr
			cmd.Stdout = &syncBuffer{}

			return cmd
		},
		ShutdownTimeout: shutdownTimeout,
		Logger:          slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	go func() {
		code, err := supervisor.Run(ctx)
		s.done <- supervisorResult{code: code, err: err, at: time.Now()}
	}()

	// The test's own end stops the supervisor, and with it the child.
	t.Cleanup(func() {
		cancel()

		select {
		case res := <-s.done:
			s.done <- res
		case <-time.After(2 * shutdownTimeout):
			t.Errorf("the supervisor did not stop:\n%s\n%s", s.logs.String(), s.stderr.String())
		}
	})

	return s
}

// child waits for the nth child (1 is the first) to log its address, and returns
// a client for it.
func (s *supervised) child(t *testing.T, n int) *gostack {
	t.Helper()

	require.Eventually(t, func() bool {
		return len(listeningAddress.FindAllStringSubmatch(s.stderr.String(), -1)) >= n
	}, startDeadline, poll, "child %d never logged its address:\n%s", n, s.stderr.String())

	matches := listeningAddress.FindAllStringSubmatch(s.stderr.String(), -1)

	return &gostack{base: "http://" + matches[n-1][1]}
}

// stuckUpload starts a request to addr whose body never finishes arriving. The
// gateway holds the connection open for as long as it lasts, so a drain that
// waits for requests in flight waits for it until it is cut.
func stuckUpload(t *testing.T, addr string) {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-amz-json-1.0\r\n"+
		"X-Amz-Target: AmazonSQS.SendMessage\r\nContent-Length: 1000000\r\n\r\n{\"QueueUrl\":", addr)
	require.NoError(t, err)

	// The server has read the request line by now and counts the connection active.
	time.Sleep(200 * time.Millisecond)
}

// seedQueue creates the queue and the message the stop has to save, and checks
// that nothing is on disk yet, so that the file found later came from the stop.
func seedQueue(t *testing.T, g *gostack, dir string) {
	t.Helper()

	url := g.sqs(t, opCreateQueue, map[string]any{fieldQueueName: queueSaved}).QueueURL
	g.sqs(t, "SendMessage", map[string]any{fieldQueueURL: url, fieldBody: messageSaved})

	require.NoFileExists(t, stateFile(dir), "the timer must not have saved it")
}

// requireSaved checks the state file holds the queue and its message.
func requireSaved(t *testing.T, dir string) {
	t.Helper()

	require.Eventually(t, fileHas(stateFile(dir), queueSaved, messageSaved), time.Second, poll,
		"the stop did not save %s", stateFile(dir))
}

// Test_shutdown_savesBeforeTheSupervisorKills is the SIGTERM of docker stop: with
// a request stuck in flight, the child saves its state and exits on its own
// before the supervisor's SIGKILL, so the exit code is 0 and not 137.
func Test_shutdown_savesBeforeTheSupervisorKills(t *testing.T) {
	dir := t.TempDir()
	sv := startSupervised(t, shutdownEnv(dir)...)
	g := sv.child(t, 1)

	seedQueue(t, g, dir)
	stuckUpload(t, strings.TrimPrefix(g.base, "http://"))

	sv.cancel()

	// The supervisor's own SIGKILL is the deadline: a child that saved and ended
	// before it exits 0, and one it had to kill would be 137 and logged. The wait
	// is a guard against a hang, twice the whole budget, not a bound on a step.
	select {
	case res := <-sv.done:
		sv.done <- res

		require.NoError(t, res.err)
		assert.Zero(t, res.code, "the child exited on its own, before the supervisor's SIGKILL; a kill would be 137")
	case <-time.After(2 * shutdownTimeout):
		t.Fatal("the supervisor did not stop")
	}

	assert.NotContains(t, sv.logs.String(), "killing it")
	requireSaved(t, dir)

	// The next start has the queue back.
	again := startGostack(t, "PERSIST_BASE_DIR="+dir)
	assert.Equal(t, []string{queueSaved}, queueNames(again.sqs(t, "ListQueues", map[string]any{}).QueueURLs))
}

// Test_shutdown_restartSavesBeforeItStopsTheChild is the supervisor's restart: the
// state is saved before the old child ends, with a request stuck in flight, and
// the new child has it.
func Test_shutdown_restartSavesBeforeItStopsTheChild(t *testing.T) {
	dir := t.TempDir()
	sv := startSupervised(t, shutdownEnv(dir)...)
	first := sv.child(t, 1)

	seedQueue(t, first, dir)
	stuckUpload(t, strings.TrimPrefix(first.base, "http://"))

	status, body := first.do(t, http.MethodPost, "/_gostack/health", `{"action":"restart"}`)
	require.Equal(t, http.StatusOK, status, body)

	second := sv.child(t, 2)

	assert.NotContains(t, sv.logs.String(), "killing it", "the old child saved and ended inside its deadline")
	requireSaved(t, dir)
	assert.Equal(t, []string{queueSaved}, queueNames(second.sqs(t, "ListQueues", map[string]any{}).QueueURLs),
		"the new child loaded what the old one saved")
}

// Test_shutdown_killSavesBeforeItExits is the kill action: it takes the same path,
// with no supervisor to enforce the deadline, so the process keeps to it itself.
func Test_shutdown_killSavesBeforeItExits(t *testing.T) {
	dir := t.TempDir()
	g := startGostack(t, shutdownEnv(dir)...)

	seedQueue(t, g, dir)
	stuckUpload(t, strings.TrimPrefix(g.base, "http://"))

	status, body := g.do(t, http.MethodPost, "/_gostack/health", `{"action":"kill"}`)
	require.Equal(t, http.StatusOK, status, body)

	// Nothing but the process itself ends the stuck upload, so a process that
	// kept to its deadline exits at all. The wait is a guard against a hang, twice
	// the whole budget, not a bound on how long the shutdown took.
	select {
	case err := <-g.exited:
		require.NoError(t, err, "the process ends with code 0")
	case <-time.After(2 * shutdownTimeout):
		t.Fatal("gostack did not stop")
	}

	requireSaved(t, dir)
}

// queueNames lists the last path element of each queue URL.
func queueNames(urls []string) []string {
	names := make([]string, 0, len(urls))
	for _, u := range urls {
		names = append(names, u[strings.LastIndex(u, "/")+1:])
	}

	return names
}
