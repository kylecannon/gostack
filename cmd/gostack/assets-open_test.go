package main_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readOnly makes dir read-only for the rest of the test, and skips the test
// for root, whom the mode does not stop.
func readOnly(t *testing.T, dir string) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}

	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
}

// s3Persisted returns what /_gostack/health says about S3.
func (g *gostack) s3Persisted(t *testing.T) bool {
	t.Helper()

	status, body := g.do(t, http.MethodGet, "/_gostack/health", "")
	require.Equal(t, http.StatusOK, status, body)

	var health struct {
		Services map[string]struct {
			Persisted bool `json:"persisted"`
		} `json:"services"`
	}

	require.NoError(t, json.Unmarshal([]byte(body), &health))

	return health.Services["s3"].Persisted
}

// Test_gostack_starts_when_the_s3_assets_cannot_be_made starts gostack on a
// read-only base dir with SQS persisted, which persists S3 too. S3's assets
// cannot be made, so S3 keeps its bodies in memory, is neither loaded nor
// saved, and health says so; gostack still starts.
func Test_gostack_starts_when_the_s3_assets_cannot_be_made(t *testing.T) {
	dir := t.TempDir()
	readOnly(t, dir)

	g := startGostack(t, "PERSIST_BASE_DIR="+dir, "PERSIST_SQS=1")

	assert.False(t, g.s3Persisted(t))

	g.s3OK(t, http.MethodPut, "/bucket", nil)
	g.s3OK(t, http.MethodPut, "/bucket/key", []byte("in memory"))
	got, _ := g.s3OK(t, http.MethodGet, "/bucket/key", nil)
	assert.Equal(t, "in memory", string(got))
}

// Test_gostack_never_saves_s3_without_its_assets starts gostack on a base dir
// that holds S3 state from an earlier run and an assets directory gostack
// cannot write. S3 runs in memory, and neither a save by hand nor the save at
// shutdown writes over the state file.
func Test_gostack_never_saves_s3_without_its_assets(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s3", "store.json")
	saved := []byte(`{"v":1,"data":{"buckets":[]}}`)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s3", "assets"), 0o750))
	require.NoError(t, os.WriteFile(state, saved, 0o600))
	readOnly(t, filepath.Join(dir, "s3", "assets"))

	g := startGostack(t, "PERSIST_BASE_DIR="+dir, "PERSIST_S3=1", envFrequency)

	assert.False(t, g.s3Persisted(t))

	g.s3OK(t, http.MethodPut, "/bucket", nil)
	g.s3OK(t, http.MethodPut, "/bucket/key", []byte("in memory"))

	status, body := g.do(t, http.MethodPost, "/_gostack/state/save", "")
	assert.Equal(t, http.StatusInternalServerError, status, body)
	assert.Contains(t, body, "s3")

	require.NoError(t, g.cmd.Process.Signal(syscall.SIGTERM))

	select {
	case err := <-g.exited:
		require.NoError(t, err, "SIGTERM ends gostack cleanly")
	case <-time.After(startDeadline):
		t.Fatal("gostack did not exit after SIGTERM")
	}

	got, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, string(saved), string(got), "the state file is as the earlier run left it")
	assert.Empty(t, mustReadDir(t, filepath.Join(dir, "s3", "assets")))
}
