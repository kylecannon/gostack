package s3

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const differentialCases = 1000

// The reference below is the listing as it was before the buckets kept their
// keys in order: take every key of the map that the query selects, sort them,
// and walk them one by one. It shares no code with the indexed walk, so a
// disagreement is a bug in one of them.

// referenceSnapshot returns the keys w selects in byte order, each with its
// chain, whose current version keep accepts.
func referenceSnapshot(b *Bucket, w walk, keep func(current *Version) bool) []listed {
	keys := make([]listed, 0, len(b.Objects))

	for key, object := range b.Objects {
		if !strings.HasPrefix(key, w.prefix) || key < w.from || (key == w.from && !w.inclusive) {
			continue
		}

		current := object.current()
		if current == nil || !keep(current) {
			continue
		}

		keys = append(keys, listed{key: key, current: current, versions: slices.Clone(object.Versions)})
	}

	slices.SortFunc(keys, func(a, b listed) int { return strings.Compare(a.key, b.key) })

	return keys
}

func referenceKeys(b *Bucket, w walk) listPage {
	var pg listPage

	if w.limit == 0 {
		return pg
	}

	var lastPrefix string

	for _, k := range referenceSnapshot(b, w, func(current *Version) bool { return !current.DeleteMarker }) {
		cp := w.commonPrefix(k.key)
		if cp != "" {
			if cp == lastPrefix {
				continue
			}

			if w.marker != "" && strings.HasPrefix(w.marker, cp) {
				lastPrefix = cp

				continue
			}
		}

		if len(pg.keys)+len(pg.prefixes) >= w.limit {
			pg.truncated = true
			pg.resume = k.key

			break
		}

		if cp != "" {
			lastPrefix = cp
			pg.prefixes = append(pg.prefixes, cp)
			pg.last = cp

			continue
		}

		pg.keys = append(pg.keys, k)
		pg.last = k.key
	}

	return pg
}

func referenceVersions(b *Bucket, w walk) versionPage {
	var pg versionPage

	if w.limit == 0 {
		return pg
	}

	var (
		lastPrefix  string
		markerFound bool
	)

	for _, k := range referenceSnapshot(b, w, func(*Version) bool { return true }) {
		cp := w.commonPrefix(k.key)
		if cp != "" && (cp == lastPrefix || (w.marker != "" && strings.HasPrefix(w.marker, cp))) {
			lastPrefix = cp

			continue
		}

		for i, v := range slices.Backward(k.versions) {
			if w.marker != "" && k.key == w.marker {
				skip, found := w.skipVersion(v, markerFound)
				markerFound = found

				if skip {
					continue
				}
			}

			if len(pg.entries)+len(pg.prefixes) >= w.limit {
				pg.truncated = true

				return pg
			}

			if cp != "" {
				lastPrefix = cp
				pg.prefixes = append(pg.prefixes, cp)
				pg.last = versionMark{key: cp}

				break
			}

			pg.entries = append(pg.entries, versionEntry{key: k.key, version: v, latest: i == len(k.versions)-1})
			pg.last = versionMark{key: k.key, versionID: v.ID}
		}
	}

	return pg
}

// keyAlphabet is small, so random keys share prefixes and delimiters, and it
// holds 0xff, the byte a prefix's end cannot be found past.
var keyAlphabet = []string{"a", "b", "c", "/", "ab", "\xff", "é"}

func randomString(rng *rand.Rand, maxLen int) string {
	var s strings.Builder

	for range rng.IntN(maxLen + 1) {
		s.WriteString(keyAlphabet[rng.IntN(len(keyAlphabet))])
	}

	return s.String()
}

// randomBucket builds a bucket of random keys through the index's own methods,
// in random order, then drops some. A key's chain has up to three versions, any
// of them a delete marker.
func randomBucket(rng *rand.Rand) *Bucket {
	b := newBucket("bucket", benchAccount, regionUSEast1, "", nil)
	sequence := 0

	for range 10 + rng.IntN(60) {
		key := randomString(rng, 4)
		if key == "" {
			continue
		}

		object := &Object{}

		for range 1 + rng.IntN(3) {
			sequence++
			object.Versions = append(object.Versions, &Version{
				ID: versionID(sequence), DeleteMarker: rng.IntN(4) == 0, LastModified: stamped(time.Unix(int64(sequence), 0)),
			})
		}

		b.putObject(key, object)
	}

	for _, key := range slices.Clone(b.keys) {
		if rng.IntN(8) == 0 {
			b.dropObject(key)
		}
	}

	return b
}

// randomWalk is a walk of the three kinds of listing, over the bucket's keys or
// keys that are not in it.
func randomWalk(rng *rand.Rand, b *Bucket, versions bool) walk {
	pick := func() string {
		if len(b.keys) > 0 && rng.IntN(3) > 0 {
			return b.keys[rng.IntN(len(b.keys))]
		}

		return randomString(rng, 4)
	}

	w := walk{
		delimiter: []string{"", "/", "/", "a", "ab", "\xff"}[rng.IntN(6)],
		limit:     []int{0, 1, 2, 3, 5, 8, 1000}[rng.IntN(7)],
	}

	if rng.IntN(5) < 2 {
		w.prefix = randomString(rng, 2)
	}

	switch {
	case versions:
		w.marker, w.from, w.inclusive = "", "", true

		if rng.IntN(2) == 0 {
			w.marker = pick()
			w.from = w.marker

			if chain := b.Objects[w.marker]; chain != nil && rng.IntN(2) == 0 {
				w.versionMarker = chain.Versions[rng.IntN(len(chain.Versions))].ID
			} else if rng.IntN(3) == 0 {
				w.versionMarker = versionID(1 + rng.IntN(200))
			}
		}
	case rng.IntN(3) == 0: // ListObjects: the marker is exclusive and skips its prefix.
		w.marker = pick()
		w.from = w.marker
	case rng.IntN(2) == 0: // ListObjectsV2 with StartAfter.
		w.from = pick()
	default: // ListObjectsV2 with a continuation token.
		w.from, w.inclusive = pick(), true
	}

	return w
}

func keyIDs(pg listPage) []string {
	ids := make([]string, 0, len(pg.keys))
	for _, k := range pg.keys {
		ids = append(ids, k.key+"@"+k.current.ID)
	}

	return ids
}

func requireIndexConsistent(t *testing.T, b *Bucket) {
	t.Helper()

	want := make([]string, 0, len(b.Objects))
	for key := range b.Objects {
		want = append(want, key)
	}

	slices.Sort(want)

	require.Equal(t, want, append([]string{}, b.keys...), "the key index is the map's keys in byte order")

	ups := make([]*MultipartUpload, 0, len(b.Uploads))
	for _, u := range b.Uploads {
		ups = append(ups, u)
	}

	slices.SortFunc(ups, compareUploads)

	require.Equal(t, ups, append([]*MultipartUpload{}, b.uploadOrder...), "the upload index is the map's uploads in order")
}

// TestListWalkMatchesTheBruteForceReference pages random buckets with random
// prefixes, delimiters, markers and limits through the indexed walk and through
// the reference, and requires the same page, keys and versions alike, and the same
// pages when the walk is followed to its end.
func TestListWalkMatchesTheBruteForceReference(t *testing.T) {
	const seed = uint64(20260218)
	rng := rand.New(rand.NewPCG(seed, seed))

	for c := range differentialCases {
		b := randomBucket(rng)
		requireIndexConsistent(t, b)

		versions := c%2 == 1
		w := randomWalk(rng, b, versions)
		name := fmt.Sprintf("seed=%d case=%d versions=%v walk=%+v", seed, c, versions, w)

		if versions {
			got, want := b.listVersions(w), referenceVersions(b, w)
			assert.Equal(t, versionIDs(want), versionIDs(got), name)
			assert.Equal(t, want.prefixes, got.prefixes, name)
			assert.Equal(t, want.truncated, got.truncated, name)
			assert.Equal(t, want.last, got.last, name)

			continue
		}

		got, want := b.listKeys(w), referenceKeys(b, w)
		assert.Equal(t, keyIDs(want), keyIDs(got), name)
		assert.Equal(t, want.prefixes, got.prefixes, name)
		assert.Equal(t, want.truncated, got.truncated, name)
		assert.Equal(t, want.last, got.last, name)
		assert.Equal(t, want.resume, got.resume, name)

		// Follow the walk the way ListObjectsV2 does, from each page's resume
		// key, until the reference runs out.
		pageW := w

		for range 50 {
			want, got := referenceKeys(b, pageW), b.listKeys(pageW)
			require.Equal(t, keyIDs(want), keyIDs(got), name)
			require.Equal(t, want.prefixes, got.prefixes, name)

			if !want.truncated {
				break
			}

			pageW.from, pageW.inclusive, pageW.marker = want.resume, true, ""
		}
	}
}

func versionIDs(pg versionPage) []string {
	ids := make([]string, 0, len(pg.entries))
	for _, e := range pg.entries {
		ids = append(ids, fmt.Sprintf("%s@%s@%v", e.key, e.version.ID, e.latest))
	}

	return ids
}

func TestPrefixEndStepsPastEveryKeyOfThePrefix(t *testing.T) {
	keys := listedKeys{
		{key: "a"},
		{key: "a/"},
		{key: "a/\xff"},
		{key: "a/\xff\xff"},
		{key: "a0"},
		{key: "b"},
		{key: "\xff"},
		{key: "\xff\xff"},
	}

	tests := []struct {
		prefix string
		want   int
	}{
		{prefix: "a/", want: 4},
		{prefix: "a/\xff", want: 4},
		{prefix: "a", want: 5},
		{prefix: "\xff", want: len(keys)},
		{prefix: "\xff\xff", want: len(keys)},
		{prefix: "zz", want: len(keys) - 2},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, prefixEnd(keys, tt.prefix), "prefix %q", tt.prefix)
	}
}

// uploadReference is ListMultipartUploads' page as it was built from every
// upload of the bucket, sorted: the fill over the whole slice, with neither the
// start search nor the early stop that a raw page has.
func uploadReference(b *Bucket, pg uploadPage) (uploads []string, prefixes []string, truncated bool) {
	all := make([]*MultipartUpload, 0, len(b.Uploads))
	for _, u := range b.Uploads {
		all = append(all, u)
	}

	slices.SortFunc(all, compareUploads)

	pg.raw = false

	return pageOf(pg, all)
}

func pageOf(pg uploadPage, uploads []*MultipartUpload) (ids, prefixes []string, truncated bool) {
	out := &s3api.ListMultipartUploadsOutput{}
	pg.fill(out, uploads)

	for _, u := range out.Uploads {
		ids = append(ids, *u.Key+"@"+*u.UploadId)
	}

	for _, p := range out.CommonPrefixes {
		prefixes = append(prefixes, *p.Prefix)
	}

	return ids, prefixes, out.IsTruncated != nil && *out.IsTruncated
}

func TestListUploadsMatchTheBruteForceReference(t *testing.T) {
	const seed = uint64(20260218)
	rng := rand.New(rand.NewPCG(seed, seed))

	for c := range differentialCases {
		b := newBucket("bucket", benchAccount, regionUSEast1, "", nil)

		var ids []string

		for n := range 5 + rng.IntN(40) {
			key := randomString(rng, 4)
			if key == "" {
				continue
			}

			upload := &MultipartUpload{
				ID: fmt.Sprintf("id-%03d", n), Key: key, Initiated: stamped(time.Unix(int64(rng.IntN(20)), 0)),
			}
			b.addUpload(upload)
			ids = append(ids, upload.ID)
		}

		for _, id := range ids {
			if rng.IntN(6) == 0 {
				b.removeUpload(id)
			}
		}

		requireIndexConsistent(t, b)

		pg := uploadPage{
			prefix: randomString(rng, 2), delimiter: []string{"", "/", "a"}[rng.IntN(3)],
			max: []int{1, 2, 5, 1000}[rng.IntN(4)], encode: func(s string) string { return s }, raw: true,
		}

		if rng.IntN(2) == 0 && len(b.uploadOrder) > 0 {
			marked := b.uploadOrder[rng.IntN(len(b.uploadOrder))]
			pg.keyMarker = marked.Key

			if rng.IntN(2) == 0 {
				pg.uploadIDMarker = marked.ID
			}
		} else if rng.IntN(3) == 0 {
			pg.keyMarker = randomString(rng, 3)
		}

		name := fmt.Sprintf("seed=%d case=%d page=%+v", seed, c, pg)

		wantIDs, wantPrefixes, wantTruncated := uploadReference(b, pg)

		gotIDs, gotPrefixes, gotTruncated := pageOf(pg, b.uploadOrder[pg.start(b.uploadOrder):])
		assert.Equal(t, wantIDs, gotIDs, name)
		assert.Equal(t, wantPrefixes, gotPrefixes, name)
		assert.Equal(t, wantTruncated, gotTruncated, name)
	}
}

func TestIndexFollowsEveryChange(t *testing.T) {
	b := newBucket("bucket", benchAccount, regionUSEast1, "", nil)

	for _, key := range []string{"m", "c", "x", "c", "a"} {
		b.putObject(key, &Object{})
	}

	b.dropObject("c")
	b.dropObject("missing")
	require.Equal(t, []string{"a", "m", "x"}, b.keys)
	requireIndexConsistent(t, b)

	b.clearObjects()
	require.Empty(t, b.keys)

	b.Objects["z"], b.Objects["y"] = &Object{}, &Object{}
	b.reindex()
	require.Equal(t, []string{"y", "z"}, b.keys)

	first := &MultipartUpload{ID: "1", Key: "k", Initiated: stamped(time.Unix(2, 0))}
	second := &MultipartUpload{ID: "2", Key: "k", Initiated: stamped(time.Unix(1, 0))}
	b.addUpload(first)
	b.addUpload(second)
	b.addUpload(first)
	require.Equal(t, []*MultipartUpload{second, first}, b.uploadOrder)

	b.removeUpload("2")
	b.removeUpload("absent")
	require.Equal(t, []*MultipartUpload{first}, b.uploadOrder)
	requireIndexConsistent(t, b)

	b.clearUploads()
	require.Empty(t, b.uploadOrder)
	require.Empty(t, b.Uploads)
}
