package main_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s3 sends one S3 call, signed for us-east-1, and returns the status, the body
// and the ETag.
func (g *gostack) s3(t *testing.T, method, path string, body []byte) (int, []byte, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, g.base+path, bytes.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, data, resp.Header.Get("ETag")
}

func (g *gostack) s3OK(t *testing.T, method, path string, body []byte) ([]byte, string) {
	t.Helper()

	status, data, etag := g.s3(t, method, path, body)
	require.Less(t, status, http.StatusMultipleChoices, "%s %s: %s", method, path, data)

	return data, etag
}

// Test_gostack_s3_survives_a_kill is the S3 half of the docker kill round
// trip: objects, a versioned key under a delete marker and an upload in
// progress are saved, gostack is killed, and a new process on the same base
// directory has them back, byte for byte, and completes the upload.
func Test_gostack_s3_survives_a_kill(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PERSIST_BASE_DIR=" + dir, "PERSIST_S3=1", envFrequency}

	first := startGostack(t, env...)

	first.s3OK(t, http.MethodPut, "/bucket", nil)

	object := make([]byte, 1<<20)
	_, err := rand.Read(object)
	require.NoError(t, err)
	first.s3OK(t, http.MethodPut, "/bucket/object", object)

	first.s3OK(t, http.MethodPut, "/bucket?versioning",
		[]byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`))
	first.s3OK(t, http.MethodPut, "/bucket/versioned", []byte("one"))
	first.s3OK(t, http.MethodPut, "/bucket/versioned", []byte("two"))
	first.s3OK(t, http.MethodDelete, "/bucket/versioned", nil)

	created, _ := first.s3OK(t, http.MethodPost, "/bucket/upload?uploads", nil)

	var initiated struct {
		UploadID string `xml:"UploadId"`
	}

	require.NoError(t, xml.Unmarshal(created, &initiated))

	part := make([]byte, 5<<20)
	_, err = rand.Read(part)
	require.NoError(t, err)

	_, etag1 := first.s3OK(t, http.MethodPut, "/bucket/upload?partNumber=1&uploadId="+initiated.UploadID, part)
	_, etag2 := first.s3OK(t, http.MethodPut, "/bucket/upload?partNumber=2&uploadId="+initiated.UploadID, []byte("end"))

	file := filepath.Join(dir, "s3", "store.json")
	require.Eventually(t, fileHas(file, initiated.UploadID, `"versioned"`), flushDeadline, poll,
		"no flush wrote %s", file)

	require.NoError(t, first.cmd.Process.Signal(syscall.SIGKILL))

	select {
	case err := <-first.exited:
		require.Error(t, err, "SIGKILL ends the process with an error")
	case <-time.After(killDeadline):
		t.Fatal("gostack survived SIGKILL")
	}

	second := startGostack(t, env...)

	got, _ := second.s3OK(t, http.MethodGet, "/bucket/object", nil)
	assert.Equal(t, sha256.Sum256(object), sha256.Sum256(got))

	status, _, _ := second.s3(t, http.MethodGet, "/bucket/versioned", nil)
	assert.Equal(t, http.StatusNotFound, status, "the delete marker is current")

	versions, _ := second.s3OK(t, http.MethodGet, "/bucket?versions&prefix=versioned", nil)
	assert.Equal(t, 2, bytes.Count(versions, []byte("<Version>")), string(versions))
	assert.Equal(t, 1, bytes.Count(versions, []byte("<DeleteMarker>")), string(versions))

	completion := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>"+
		"<Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", etag1, etag2)
	second.s3OK(t, http.MethodPost, "/bucket/upload?uploadId="+initiated.UploadID, []byte(completion))

	got, _ = second.s3OK(t, http.MethodGet, "/bucket/upload", nil)
	assert.Equal(t, sha256.Sum256(slices.Concat(part, []byte("end"))), sha256.Sum256(got))
}
