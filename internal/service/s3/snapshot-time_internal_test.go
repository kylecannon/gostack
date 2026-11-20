package s3

import (
	"bytes"
	"testing"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/require"
)

// Test_Snapshot_anyRetainUntil saves object versions whose Object Lock dates
// RFC 3339 cannot write once they are in UTC: a retain-until date of the last
// second of 9999 with a zone west of UTC, and default retentions of 100000
// years and of -5000000 days, which LocalStack takes. Each snapshot is written
// and restores the same instants. Before, encoding/json/v2 refused the year
// and every S3 save failed from then on.
func Test_Snapshot_anyRetainUntil(t *testing.T) {
	now := time.Now()
	b := &Bucket{}

	header, err := b.lockAt(new(lockGovernance), &protocol.Time{
		Time: time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("west", -60*60)),
	}, nil, now)
	require.NoError(t, err)

	untils := map[string]time.Time{
		"header":    header.Until,
		"years":     retainedUntil(now, &s3api.DefaultRetention{Years: new(int32(100000))}),
		"past days": retainedUntil(now, &s3api.DefaultRetention{Days: new(int32(-5000000))}),
		"in reach":  retainedUntil(now, &s3api.DefaultRetention{Days: new(int32(1))}),
	}

	for name, until := range untils {
		t.Run(name, func(t *testing.T) {
			first := newSnapshotProvider(t)
			filled := filledBucket("111111111111", "alpha")
			version := filled.Objects["key\xfe"].Versions[0]
			version.Lock = Lock{Mode: lockGovernance, Until: until}

			_, claimed := first.addBucket(filled)
			require.True(t, claimed)

			var saved bytes.Buffer
			require.NoError(t, first.Snapshot(&saved))

			second := newSnapshotProvider(t)
			require.NoError(t, second.Restore(bytes.NewReader(saved.Bytes())))

			got := second.state("111111111111").get("alpha").Objects["key\xfe"].Versions[0].Lock.Until
			require.True(t, until.Equal(got), "restored %v, saved %v", got, until)
		})
	}
}
