package s3

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/blob"
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

	return benchProviderOf(tb, benchKeyCount)
}

// benchProviderOf is benchProvider with n keys.
func benchProviderOf(tb testing.TB, n int) *Provider {
	tb.Helper()

	p := New(func() Settings { return Settings{LocalstackHost: "localhost.localstack.cloud:4566"} },
		blob.NewEphemeral(tb.TempDir()))
	tb.Cleanup(func() { p.Close(context.WithoutCancel(tb.Context())) })

	b := newBucket(benchBucket, benchAccount, regionUSEast1, "", nil)
	p.state(benchAccount).buckets[benchBucket] = b

	now := time.Now().UTC()

	for i := range n {
		key := "folder-" + strconv.Itoa(i/100) + "/object-" + strconv.Itoa(i%100)
		b.Objects[key] = &Object{Versions: []*Version{{
			ID: versionNull, ETag: "d41d8cd98f00b204e9800998ecf8427e", Size: 11, LastModified: stamped(now),
			StorageClass: storageClassDefault, ChecksumAlgorithm: "CRC32", ChecksumType: checksumFullObject,
		}}}
	}

	b.reindex()

	return p
}

// BenchmarkIndexInsert is the cost a new key adds to a write: a binary search
// and the memmove of the keys above it, into a bucket of 100,000. The keys sort
// before every key already there, so each insert moves the whole index: the
// worst case.
func BenchmarkIndexInsert(b *testing.B) {
	p := benchProviderOf(b, 100_000)
	bucket := p.state(benchAccount).buckets[benchBucket]
	object := &Object{}

	b.ReportAllocs()

	i := 0

	for b.Loop() {
		key := "a-" + strconv.Itoa(i*7919%1_000_003)
		i++

		bucket.mu.Lock()
		bucket.putObject(key, object)
		bucket.mu.Unlock()
	}
}

// tooSlowToGate leaves out keys=100000 with no delimiter of
// BenchmarkListObjectsV2Paging (docs/standards.md, "Benchmarks"): paging the
// 100,000 keys flat took 30.8 ms, the slowest of three runs at
// -benchtime=500ms on linux/arm64 in a golang:1.27.4 container. The 100,000
// keys with a delimiter stay (0.3 ms), and keys=10000 with none covers the flat
// paging (2.6 ms).
func tooSlowToGate(keys int, delimiter string) bool {
	return keys == 100_000 && delimiter == ""
}

// BenchmarkListObjectsV2Paging pages through a whole bucket with ListObjectsV2
// calls: flat, a thousand keys to the page, and with a delimiter, which lists
// the folders (a hundred keys to a folder) a hundred to the page. An op is the
// whole paging.
func BenchmarkListObjectsV2Paging(b *testing.B) {
	for _, keys := range []int{10_000, 100_000} {
		p := benchProviderOf(b, keys)

		r := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/"+benchBucket, http.NoBody)
		rc := &request.Context{
			Service: serviceName, AccountID: benchAccount, Region: regionUSEast1, RequestID: benchBucket, Request: r,
		}
		ctx := request.With(b.Context(), rc)

		for _, tt := range []struct{ name, delimiter string }{{"none", ""}, {"slash", "/"}} {
			if tooSlowToGate(keys, tt.delimiter) {
				continue
			}

			b.Run(fmt.Sprintf("keys=%d/delimiter=%s", keys, tt.name), func(b *testing.B) {
				for b.Loop() {
					in := &s3api.ListObjectsV2Input{Bucket: new(benchBucket)}
					if tt.delimiter != "" {
						in.Delimiter, in.MaxKeys = new(tt.delimiter), new(int32(100))
					}

					listed, pages := pageThrough(ctx, b, p, in)
					if tt.delimiter == "" && listed != keys {
						b.Fatalf("listed %d keys in %d pages, want %d", listed, pages, keys)
					}
				}
			})
		}
	}
}

// pageThrough lists a bucket to its end, continuing each page from the last,
// and returns how many entries and pages it took.
func pageThrough(ctx context.Context, b *testing.B, p *Provider, in *s3api.ListObjectsV2Input) (listed, pages int) {
	b.Helper()

	for {
		out, err := p.ListObjectsV2(ctx, in)
		if err != nil {
			b.Fatal(err)
		}

		pages++
		listed += len(out.Contents) + len(out.CommonPrefixes)

		if !*out.IsTruncated {
			return listed, pages
		}

		in.ContinuationToken = out.NextContinuationToken
	}
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
				Service: serviceName, AccountID: benchAccount, Region: regionUSEast1, RequestID: benchBucket, Request: r,
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
