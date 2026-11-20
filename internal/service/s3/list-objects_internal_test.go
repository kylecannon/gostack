package s3

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionID is a version id the way versioned buckets write them: a sequence in
// the leading bytes that grows, in the alphabet of a continuation token.
const (
	dirPrefix = "dir/"
	keyA1     = "a/1"
	keyA2     = "a/2"
)

func versionID(sequence int) string {
	raw := make([]byte, 24)
	for i := range 6 {
		raw[5-i] = byte(sequence >> (8 * i))
	}

	return versionIDEncoding.EncodeToString(raw)
}

// chainOf builds a key whose versions are oldest first. A negative sequence is
// a delete marker.
func chainOf(key string, sequences ...int) listed {
	k := listed{key: key}

	for _, sequence := range sequences {
		k.versions = append(k.versions, &Version{
			ID: versionID(abs(sequence)), DeleteMarker: sequence < 0, LastModified: stamped(time.Unix(int64(abs(sequence)), 0)),
		})
	}

	if len(k.versions) > 0 {
		k.current = k.versions[len(k.versions)-1]
	}

	return k
}

func abs(n int) int {
	if n < 0 {
		return -n
	}

	return n
}

// entryIDs lists the emitted versions as key@sequence, newest first, in
// ListObjectVersions order.
func entryIDs(pg versionPage) []string {
	ids := make([]string, 0, len(pg.entries))

	for _, e := range pg.entries {
		ids = append(ids, e.key+"@"+strconv.Itoa(sequenceOf(e.version.ID)))
	}

	return ids
}

func sequenceOf(id string) int {
	raw, err := versionIDEncoding.DecodeString(id)
	if err != nil {
		return -1
	}

	sequence := 0
	for _, c := range raw[:6] {
		sequence = sequence<<8 | int(c)
	}

	return sequence
}

func Test_walkVersions_pagesEveryVersionOnce(t *testing.T) {
	keys := []listed{chainOf("a", 1, 2, 3), chainOf("b", 4, -5), chainOf("c", 6)}

	var all []string

	w := walk{limit: 2}

	for {
		// The snapshot leaves out the keys before the marker.
		from := 0
		for from < len(keys) && keys[from].key < w.marker {
			from++
		}

		pg := walkVersions(keys[from:], w)
		all = append(all, entryIDs(pg)...)

		if !pg.truncated {
			break
		}

		w.marker, w.versionMarker = pg.last.key, pg.last.versionID
	}

	assert.Equal(t, []string{"a@3", "a@2", "a@1", "b@5", "b@4", "c@6"}, all)
}

func Test_walkVersions_resumesAfterAVersionThatWasDeleted(t *testing.T) {
	keys := []listed{chainOf("a", 1, 3, 5)}

	// Version 4 never existed, as if it was deleted: the page resumes at the
	// first version older than it.
	pg := walkVersions(keys, walk{limit: 10, marker: "a", versionMarker: versionID(4)})

	assert.Equal(t, []string{"a@3", "a@1"}, entryIDs(pg))
}

func Test_walkVersions_commonPrefixes(t *testing.T) {
	keys := []listed{chainOf("dir/a", 1, 2), chainOf("dir/b", 3), chainOf("top", 4)}

	pg := walkVersions(keys, walk{limit: 10, delimiter: "/"})
	assert.Equal(t, []string{dirPrefix}, pg.prefixes)
	assert.Equal(t, []string{"top@4"}, entryIDs(pg))
	assert.False(t, pg.truncated)

	pg = walkVersions(keys, walk{limit: 1, delimiter: "/"})
	assert.Equal(t, []string{dirPrefix}, pg.prefixes)
	assert.True(t, pg.truncated)
	assert.Equal(t, dirPrefix, pg.last.key)
	assert.Empty(t, pg.last.versionID)

	// Resuming from the common prefix skips every key under it.
	pg = walkVersions(keys, walk{limit: 10, delimiter: "/", marker: dirPrefix})
	assert.Empty(t, pg.prefixes)
	assert.Equal(t, []string{"top@4"}, entryIDs(pg))
}

func Test_walkVersions_deleteMarkersAreEntriesToo(t *testing.T) {
	pg := walkVersions([]listed{chainOf("a", 1, -2)}, walk{limit: 10})

	require.Len(t, pg.entries, 2)
	assert.True(t, pg.entries[0].version.DeleteMarker)
	assert.True(t, pg.entries[0].latest)
	assert.False(t, pg.entries[1].latest)
}

func Test_walkKeys(t *testing.T) {
	keys := make([]listed, 0, 6)
	for _, key := range []string{keyA1, keyA2, "b", "c/1", "d", "e"} {
		keys = append(keys, chainOf(key, 1))
	}

	tests := []struct {
		name         string
		give         walk
		wantKeys     []string
		wantPrefixes []string
		wantTrunc    bool
		wantLast     string
		wantResume   string
	}{
		{
			name: "no delimiter stops at the limit", give: walk{limit: 2},
			wantKeys: []string{keyA1, keyA2}, wantTrunc: true, wantLast: keyA2, wantResume: "b",
		},
		{
			name: "a limit that fits is not truncated", give: walk{limit: 6},
			wantKeys: []string{keyA1, keyA2, "b", "c/1", "d", "e"}, wantLast: "e",
		},
		{
			name: "common prefixes count once against the limit", give: walk{limit: 3, delimiter: "/"},
			wantKeys: []string{"b"}, wantPrefixes: []string{"a/", "c/"}, wantTrunc: true, wantLast: "c/", wantResume: "d",
		},
		{
			name: "a marker inside a prefix skips the prefix", give: walk{limit: 10, delimiter: "/", marker: keyA1},
			wantKeys: []string{"b", "d", "e"}, wantPrefixes: []string{"c/"}, wantLast: "e",
		},
		{
			name: "limit zero returns nothing and is not truncated", give: walk{limit: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg := walkKeys(keys, tt.give)

			var got []string
			for _, k := range pg.keys {
				got = append(got, k.key)
			}

			assert.Equal(t, tt.wantKeys, got)
			assert.Equal(t, tt.wantPrefixes, pg.prefixes)
			assert.Equal(t, tt.wantTrunc, pg.truncated)
			assert.Equal(t, tt.wantLast, pg.last)
			assert.Equal(t, tt.wantResume, pg.resume)
		})
	}
}

func Test_quote(t *testing.T) {
	tests := []struct {
		give string
		want string
	}{
		{give: "plain/key-1_2.3~", want: "plain/key-1_2.3~"},
		{give: "date=2026-01-01/", want: "date%3D2026-01-01/"},
		{give: "a b", want: "a%20b"},
		{give: "file%2Fname", want: "file%252Fname"},
		{give: "é", want: "%C3%A9"},
		{give: "a/\xf0\x9f\x98\x80/", want: "a/%F0%9F%98%80/"},
		{give: "", want: ""},
		// What CompleteMultipartUpload's Location, a ListMultipartUploads
		// EncodingType and an event's key used to get from quoteKey, which was
		// a second copy of this function.
		{give: "dir/a+b?c#d%e", want: "dir/a%2Bb%3Fc%23d%25e"},
		{give: "\xff\x00", want: "%FF%00"},
	}

	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			assert.Equal(t, tt.want, quote(tt.give))
		})
	}
}

func Test_decodeToken(t *testing.T) {
	token := tokenEncoding.EncodeToString([]byte("some/key?"))

	got, err := decodeToken(token)
	require.NoError(t, err)
	assert.Equal(t, "some/key?", got)

	_, err = decodeToken("not a token!")
	require.Error(t, err)
}
