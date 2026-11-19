package main_test

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s3BodiesOnDisk counts the bodies under the S3 assets of dir.
func s3BodiesOnDisk(t *testing.T, dir string) int {
	t.Helper()

	count := 0
	err := filepath.WalkDir(filepath.Join(dir, "s3", "assets", ".gostack", "blobs"),
		func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				count++
			}

			return err
		})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		require.NoError(t, err)
	}

	return count
}

// Test_gostack_keeps_no_s3_bodies_for_a_refused_state_file starts gostack
// twice with s3/store.json a link that leaves the base dir. The load refuses
// S3, which is then neither saved nor swept, so it keeps its bodies in memory:
// none is left on disk.
func Test_gostack_keeps_no_s3_bodies_for_a_refused_state_file(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s3"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "store.json"), []byte(`{"v":1,"data":{"buckets":[]}}`), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "store.json"), filepath.Join(dir, "s3", "store.json")))

	for run := range 2 {
		g := startGostack(t, "PERSIST_BASE_DIR="+dir, "PERSIST_S3=1")
		assert.False(t, g.s3Persisted(t))

		bucket := "/bucket" + strconv.Itoa(run)
		g.s3OK(t, http.MethodPut, bucket, nil)

		for i := range 5 {
			g.s3OK(t, http.MethodPut, bucket+"/k"+strconv.Itoa(i), []byte("v"))
		}

		g.s3OK(t, http.MethodDelete, bucket+"/k0", nil)
		got, _ := g.s3OK(t, http.MethodGet, bucket+"/k1", nil)
		assert.Equal(t, "v", string(got))

		require.NoError(t, g.cmd.Process.Signal(syscall.SIGTERM))

		select {
		case err := <-g.exited:
			require.NoError(t, err, "SIGTERM ends gostack cleanly")
		case <-time.After(startDeadline):
			t.Fatal("gostack did not exit after SIGTERM")
		}

		assert.Zero(t, s3BodiesOnDisk(t, dir), "run %d: a service that is never saved keeps no bodies on disk", run)
	}
}

// Test_gostack_leaves_an_unsafe_s3_alone_on_the_state_routes starts gostack
// with S3 state from an earlier run and an assets directory gostack cannot
// write. Load and save answer 500 and leave the in-memory S3 alone, a reset
// empties memory only, and the state file and assets stay as they were.
func Test_gostack_leaves_an_unsafe_s3_alone_on_the_state_routes(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "s3", "store.json")
	saved := []byte(`{"v":1,"data":{"buckets":[]}}`)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s3", "assets"), 0o750))
	require.NoError(t, os.WriteFile(state, saved, 0o600))
	readOnly(t, filepath.Join(dir, "s3", "assets"))

	g := startGostack(t, "PERSIST_BASE_DIR="+dir, "PERSIST_S3=1", envFrequency)
	g.s3OK(t, http.MethodPut, "/bucket", nil)
	g.s3OK(t, http.MethodPut, "/bucket/key", []byte("mem"))

	status, body := g.do(t, http.MethodPost, "/_gostack/state/load", "")
	assert.Equal(t, http.StatusInternalServerError, status, body)

	got, _ := g.s3OK(t, http.MethodGet, "/bucket/key", nil)
	assert.Equal(t, "mem", string(got), "a load leaves the in-memory S3 alone")

	status, body = g.do(t, http.MethodPost, "/_gostack/state/reset?service=s3", "")
	assert.Equal(t, http.StatusOK, status, body)

	status, body = g.do(t, http.MethodGet, "/bucket/key", "")
	assert.NotEqual(t, http.StatusOK, status, "a reset empties memory: %s", body)

	status, body = g.do(t, http.MethodPost, "/_gostack/state/save", "")
	assert.Equal(t, http.StatusInternalServerError, status, body)

	b, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, string(saved), string(b))
	assert.Empty(t, mustReadDir(t, filepath.Join(dir, "s3", "assets")))
}
