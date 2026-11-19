package s3_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	listingBucketPath = "/bucket"
	listingPart3Len   = 1 << 10
	keyAOne           = "a/one"
	keyBig            = "big"
	keyATwo           = "a/two"
	keyDeep           = "dir/deep/x"
)

// listedVersion is a Version or DeleteMarker entry of ListObjectVersions with
// the key it belongs to.
type listedVersion struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

type listedVersions struct {
	Versions []listedVersion `xml:"Version"`
	Markers  []listedVersion `xml:"DeleteMarker"`
}

// listingState saves a versioned bucket whose every listing has something to
// show: keys under two prefixes, a key with two versions, a key under a delete
// marker, an object completed from parts 1 and 3, and two uploads in progress.
// The bucket has to be listable from the restored state, not only readable by
// key, so a restore that leaves a sorted index empty fails here.
func listingState(t *testing.T, p *persisted) {
	t.Helper()

	createBucket(t, p.server)
	p.put(versionsPath, versioningBody("Enabled"))

	for _, key := range []string{keyAOne, keyAOne, keyATwo, "b", keyDeep} {
		p.put(listingBucketPath+"/"+key, []byte("body of "+key))
	}

	require.Equal(t, http.StatusNoContent,
		p.do(call{method: http.MethodDelete, target: listingBucketPath + "/b", region: regionUSEast1}).status)

	// An object whose parts are numbered 1 and 3: SQ-152 reads a part by its own
	// number, so the saved Parts have to keep them.
	id := startUpload(t, p.server, multipartKey, nil)
	partOne, partThree := randomBytes(t, partOneLen), randomBytes(t, listingPart3Len)
	manifest := fmt.Sprintf(
		"<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>"+
			"<Part><PartNumber>3</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>",
		putPart(t, p.server, id, 1, partOne, nil), putPart(t, p.server, id, 3, partThree, nil))

	done := complete(p.server, id, []byte(manifest), nil)
	require.Equal(t, http.StatusOK, done.status, string(done.body))

	// Created out of key order, so the saved order is not the listing order.
	for _, key := range []string{"up/two", "up/one"} {
		started := startUpload(t, p.server, listingBucketPath+"/"+key, nil)
		got := p.do(call{
			method: http.MethodPut, target: fmt.Sprintf("%s/%s?partNumber=1&uploadId=%s", listingBucketPath, key, started),
			region: regionUSEast1, body: []byte("part of " + key),
		})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
	}

	p.flush()
}

// assertListed checks every listing against what listingState saved.
func assertListed(t *testing.T, p *persisted) {
	t.Helper()

	keys := func(target string) []string { return keysOf(decodeXML[listResult](t, p.get(target))) }

	assert.Equal(t, []string{keyAOne, keyATwo, keyBig, keyDeep}, keys(listingBucketPath+"?list-type=2"),
		"ListObjectsV2 lists every key, and not the one under a delete marker")
	assert.Equal(t, []string{keyAOne, keyATwo}, keys(listingBucketPath+"?list-type=2&prefix=a/"))
	assert.Equal(t, []string{keyDeep}, keys(listingBucketPath+"?list-type=2&start-after=big"))

	grouped := decodeXML[listResult](t, p.get(listingBucketPath+"?list-type=2&delimiter=/"))
	assert.Equal(t, []listPrefix{{"a/"}, {"dir/"}}, grouped.CommonPrefixes)
	assert.Equal(t, []string{keyBig}, keysOf(grouped))

	versions := decodeXML[listedVersions](t, p.get(listingBucketPath+"?versions"))
	byKey := map[string]int{}

	for _, v := range versions.Versions {
		byKey[v.Key]++
	}

	assert.Equal(t, map[string]int{keyAOne: 2, keyATwo: 1, "b": 1, keyBig: 1, keyDeep: 1}, byKey,
		"ListObjectVersions lists every version of every key")
	require.Len(t, versions.Markers, 1)
	assert.Equal(t, "b", versions.Markers[0].Key)

	uploads := decodeXML[listedUploads](t, p.get(listingBucketPath+"?uploads"))
	require.Len(t, uploads.Uploads, 2)
	assert.Equal(t, "up/one", uploads.Uploads[0].Key, "ListMultipartUploads lists by key")
	assert.Equal(t, "up/two", uploads.Uploads[1].Key)

	// The restored Parts keep the numbers the upload was completed with.
	third := p.get(multipartKey + "?partNumber=3")
	require.Equal(t, http.StatusPartialContent, third.status, string(third.body))
	assert.Len(t, third.body, listingPart3Len)
	assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", partOneLen, partOneLen+listingPart3Len-1, partOneLen+listingPart3Len),
		third.header.Get("Content-Range"))

	two := p.get(multipartKey + "?partNumber=2")
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, two.status, "no part was numbered 2")
	assert.Equal(t, "InvalidPartNumber", two.err(t).Code)
}

// TestPersistedS3ListsEverythingAfterARestart starts over on a saved
// directory: ListObjectsV2, ListObjectVersions and ListMultipartUploads return
// what was saved, because Restore rebuilds the sorted indexes the listings walk.
func TestPersistedS3ListsEverythingAfterARestart(t *testing.T) {
	dir := t.TempDir()
	first := startPersisted(t, dir)
	listingState(t, first)
	assertListed(t, first)

	second := startPersisted(t, dir)
	assertListed(t, second)

	// The restored bucket takes writes at the right place in the index.
	second.put(listingBucketPath+"/a/three", []byte("three"))

	got := decodeXML[listResult](t, second.get(listingBucketPath+"?list-type=2&prefix=a/"))
	assert.Equal(t, []string{keyAOne, "a/three", keyATwo}, keysOf(got))

	removed := second.do(call{
		method: http.MethodDelete, target: listingBucketPath + "/up/one?uploadId=" + uploadOf(t, second, "up/one"),
		region: regionUSEast1,
	})
	require.Equal(t, http.StatusNoContent, removed.status, string(removed.body))

	uploads := decodeXML[listedUploads](t, second.get(listingBucketPath+"?uploads"))
	require.Len(t, uploads.Uploads, 1)
	assert.Equal(t, "up/two", uploads.Uploads[0].Key)
}

// TestPersistedS3ListsEverythingAfterALiveLoad loads the saved state into a
// running S3, as /_gostack/state/load does, after the live state moved on: the
// listings show the saved state, not the live one and not an empty bucket.
func TestPersistedS3ListsEverythingAfterALiveLoad(t *testing.T) {
	p := startPersisted(t, t.TempDir())
	listingState(t, p)

	p.put(listingBucketPath+"/zz", []byte("after the save"))
	require.Equal(t, http.StatusNoContent,
		p.do(call{method: http.MethodDelete, target: listingBucketPath + "/a/two", region: regionUSEast1}).status)

	require.NoError(t, p.eng.Load(t.Context()))

	assertListed(t, p)
}

func keysOf(got listResult) []string {
	out := make([]string, 0, len(got.Contents))
	for _, c := range got.Contents {
		out = append(out, c.Key)
	}

	return out
}

// uploadOf is the id of the upload in progress for key.
func uploadOf(t *testing.T, p *persisted, key string) string {
	t.Helper()

	got := decodeXML[listedUploads](t, p.get(listingBucketPath+"?uploads&prefix="+key))
	require.Len(t, got.Uploads, 1, key)

	return got.Uploads[0].UploadID
}
