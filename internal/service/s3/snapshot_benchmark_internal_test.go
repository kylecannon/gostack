package s3

import (
	"bytes"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// snapshotBenchProvider is a provider with one bucket of objects keys, each with one
// version.
func snapshotBenchProvider(b *testing.B, objects int) *Provider {
	b.Helper()

	p := newSnapshotProvider(b)

	bucket := newBucket(benchBucket, "000000000000", regionUSEast1, "", nil)
	for i := range objects {
		bucket.Objects["key-"+strconv.Itoa(i)] = &Object{Versions: []*Version{{
			ID: versionNull, BlobID: "0123456789abcdef0123456789abcdef", Size: 1024, ETag: "etag",
			LastModified: stamped(time.Now().UTC()), ContentType: defaultContentType, StorageClass: storageClassDefault,
			Metadata: map[string]string{"owner": benchBucket},
		}}}
	}

	_, claimed := p.addBucket(bucket)
	require.True(b, claimed)

	return p
}

// BenchmarkSnapshot measures a save and a load of S3's state by the number of
// objects it holds.
func BenchmarkSnapshot(b *testing.B) {
	for _, objects := range []int{1_000, 10_000} {
		p := snapshotBenchProvider(b, objects)

		var saved bytes.Buffer
		require.NoError(b, p.Snapshot(&saved))

		b.Run("op=snapshot/objects="+strconv.Itoa(objects), func(b *testing.B) {
			b.SetBytes(int64(saved.Len()))

			for b.Loop() {
				err := p.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run("op=restore/objects="+strconv.Itoa(objects), func(b *testing.B) {
			b.SetBytes(int64(saved.Len()))

			restored := newSnapshotProvider(b)

			for b.Loop() {
				err := restored.Restore(bytes.NewReader(saved.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
