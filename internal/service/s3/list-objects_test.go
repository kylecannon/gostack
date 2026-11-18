package s3_test

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bucketPath      = "/bucket"
	classStandardIA = "STANDARD_IA"
	keyFile1        = "folder/file1"
	restoreDays30   = `<RestoreRequest><Days>30</Days></RestoreRequest>`
)

// listResult is a ListObjects or ListObjectsV2 result, with the members these
// tests read. It takes any root element: the root's name is the wire's matter,
// not the listing's.
type listResult struct {
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	Delimiter             string        `xml:"Delimiter"`
	Marker                *string       `xml:"Marker"`
	NextMarker            string        `xml:"NextMarker"`
	MaxKeys               int           `xml:"MaxKeys"`
	KeyCount              int           `xml:"KeyCount"`
	IsTruncated           bool          `xml:"IsTruncated"`
	EncodingType          string        `xml:"EncodingType"`
	ContinuationToken     string        `xml:"ContinuationToken"`
	NextContinuationToken string        `xml:"NextContinuationToken"`
	StartAfter            string        `xml:"StartAfter"`
	Contents              []listContent `xml:"Contents"`
	CommonPrefixes        []listPrefix  `xml:"CommonPrefixes"`
}

type listContent struct {
	Key          string     `xml:"Key"`
	ETag         string     `xml:"ETag"`
	Size         int64      `xml:"Size"`
	StorageClass string     `xml:"StorageClass"`
	LastModified string     `xml:"LastModified"`
	Owner        *listOwner `xml:"Owner"`
}

type listOwner struct {
	ID string `xml:"ID"`
}

type listPrefix struct {
	Prefix string `xml:"Prefix"`
}

func (l listResult) keys() []string {
	keys := make([]string, 0, len(l.Contents))
	for _, c := range l.Contents {
		keys = append(keys, c.Key)
	}

	return keys
}

func (l listResult) prefixes() []string {
	prefixes := make([]string, 0, len(l.CommonPrefixes))
	for _, c := range l.CommonPrefixes {
		prefixes = append(prefixes, c.Prefix)
	}

	return prefixes
}

// bucketWith creates the bucket and puts an empty object under each key.
func bucketWith(t *testing.T, s *server, keys ...string) {
	t.Helper()

	createBucket(t, s)

	for _, key := range keys {
		got := s.do(call{method: http.MethodPut, target: bucketPath + "/" + url.PathEscape(key), region: regionUSEast1})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
	}
}

func (s *server) list(t *testing.T, query string) listResult {
	t.Helper()

	got := s.do(call{method: http.MethodGet, target: bucketPath + "?" + query, region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	var result listResult
	require.NoError(t, xml.Unmarshal(got.body, &result), string(got.body))

	return result
}

func TestListObjectsIsOrderedByByte(t *testing.T) {
	s := newServer(t)
	bucketWith(t, s, "b", "a/2", "é", "a/1", "Z", "~")

	got := s.do(call{method: http.MethodGet, target: bucketPath, region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status)

	var result listResult
	require.NoError(t, xml.Unmarshal(got.body, &result))
	assert.Equal(t, []string{"Z", "a/1", "a/2", "b", "~", "é"}, result.keys())
	assert.Equal(t, 1000, result.MaxKeys)
	assert.False(t, result.IsTruncated)
	require.NotNil(t, result.Marker, "Marker is sent, empty")
	assert.Empty(t, *result.Marker)

	first := result.Contents[0]
	assert.Equal(t, `"d41d8cd98f00b204e9800998ecf8427e"`, first.ETag)
	assert.Equal(t, "STANDARD", first.StorageClass)
	require.NotNil(t, first.Owner, "ListObjects always carries the owner")
	assert.NotEmpty(t, first.LastModified)
}

func TestListObjectsPagesThroughCommonPrefixesWithMarkers(t *testing.T) {
	s := newServer(t)
	bucketWith(t, s, "folder/aSubfolder/subFile1", "folder/aSubfolder/subFile2", keyFile1, "folder/file2")

	query := "prefix=folder/&delimiter=/&max-keys=1"

	first := s.list(t, query)
	assert.Equal(t, []string{"folder/aSubfolder/"}, first.prefixes())
	assert.Empty(t, first.keys())
	assert.True(t, first.IsTruncated)
	assert.Equal(t, "folder/aSubfolder/", first.NextMarker)

	second := s.list(t, query+"&marker="+url.QueryEscape(first.NextMarker))
	assert.Equal(t, []string{keyFile1}, second.keys())
	assert.True(t, second.IsTruncated)
	assert.Equal(t, keyFile1, second.NextMarker)

	third := s.list(t, query+"&marker="+url.QueryEscape(second.NextMarker))
	assert.Equal(t, []string{"folder/file2"}, third.keys())
	assert.False(t, third.IsTruncated)
	assert.Empty(t, third.NextMarker)

	// A marker on a key inside the common prefix skips the rest of the prefix.
	inside := s.list(t, query+"&marker=folder/aSubfolder/subFile1")
	assert.Empty(t, inside.prefixes())
	assert.Equal(t, []string{keyFile1}, inside.keys())
}

func TestListObjectsWithoutDelimiterHasNoNextMarker(t *testing.T) {
	s := newServer(t)
	bucketWith(t, s, "a", "b", "c")

	got := s.list(t, "max-keys=2")
	assert.True(t, got.IsTruncated)
	assert.Equal(t, []string{"a", "b"}, got.keys())
	assert.Empty(t, got.NextMarker, "the client continues from the last key")

	assert.False(t, s.list(t, "max-keys=2&prefix=c").IsTruncated)
	assert.False(t, s.list(t, "max-keys=3").IsTruncated)
	assert.Empty(t, s.list(t, "max-keys=0").keys())
	assert.False(t, s.list(t, "max-keys=0").IsTruncated)
	assert.Equal(t, 1000, s.list(t, "max-keys=5000").MaxKeys)
}

func TestListObjectsV2Pages(t *testing.T) {
	s := newServer(t)

	keys := make([]string, 0, 12)
	for i := range 12 {
		keys = append(keys, "test_"+strconv.Itoa(i))
	}

	bucketWith(t, s, keys...)

	first := s.list(t, "list-type=2&max-keys=5")
	assert.Equal(t, []string{"test_0", "test_1", "test_10", "test_11", "test_2"}, first.keys())
	assert.Equal(t, 5, first.KeyCount)
	assert.True(t, first.IsTruncated)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("test_3")), strings.NewReplacer("_", "/", ".", "+").
		Replace(first.NextContinuationToken))

	rest := s.list(t, "list-type=2&continuation-token="+url.QueryEscape(first.NextContinuationToken))
	assert.Equal(t, []string{"test_3", "test_4", "test_5", "test_6", "test_7", "test_8", "test_9"}, rest.keys())
	assert.False(t, rest.IsTruncated)
	assert.Equal(t, first.NextContinuationToken, rest.ContinuationToken)
	assert.Empty(t, rest.NextContinuationToken)
	assert.Nil(t, rest.Contents[0].Owner, "V2 sends the owner only for FetchOwner")

	after := s.list(t, "list-type=2&start-after=test_7&max-keys=2")
	assert.Equal(t, []string{"test_8", "test_9"}, after.keys())
	assert.False(t, after.IsTruncated)
	assert.Equal(t, "test_7", after.StartAfter)

	// The continuation token wins over StartAfter, and echoes no StartAfter.
	both := s.list(t, "list-type=2&start-after=test_7&continuation-token="+url.QueryEscape(first.NextContinuationToken))
	assert.Equal(t, rest.keys(), both.keys())
	assert.Empty(t, both.StartAfter)

	owner := s.list(t, "list-type=2&fetch-owner=true&max-keys=1")
	require.NotNil(t, owner.Contents[0].Owner)
}

func TestListObjectsV2CommonPrefixesCountOnceAndContinueAfterTheGroup(t *testing.T) {
	s := newServer(t)
	bucketWith(t, s, "folder/aSubfolder/subFile1", "folder/aSubfolder/subFile2", keyFile1, "folder/file2")

	query := "list-type=2&prefix=folder/&delimiter=/&max-keys=1"

	first := s.list(t, query)
	assert.Equal(t, []string{"folder/aSubfolder/"}, first.prefixes())
	assert.Equal(t, 1, first.KeyCount)
	assert.True(t, first.IsTruncated)

	second := s.list(t, query+"&continuation-token="+url.QueryEscape(first.NextContinuationToken))
	assert.Equal(t, []string{keyFile1}, second.keys())

	third := s.list(t, query+"&continuation-token="+url.QueryEscape(second.NextContinuationToken))
	assert.Equal(t, []string{"folder/file2"}, third.keys())
	assert.Empty(t, third.NextContinuationToken)
}

func TestListObjectsEncodesWhatTheClientAsksFor(t *testing.T) {
	s := newServer(t)
	bucketWith(t, s, "date=2026-01-01/", "date=2026-02-01/", "test key", "a%b")

	got := s.list(t, "list-type=2&encoding-type=url&delimiter=%2F&start-after=a%25b")
	assert.Equal(t, "url", got.EncodingType)
	assert.Equal(t, "/", got.Delimiter, "a slash stays a slash")
	assert.Equal(t, []string{"date%3D2026-01-01/", "date%3D2026-02-01/"}, got.prefixes())
	assert.Equal(t, []string{"test%20key"}, got.keys())
	assert.Equal(t, "a%25b", got.StartAfter)

	assert.Equal(t, "%252F", s.list(t, "list-type=2&encoding-type=url&delimiter=%252F").Delimiter,
		"a delimiter of percent-2F is quoted as it is")

	// Without it the keys come as they are.
	plain := s.list(t, "list-type=2")
	assert.Equal(t, []string{"a%b", "date=2026-01-01/", "date=2026-02-01/", "test key"}, plain.keys())
	assert.Empty(t, plain.EncodingType)

	v1 := s.list(t, "encoding-type=url&prefix=test+key&marker=a%25b")
	assert.Equal(t, "test%20key", v1.Prefix)
	require.NotNil(t, v1.Marker)
	assert.Equal(t, "a%25b", *v1.Marker)
}

func TestListObjectsRefusesWhatIsInvalid(t *testing.T) {
	tests := []struct {
		name       string
		giveQuery  string
		wantStatus int
		wantCode   string
		wantArg    string
	}{
		{
			name: "an encoding type other than url", giveQuery: "encoding-type=base64",
			wantStatus: http.StatusBadRequest, wantCode: codeInvalidArgument, wantArg: "encoding-type",
		},
		{
			name: "an empty encoding type", giveQuery: "encoding-type=",
			wantStatus: http.StatusBadRequest, wantCode: codeInvalidArgument, wantArg: "encoding-type",
		},
		{
			name: "an empty continuation token", giveQuery: "list-type=2&continuation-token=",
			wantStatus: http.StatusBadRequest, wantCode: codeInvalidArgument, wantArg: "continuation-token",
		},
		{
			name: "a continuation token that is not one", giveQuery: "list-type=2&continuation-token=!!",
			wantStatus: http.StatusBadRequest, wantCode: codeInvalidArgument, wantArg: "continuation-token",
		},
		{
			name: "a negative max-keys", giveQuery: "max-keys=-1",
			wantStatus: http.StatusBadRequest, wantCode: codeInvalidArgument, wantArg: "maxKeys",
		},
		{
			name: "a version id marker without a key marker", giveQuery: "versions&version-id-marker=abc",
			wantStatus: http.StatusBadRequest, wantCode: codeInvalidArgument, wantArg: "version-id-marker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			got := s.do(call{method: http.MethodGet, target: bucketPath + "?" + tt.giveQuery, region: regionUSEast1})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))
			assert.Equal(t, tt.wantCode, got.err(t).Code)
			assert.Contains(t, string(got.body), "<ArgumentName>"+tt.wantArg+"</ArgumentName>")
		})
	}

	s := newServer(t)
	missing := s.do(call{method: http.MethodGet, target: "/nope?list-type=2", region: regionUSEast1})
	assert.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, "NoSuchBucket", missing.err(t).Code)
}

func TestListObjectsOutsideUSEast1CarriesTheBucketRegion(t *testing.T) {
	s := newServer(t)

	created := s.do(call{
		method: http.MethodPut, target: "/bucket-eu", region: regionEUWest1,
		body: []byte(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint>` +
			`</CreateBucketConfiguration>`),
	})
	require.Equal(t, http.StatusOK, created.status, string(created.body))

	got := s.do(call{method: http.MethodGet, target: "/bucket-eu?list-type=2", region: regionEUWest1})
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, regionEUWest1, got.header.Get("X-Amz-Bucket-Region"))

	us := newServer(t)
	createBucket(t, us)

	got = us.do(call{method: http.MethodGet, target: bucketPath, region: regionUSEast1})
	assert.Empty(t, got.header.Get("X-Amz-Bucket-Region"))
}

func TestListObjectVersionsOfABucketThatNeverHadVersioning(t *testing.T) {
	s := newServer(t)
	bucketWith(t, s, "a", "dir/b")

	got := s.do(call{method: http.MethodGet, target: bucketPath + "?versions", region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	body := string(got.body)
	assert.Contains(t, body, "<Key>a</Key>")
	assert.Contains(t, body, "<VersionId>null</VersionId>")
	assert.Contains(t, body, "<IsLatest>true</IsLatest>")
	assert.Regexp(t, `<KeyMarker\s*/>|<KeyMarker></KeyMarker>`, body)
	assert.Regexp(t, `<VersionIdMarker\s*/>|<VersionIdMarker></VersionIdMarker>`, body)
	assert.NotContains(t, body, "<DeleteMarker>")

	paged := s.do(call{method: http.MethodGet, target: bucketPath + "?versions&max-keys=1", region: regionUSEast1})
	require.Equal(t, http.StatusOK, paged.status)
	assert.Contains(t, string(paged.body), "<IsTruncated>true</IsTruncated>")
	assert.Contains(t, string(paged.body), "<NextKeyMarker>a</NextKeyMarker>")
	assert.Contains(t, string(paged.body), "<NextVersionIdMarker>null</NextVersionIdMarker>")

	delimited := s.do(call{
		method: http.MethodGet, target: bucketPath + "?versions&delimiter=%2F", region: regionUSEast1,
	})
	require.Equal(t, http.StatusOK, delimited.status)
	assert.Contains(t, string(delimited.body), "<CommonPrefixes><Prefix>dir/</Prefix></CommonPrefixes>")
}

func TestBucketNamesAreSharedAcrossAccounts(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	other := map[string]string{
		"Authorization": "AWS4-HMAC-SHA256 Credential=000000000001/20250101/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host, Signature=0",
	}

	taken := s.do(call{method: http.MethodPut, target: bucketPath, header: other})
	require.Equal(t, http.StatusConflict, taken.status, string(taken.body))
	assert.Equal(t, "BucketAlreadyExists", taken.err(t).Code)

	mine := s.do(call{method: http.MethodPut, target: bucketPath, region: regionUSEast1})
	assert.Equal(t, http.StatusOK, mine.status, "the owner may create it again in us-east-1")

	notMine := s.do(call{method: http.MethodPut, target: "/other-bucket", header: other})
	require.Equal(t, http.StatusOK, notMine.status, string(notMine.body))

	// Each account lists only its own buckets.
	listed := s.do(call{method: http.MethodGet, target: "/", header: other})
	require.Equal(t, http.StatusOK, listed.status)
	assert.Contains(t, string(listed.body), "<Name>other-bucket</Name>")
	assert.NotContains(t, string(listed.body), "<Name>bucket</Name>")

	// A bucket of another account is still reachable by name.
	assert.Equal(t, http.StatusOK, s.do(call{
		method: http.MethodGet, target: bucketPath + "?list-type=2", header: other,
	}).status)
}

func TestStorageClass(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	put := func(key, class string) reply {
		return s.do(call{
			method: http.MethodPut, target: bucketPath + "/" + key, region: regionUSEast1,
			header: map[string]string{"x-amz-storage-class": class}, body: []byte("body"),
		})
	}

	for _, class := range []string{classStandardIA, "GLACIER_IR", "REDUCED_REDUNDANCY", "INTELLIGENT_TIERING"} {
		require.Equal(t, http.StatusOK, put("k-"+class, class).status, class)
	}

	for _, class := range []string{"OUTPOSTS", "NOPE", "standard"} {
		got := put("bad", class)
		require.Equal(t, http.StatusBadRequest, got.status, class)
		assert.Equal(t, "InvalidStorageClass", got.err(t).Code)
		assert.Contains(t, string(got.body), "<StorageClassRequested>"+class+"</StorageClassRequested>")
	}

	listed := s.list(t, "list-type=2&prefix=k-STANDARD_IA")
	require.Len(t, listed.Contents, 1)
	assert.Equal(t, classStandardIA, listed.Contents[0].StorageClass)

	// A readable class reads back its class; the default sends no header.
	got := s.do(call{method: http.MethodGet, target: bucketPath + "/k-GLACIER_IR", region: regionUSEast1})
	assert.Equal(t, "GLACIER_IR", got.header.Get("X-Amz-Storage-Class"))
}

func TestArchivedObjectsAreRestoredBeforeTheyAreRead(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	require.Equal(t, http.StatusOK, s.do(call{
		method: http.MethodPut, target: bucketPath + "/deep", region: regionUSEast1,
		header: map[string]string{"x-amz-storage-class": "DEEP_ARCHIVE"}, body: []byte("body data"),
	}).status)

	blocked := s.do(call{method: http.MethodGet, target: bucketPath + "/deep", region: regionUSEast1})
	require.Equal(t, http.StatusForbidden, blocked.status, string(blocked.body))
	assert.Equal(t, "InvalidObjectState", blocked.err(t).Code)
	assert.Contains(t, string(blocked.body), "<StorageClass>DEEP_ARCHIVE</StorageClass>")

	head := s.do(call{method: http.MethodHead, target: bucketPath + "/deep", region: regionUSEast1})
	require.Equal(t, http.StatusOK, head.status, "HEAD answers for an archived object")
	assert.Equal(t, "DEEP_ARCHIVE", head.header.Get("X-Amz-Storage-Class"))
	assert.Empty(t, head.header.Get("X-Amz-Restore"))

	restore := []byte(restoreDays30)
	restoreDeep := call{
		method: http.MethodPost, target: bucketPath + "/deep?restore", region: regionUSEast1, body: restore,
	}

	first := s.do(restoreDeep)
	assert.Equal(t, http.StatusAccepted, first.status, string(first.body))

	again := s.do(restoreDeep)
	assert.Equal(t, http.StatusOK, again.status, "a restore of a restored object extends it")

	restored := s.do(call{method: http.MethodHead, target: bucketPath + "/deep", region: regionUSEast1})
	assert.Contains(t, restored.header.Get("X-Amz-Restore"), `ongoing-request="false"`)

	read := s.do(call{method: http.MethodGet, target: bucketPath + "/deep", region: regionUSEast1})
	require.Equal(t, http.StatusOK, read.status)
	assert.Equal(t, "body data", string(read.body))

	// Restoring an object that is not archived is refused.
	require.Equal(t, http.StatusOK, s.do(call{
		method: http.MethodPut, target: bucketPath + "/plain", region: regionUSEast1, body: []byte("x"),
	}).status)

	refused := s.do(call{
		method: http.MethodPost, target: bucketPath + "/plain?restore", region: regionUSEast1, body: restore,
	})
	assert.Equal(t, http.StatusForbidden, refused.status)
	assert.Equal(t, "InvalidObjectState", refused.err(t).Code)
}
