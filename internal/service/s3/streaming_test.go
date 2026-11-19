package s3_test

import (
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// rssChildEnv marks the run of this test binary that does the measuring.
	rssChildEnv = "GOSTACK_S3_RSS_CHILD"

	streamedSize = 64 << 20
	// maxRSSGrowth is how much the resident set may grow while a 64 MiB object
	// goes in and comes out.
	maxRSSGrowth = 16 << 20
)

// patternReader yields size bytes of a repeating pattern without holding them.
type patternReader struct {
	remaining int64
	sum       uint32
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), r.remaining))
	for i := range n {
		p[i] = byte(r.remaining - int64(i))
	}

	r.sum = crc32.Update(r.sum, crc32.IEEETable, p[:n])
	r.remaining -= int64(n)

	return n, nil
}

// peakRSS is the process's peak resident set size in bytes.
func peakRSS(t *testing.T) int64 {
	t.Helper()

	var usage syscall.Rusage

	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &usage))

	// Linux reports kilobytes, macOS bytes.
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}

	return usage.Maxrss * 1024
}

// TestLargeObjectStreamsEndToEnd sends a 64 MiB object and reads it back, and
// asserts the process's resident set grew by less than 16 MiB. The measuring
// runs in a child process of this test binary: the peak resident size only ever
// grows, so the tests that ran before in this process would hide the growth.
func TestLargeObjectStreamsEndToEnd(t *testing.T) {
	if os.Getenv(rssChildEnv) == "" {
		exe, err := os.Executable()
		require.NoError(t, err)

		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestLargeObjectStreamsEndToEnd$", "-test.v")
		cmd.Env = append(os.Environ(), rssChildEnv+"=1")

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))

		return
	}

	s := newServer(t)
	createBucket(t, s)

	// Warm up every path once, so what is measured is the object's, not the
	// first request's.
	put(t, s, "/bucket/warm", "warm", nil)
	require.Equal(t, http.StatusOK, get(s, http.MethodGet, "/bucket/warm", nil).status)

	before := peakRSS(t)

	body := &patternReader{remaining: streamedSize}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, s.url+"/bucket/large", body)
	require.NoError(t, err)

	req.ContentLength = streamedSize
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, "+
		"SignedHeaders=host, Signature=0")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	getReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.url+"/bucket/large", nil)
	require.NoError(t, err)

	getReq.Header.Set("Authorization", req.Header.Get("Authorization"))

	got, err := http.DefaultClient.Do(getReq)
	require.NoError(t, err)

	defer func() { require.NoError(t, got.Body.Close()) }()

	require.Equal(t, http.StatusOK, got.StatusCode)
	assert.Equal(t, int64(streamedSize), got.ContentLength)

	sum := crc32.NewIEEE()
	n, err := io.Copy(sum, got.Body)
	require.NoError(t, err)
	assert.Equal(t, int64(streamedSize), n)
	assert.Equal(t, body.sum, sum.Sum32(), "the object came back as it went in")

	growth := peakRSS(t) - before
	t.Logf("peak RSS grew by %d KiB", growth>>10)
	assert.Less(t, growth, int64(maxRSSGrowth), "RSS grew by %d bytes", growth)
}
