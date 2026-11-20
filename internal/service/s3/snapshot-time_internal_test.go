package s3

import (
	"bytes"
	"strings"
	"testing"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stamped is t as the time a Bucket, a Version, an upload or a part holds.
func stamped(t time.Time) protocol.SnapshotTime {
	return protocol.SnapshotTime{Time: t}
}

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
		"header":    header.Until.Time,
		"years":     retainedUntil(now, &s3api.DefaultRetention{Years: new(int32(100000))}),
		"past days": retainedUntil(now, &s3api.DefaultRetention{Days: new(int32(-5000000))}),
		"in reach":  retainedUntil(now, &s3api.DefaultRetention{Days: new(int32(1))}),
	}

	for name, until := range untils {
		t.Run(name, func(t *testing.T) {
			first := newSnapshotProvider(t)
			filled := filledBucket("111111111111", "alpha")
			version := filled.Objects["key\xfe"].Versions[0]
			version.Lock = Lock{Mode: lockGovernance, Until: stamped(until)}

			_, claimed := first.addBucket(filled)
			require.True(t, claimed)

			var saved bytes.Buffer
			require.NoError(t, first.Snapshot(&saved))

			second := newSnapshotProvider(t)
			require.NoError(t, second.Restore(bytes.NewReader(saved.Bytes())))

			got := second.state("111111111111").get("alpha").Objects["key\xfe"].Versions[0].Lock.Until
			require.True(t, until.Equal(got.Time), "restored %v, saved %v", got, until)
		})
	}
}

// Test_Snapshot_anyTime saves each of the seven time fields of a bucket with a
// time RFC 3339 cannot write, and restores the same instants: a year past
// 9999, a year before 0, a zone a day east, a zone with seconds, and the least
// time there is.
func Test_Snapshot_anyTime(t *testing.T) {
	times := map[string]time.Time{
		"past 9999":         time.Unix(300000000000, 123),
		"before 0":          time.Unix(-100000000000, 500000000),
		"a day east":        time.Unix(1700000000, 0).In(time.FixedZone("", 24*60*60)),
		"a day and an hour": time.Unix(1700000000, 0).In(time.FixedZone("", -25*60*60)),
		"offset seconds":    time.Date(1800, 1, 1, 0, 0, 0, 0, time.FixedZone("LMT", -28378)),
		"the least time":    time.Unix(-1<<63, 0),
	}

	for name, give := range times {
		t.Run(name, func(t *testing.T) {
			first := newSnapshotProvider(t)
			filled := filledBucket("111111111111", "alpha")
			object := filled.Objects["key\xfe"]
			upload := onlyUpload(t, filled)

			fields := map[string]*protocol.SnapshotTime{
				"Bucket.CreationDate":  &filled.CreationDate,
				"Version.LastModified": &object.Versions[0].LastModified,
				"Version.Lock.Until":   &object.Versions[0].Lock.Until,
				"delete marker":        &object.Versions[1].LastModified,
				"Upload.Initiated":     &upload.Initiated,
				"Upload.Lock.Until":    &upload.Lock.Until,
				"Part.LastModified":    &upload.Parts[3].LastModified,
			}
			for _, field := range fields {
				*field = stamped(give)
			}

			_, claimed := first.addBucket(filled)
			require.True(t, claimed)

			second := newSnapshotProvider(t)
			require.NoError(t, second.Restore(bytes.NewReader(snapshotOf(t, first))))

			restored := second.state("111111111111").get("alpha")
			restoredObject := restored.Objects["key\xfe"]
			restoredUpload := restored.Uploads[upload.ID]

			got := map[string]protocol.SnapshotTime{
				"Bucket.CreationDate":  restored.CreationDate,
				"Version.LastModified": restoredObject.Versions[0].LastModified,
				"Version.Lock.Until":   restoredObject.Versions[0].Lock.Until,
				"delete marker":        restoredObject.Versions[1].LastModified,
				"Upload.Initiated":     restoredUpload.Initiated,
				"Upload.Lock.Until":    restoredUpload.Lock.Until,
				"Part.LastModified":    restoredUpload.Parts[3].LastModified,
			}

			for field, saved := range fields {
				assert.True(t, give.Equal(got[field].Time), "%s restored %v, saved %v", field, got[field], saved)
			}
		})
	}
}

// onlyUpload is the one upload of a filled bucket.
func onlyUpload(t *testing.T, b *Bucket) *MultipartUpload {
	t.Helper()
	require.Len(t, b.Uploads, 1)

	for _, upload := range b.Uploads {
		return upload
	}

	return nil
}

// Test_Restore_readsEarlierSnapshots restores a snapshot in each form one has
// held its times in: RFC 3339 only, as written before a time RFC 3339 could
// not hold had a form; RFC 3339 and epoch seconds together, as written after;
// and epoch seconds only, as a hand-edited one may be. Every time reads as the
// instant that was saved, and a snapshot of what was read restores again.
func Test_Restore_readsEarlierSnapshots(t *testing.T) {
	const (
		// 1700000000 epoch seconds, in UTC and in a zone east of it.
		whole = `"2023-11-14T22:13:20Z"`
		east  = `"2023-11-15T03:43:20.5+05:30"`
	)

	snapshot := func(creation, modified, until, initiated string) string {
		return `{"v":1,"data":{"buckets":[{"Name":"a","Account":"1","Region":"us-east-1","CreationDate":` + creation +
			`,"Objects":[{"Key":"k","Versions":[{"ID":"null","LastModified":` + modified +
			`,"Lock":{"Mode":"GOVERNANCE","Until":` + until + `}}]}]` +
			`,"Uploads":[{"ID":"u","Key":"k","Initiated":` + initiated + `,"Lock":{"Until":` + until + `},` +
			`"Parts":{"1":{"Number":1,"BlobID":"x","LastModified":` + modified + `}}}]}]}}`
	}

	var (
		at     = time.Unix(1700000000, 0)
		atHalf = time.Unix(1700000000, 500000000)
		far    = time.Unix(300000000000, 0)
		early  = time.Unix(-100000000000, 500000000)
	)

	tests := []struct {
		name string
		give string
		// want is the creation date, the version's date, the retain-until date
		// of version and upload, and the upload's date.
		want [4]time.Time
	}{
		{
			name: "RFC 3339 only, as before epoch seconds",
			give: snapshot(whole, east, whole, east),
			want: [4]time.Time{at, atHalf, at, atHalf},
		},
		{
			name: "both forms",
			give: snapshot(whole, "300000000000", "-99999999999.5", east),
			want: [4]time.Time{at, far, early, atHalf},
		},
		{
			name: "epoch seconds only",
			give: snapshot("1700000000", "300000000000", "-99999999999.5", "1700000000.5"),
			want: [4]time.Time{at, far, early, atHalf},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newSnapshotProvider(t)
			require.NoError(t, p.Restore(strings.NewReader(tt.give)))

			b := p.state("1").get("a")
			require.NotNil(t, b)

			v := b.Objects["k"].Versions[0]
			u := b.Uploads["u"]

			got := [4]time.Time{b.CreationDate.Time, v.LastModified.Time, v.Lock.Until.Time, u.Initiated.Time}
			for i, want := range tt.want {
				assert.True(t, want.Equal(got[i]), "time %d read %v, want %v", i, got[i], want)
			}

			assert.True(t, u.Lock.Until.Equal(tt.want[2]), "the upload's retain-until date reads %v", u.Lock.Until)
			assert.True(t, u.Parts[1].LastModified.Equal(tt.want[1]), "the part's date reads %v", u.Parts[1].LastModified)

			// What it wrote is read again: the snapshot of a restored snapshot
			// restores to the same times.
			again := newSnapshotProvider(t)
			require.NoError(t, again.Restore(bytes.NewReader(snapshotOf(t, p))))
			assert.True(t, tt.want[1].Equal(again.state("1").get("a").Objects["k"].Versions[0].LastModified.Time))
		})
	}
}

// Test_Snapshot_writesRFC3339ForReachableTimes checks that a time RFC 3339 can
// write is still written as RFC 3339, in the zone it was in, so a snapshot
// written now reads in a build that reads only RFC 3339 times.
func Test_Snapshot_writesRFC3339ForReachableTimes(t *testing.T) {
	p := newSnapshotProvider(t)
	b := newBucket("a", "1", regionUSEast1, "", nil)
	b.CreationDate = stamped(time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC))
	_, claimed := p.addBucket(b)
	require.True(t, claimed)

	assert.Contains(t, string(snapshotOf(t, p)), `"CreationDate":"2023-11-14T22:13:20Z"`)
}
