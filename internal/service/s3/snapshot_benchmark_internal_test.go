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

// snapshotSizes are the objects of a save and of a load of S3's state, which
// stop at different counts (docs/standards.md, "Benchmarks"): a restore of 1,000
// objects took 8.5 ms on a runner, over the limit, and a snapshot of them 5.8
// ms. The cost is linear in the count, so the widest cell under the limit
// stands for the ladder.
var snapshotSizes = struct{ snapshot, restore []int }{
	snapshot: []int{10, 1_000},
	restore:  []int{10, 500},
}

// BenchmarkSnapshot measures a save and a load of S3's state by the number of
// objects it holds.
func BenchmarkSnapshot(b *testing.B) {
	for _, objects := range snapshotSizes.snapshot {
		b.Run("op=snapshot/objects="+strconv.Itoa(objects), func(b *testing.B) {
			p := snapshotBenchProvider(b, objects)

			var saved bytes.Buffer
			require.NoError(b, p.Snapshot(&saved))
			b.SetBytes(int64(saved.Len()))

			for b.Loop() {
				err := p.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	for _, objects := range snapshotSizes.restore {
		b.Run("op=restore/objects="+strconv.Itoa(objects), func(b *testing.B) {
			var saved bytes.Buffer
			require.NoError(b, snapshotBenchProvider(b, objects).Snapshot(&saved))
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
