package s3

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

const (
	benchBucket   = "bench"
	benchAccount  = "000000000000"
	benchKeyCount = 10_000
)

// benchProvider holds a bucket of benchKeyCount keys: 100 folders of 100
// objects, "folder-<n>/object-<m>". The objects are put straight into the
// state, so the benchmark measures the listing and not the writes.
func benchProvider(tb testing.TB) *Provider {
	tb.Helper()

	p := New(func() Settings { return Settings{LocalstackHost: "localhost.localstack.cloud:4566"} },
		NewEphemeral(tb.TempDir()))
	tb.Cleanup(p.Close)

	b := newBucket(benchBucket, benchAccount, regionUSEast1, "", nil)
	p.state(benchAccount).buckets[benchBucket] = b

	now := time.Now().UTC()

	for i := range benchKeyCount {
		key := "folder-" + strconv.Itoa(i/100) + "/object-" + strconv.Itoa(i%100)
		b.Objects[key] = &Object{Versions: []*Version{{
			ID: versionNull, ETag: "d41d8cd98f00b204e9800998ecf8427e", Size: 11, LastModified: now,
			StorageClass: storageClassDefault, ChecksumAlgorithm: "CRC32", ChecksumType: checksumFullObject,
		}}}
	}

	return p
}

// BenchmarkListObjectsV2 serves a ListObjectsV2 of a bucket of 10,000 keys
// through the generated router and XML writer: flat, which pages a thousand
// keys, and with a delimiter, which folds the bucket into 100 common prefixes.
func BenchmarkListObjectsV2(b *testing.B) {
	p := benchProvider(b)

	tests := []struct {
		name  string
		query string
	}{
		{name: "delimiter=none/max=1000", query: "list-type=2"},
		{name: "delimiter=none/max=100", query: "list-type=2&max-keys=100"},
		{name: "delimiter=slash", query: "list-type=2&delimiter=%2F"},
		{name: "delimiter=slash/prefix=folder-7", query: "list-type=2&delimiter=%2F&prefix=folder-7%2F"},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/"+benchBucket+"?"+tt.query, http.NoBody)
			rc := &request.Context{
				Service: serviceName, AccountID: benchAccount, Region: regionUSEast1, RequestID: "bench", Request: r,
			}

			var size int

			for b.Loop() {
				w := httptest.NewRecorder()
				p.serve(w, rc)

				if w.Code != http.StatusOK {
					b.Fatalf("status %d: %s", w.Code, w.Body.String())
				}

				size = w.Body.Len()
			}

			b.SetBytes(int64(size))
		})
	}
}

func TestBenchListingIsTheBucketItClaims(t *testing.T) {
	p := benchProvider(t)

	r := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/"+benchBucket+"?list-type=2&delimiter=%2F", http.NoBody)
	rc := &request.Context{
		Service: serviceName, AccountID: benchAccount, Region: regionUSEast1, RequestID: "t", Request: r,
	}

	w := httptest.NewRecorder()
	p.serve(w, rc)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "<KeyCount>100</KeyCount>")
}
