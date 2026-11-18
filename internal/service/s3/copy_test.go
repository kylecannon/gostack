package s3_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	destPath  = "/bucket/copy"
	sourceRef = "bucket/key"
)

// copyTo copies the object at source to destPath, with extra headers.
func copyTo(s *server, source string, header map[string]string) reply {
	h := map[string]string{headerCopySource: source}
	maps.Copy(h, header)

	return s.do(call{method: http.MethodPut, target: destPath, region: regionUSEast1, header: h})
}

func TestCopyObject(t *testing.T) {
	source := map[string]string{
		headerContentType: contentTypePlain, "Content-Language": "de", "Cache-Control": "max-age=60",
		headerMetaColor: colorBlue, headerTagging: "a=1",
	}

	t.Run("the directive COPY keeps the source's metadata, tags and ETag", func(t *testing.T) {
		s := newServer(t)
		etag := seed(t, s, source)

		got := copyTo(s, sourceRef, nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Contains(t, string(got.body), "<ETag>"+etag+"</ETag>")
		assert.Contains(t, string(got.body), "<LastModified>")

		read := get(s, http.MethodGet, destPath, nil)
		require.Equal(t, http.StatusOK, read.status)
		assert.Equal(t, objectBody, string(read.body))
		assert.Equal(t, etag, read.header.Get(headerETag))
		assert.Equal(t, contentTypePlain, read.header.Get(headerContentType))
		assert.Equal(t, "de", read.header.Get("Content-Language"))
		assert.Equal(t, colorBlue, read.header.Get(headerMetaColor))
		assert.Equal(t, "1", read.header.Get("X-Amz-Tagging-Count"))

		// The source is untouched.
		assert.Equal(t, http.StatusOK, get(s, http.MethodHead, objectPath, nil).status)
	})

	t.Run("the directive REPLACE takes the request's metadata and drops the source's", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, source)

		got := copyTo(s, sourceRef, map[string]string{
			headerMetadataDirective: valueReplace, headerContentType: "text/csv", "X-Amz-Meta-Shape": "round",
		})
		require.Equal(t, http.StatusOK, got.status, string(got.body))

		read := get(s, http.MethodHead, destPath, nil)
		assert.Equal(t, "text/csv", read.header.Get(headerContentType))
		assert.Equal(t, "round", read.header.Get("X-Amz-Meta-Shape"))
		assert.Empty(t, read.header.Get(headerMetaColor))
		assert.Empty(t, read.header.Get("Content-Language"))
		// Tags follow their own directive.
		assert.Equal(t, "1", get(s, http.MethodGet, destPath, nil).header.Get("X-Amz-Tagging-Count"))
	})

	t.Run("REPLACE without a Content-Type gives the default one", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, source)

		got := copyTo(s, sourceRef, map[string]string{headerMetadataDirective: valueReplace})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, "binary/octet-stream", get(s, http.MethodHead, destPath, nil).header.Get(headerContentType))
	})

	t.Run("the tagging directive REPLACE takes the request's tags", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, source)

		got := copyTo(s, sourceRef, map[string]string{
			"X-Amz-Tagging-Directive": valueReplace, headerTagging: "x=1&y=2&z=3",
		})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, "3", get(s, http.MethodGet, destPath, nil).header.Get("X-Amz-Tagging-Count"))
		assert.Equal(t, colorBlue, get(s, http.MethodHead, destPath, nil).header.Get(headerMetaColor))
	})

	t.Run("an unknown directive is refused", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, source)

		for _, name := range []string{headerMetadataDirective, "X-Amz-Tagging-Directive"} {
			got := copyTo(s, sourceRef, map[string]string{name: "MERGE"})
			require.Equal(t, http.StatusBadRequest, got.status, name)
			assert.Equal(t, "InvalidArgument", got.err(t).Code)
		}
	})

	t.Run("the source may be URL encoded, with a plus for a space and a leading slash", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)
		put(t, s, "/bucket/dir/a%20b%2Bc.txt", "spaced", nil)

		for _, ref := range []string{"bucket/dir/a%20b%2Bc.txt", "/bucket/dir/a+b%2Bc.txt", "bucket/dir/a b%2Bc.txt"} {
			got := copyTo(s, ref, nil)
			require.Equal(t, http.StatusOK, got.status, ref+": "+string(got.body))
			assert.Equal(t, "spaced", string(get(s, http.MethodGet, destPath, nil).body), ref)
		}
	})

	t.Run("the source may be a whole URL, read for its path", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)
		put(t, s, "/bucket/dir/a%20b.txt", "whole", nil)

		for _, ref := range []string{
			"http://localhost.localstack.cloud:4566/bucket/dir/a%20b.txt",
			"https://s3.amazonaws.com/bucket/dir/a+b.txt?versionId=null",
		} {
			got := copyTo(s, ref, nil)
			require.Equal(t, http.StatusOK, got.status, ref+": "+string(got.body))
			assert.Equal(t, "whole", string(get(s, http.MethodGet, destPath, nil).body), ref)
		}
	})

	t.Run("a source with no key is refused", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		for _, ref := range []string{bucketPath[1:], "http://localhost:4566" + bucketPath, "http://localhost:4566"} {
			got := copyTo(s, ref, nil)
			require.Equal(t, http.StatusBadRequest, got.status, ref+": "+string(got.body))
			assert.Equal(t, "InvalidArgument", got.err(t).Code, ref)
			assert.Contains(t, string(got.body), "<ArgumentName>x-amz-copy-source</ArgumentName>", ref)
		}
	})

	t.Run("a missing source key or bucket", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		key := copyTo(s, "bucket/missing", nil)
		require.Equal(t, http.StatusNotFound, key.status)
		assert.Equal(t, codeNoSuchKey, key.err(t).Code)

		bucket := copyTo(s, "nobucket/key", nil)
		require.Equal(t, http.StatusNotFound, bucket.status)
		assert.Equal(t, "NoSuchBucket", bucket.err(t).Code)
	})

	t.Run("a source version of an unversioned bucket", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		bad := copyTo(s, sourceRef+"?versionId=abc", nil)
		require.Equal(t, http.StatusBadRequest, bad.status, string(bad.body))
		assert.Equal(t, "InvalidArgument", bad.err(t).Code)

		null := copyTo(s, sourceRef+"?versionId=null", nil)
		assert.Equal(t, http.StatusOK, null.status, string(null.body))
	})

	t.Run("a destination key over 1024 bytes", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := s.do(call{
			method: http.MethodPut, target: "/bucket/" + strings.Repeat("k", 1025), region: regionUSEast1,
			header: map[string]string{headerCopySource: sourceRef},
		})
		require.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, "KeyTooLongError", got.err(t).Code)
	})

	t.Run("an archived source", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, map[string]string{headerStorageClass: "DEEP_ARCHIVE"})

		got := copyTo(s, sourceRef, nil)
		require.Equal(t, http.StatusForbidden, got.status, string(got.body))
		assert.Equal(t, "InvalidObjectState", got.err(t).Code)
	})

	t.Run("the storage class is the request's, not the source's", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, map[string]string{headerStorageClass: classStandardIA})

		got := copyTo(s, sourceRef, nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Empty(t, get(s, http.MethodHead, destPath, nil).header.Get(headerStorageClass))

		ia := copyTo(s, sourceRef, map[string]string{headerStorageClass: "ONEZONE_IA"})
		require.Equal(t, http.StatusOK, ia.status, string(ia.body))
		assert.Equal(t, "ONEZONE_IA", get(s, http.MethodHead, destPath, nil).header.Get(headerStorageClass))
	})

	t.Run("a copy keeps its own body when the source goes", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		require.Equal(t, http.StatusOK, copyTo(s, sourceRef, nil).status)
		require.Equal(t, http.StatusNoContent, get(s, http.MethodDelete, objectPath, nil).status)
		assert.Equal(t, objectBody, string(get(s, http.MethodGet, destPath, nil).body))
	})
}

func TestCopyObjectInPlace(t *testing.T) {
	tests := []struct {
		name       string
		giveHeader map[string]string
		wantStatus int
	}{
		{"nothing changes", nil, http.StatusBadRequest},
		{"the directive COPY changes nothing", map[string]string{headerMetadataDirective: "COPY"}, http.StatusBadRequest},
		{"new metadata", map[string]string{headerMetadataDirective: valueReplace, "X-Amz-Meta-New": "v"}, http.StatusOK},
		{"a storage class", map[string]string{headerStorageClass: classStandardIA}, http.StatusOK},
		{"a server-side encryption", map[string]string{"X-Amz-Server-Side-Encryption": "AES256"}, http.StatusOK},
		{"a website redirect", map[string]string{headerWebsiteRedirect: "/x"}, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			etag := seed(t, s, map[string]string{"X-Amz-Meta-Old": "v"})

			header := map[string]string{headerCopySource: sourceRef}
			maps.Copy(header, tt.giveHeader)

			got := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, header: header})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))

			read := get(s, http.MethodGet, objectPath, nil)
			require.Equal(t, http.StatusOK, read.status)
			assert.Equal(t, objectBody, string(read.body))
			assert.Equal(t, etag, read.header.Get(headerETag))

			if tt.wantStatus == http.StatusBadRequest {
				assert.Equal(t, codeInvalidRequest, got.err(t).Code)
				assert.Contains(t, got.err(t).Message, "copy an object to itself")
			}
		})
	}
}

func TestCopyObjectSourcePreconditions(t *testing.T) {
	s := newServer(t)
	etag := seed(t, s, nil)

	modified := get(s, http.MethodHead, objectPath, nil).header.Get(headerLastModified)

	lastModified, err := time.Parse(http.TimeFormat, modified)
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	earlier := lastModified.Add(-time.Hour).Format(http.TimeFormat)
	now := time.Now().UTC().Format(http.TimeFormat)

	tests := []struct {
		name          string
		giveHeader    map[string]string
		wantCondition string
	}{
		{"If-Match with the ETag", map[string]string{"X-Amz-Copy-Source-If-Match": etag}, ""},
		{"If-Match with another", map[string]string{"X-Amz-Copy-Source-If-Match": etagOther}, "x-amz-copy-source-If-Match"},
		{"If-None-Match with another", map[string]string{"X-Amz-Copy-Source-If-None-Match": etagOther}, ""},
		{
			"If-None-Match with the ETag",
			map[string]string{"X-Amz-Copy-Source-If-None-Match": etag},
			"x-amz-copy-source-If-None-Match",
		},
		{"If-Unmodified-Since after the change", map[string]string{"X-Amz-Copy-Source-If-Unmodified-Since": now}, ""},
		{
			"If-Unmodified-Since before the change",
			map[string]string{"X-Amz-Copy-Source-If-Unmodified-Since": earlier},
			"x-amz-copy-source-If-Unmodified-Since",
		},
		{"If-Modified-Since before the change", map[string]string{"X-Amz-Copy-Source-If-Modified-Since": earlier}, ""},
		{
			"If-Modified-Since after the change",
			map[string]string{"X-Amz-Copy-Source-If-Modified-Since": now},
			"x-amz-copy-source-If-Modified-Since",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := copyTo(s, sourceRef, tt.giveHeader)

			if tt.wantCondition == "" {
				assert.Equal(t, http.StatusOK, got.status, string(got.body))

				return
			}

			require.Equal(t, http.StatusPreconditionFailed, got.status, string(got.body))
			assert.Equal(t, codePrecondition, got.err(t).Code)
			assert.Contains(t, string(got.body), "<Condition>"+tt.wantCondition+"</Condition>")
		})
	}
}

func TestCopyObjectDestinationConditions(t *testing.T) {
	t.Run("If-None-Match * on an existing destination", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		first := copyTo(s, sourceRef, map[string]string{headerIfNoneMatch: "*"})
		require.Equal(t, http.StatusOK, first.status, string(first.body))

		second := copyTo(s, sourceRef, map[string]string{headerIfNoneMatch: "*"})
		require.Equal(t, http.StatusPreconditionFailed, second.status, string(second.body))
		assert.Contains(t, string(second.body), "<Condition>If-None-Match</Condition>")
	})

	t.Run("If-Match against the destination's ETag", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)
		destETag := put(t, s, destPath, "other body", nil)

		wrong := copyTo(s, sourceRef, map[string]string{headerIfMatch: `"nope"`})
		require.Equal(t, http.StatusPreconditionFailed, wrong.status, string(wrong.body))

		right := copyTo(s, sourceRef, map[string]string{headerIfMatch: destETag})
		require.Equal(t, http.StatusOK, right.status, string(right.body))
		assert.Equal(t, objectBody, string(get(s, http.MethodGet, destPath, nil).body))
	})

	t.Run("If-Match against a missing destination", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := copyTo(s, sourceRef, map[string]string{headerIfMatch: `"abc"`})
		require.Equal(t, http.StatusNotFound, got.status, string(got.body))
		assert.Equal(t, codeNoSuchKey, got.err(t).Code)
	})

	t.Run("values S3 does not implement", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		for _, header := range []map[string]string{
			{headerIfMatch: "*"},
			{headerIfNoneMatch: etagX},
			{headerIfMatch: etagX, headerIfNoneMatch: "*"},
		} {
			got := copyTo(s, sourceRef, header)
			assert.Equal(t, http.StatusNotImplemented, got.status, "%v: %s", header, got.body)
		}
	})
}

func TestCopyObjectBetweenBuckets(t *testing.T) {
	s := newServer(t)
	seed(t, s, map[string]string{headerMetaColor: colorBlue})

	other := s.do(call{method: http.MethodPut, target: "/other", region: regionUSEast1})
	require.Equal(t, http.StatusOK, other.status, string(other.body))

	got := s.do(call{
		method: http.MethodPut, target: "/other/" + url.PathEscape("copied key"), region: regionUSEast1,
		header: map[string]string{headerCopySource: sourceRef},
	})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	read := get(s, http.MethodGet, "/other/"+url.PathEscape("copied key"), nil)
	assert.Equal(t, objectBody, string(read.body))
	assert.Equal(t, colorBlue, read.header.Get(headerMetaColor))
}

// TestCopyOfAMultipartObjectIsWhole checks that a copy of an object completed
// from parts is a whole object: the MD5 ETag of its bytes, not the source's
// "-N" one, and no parts to read by partNumber.
func TestCopyOfAMultipartObjectIsWhole(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	first, second := bytes.Repeat([]byte("a"), partSize), []byte("tail")
	id := startUpload(t, s, multipartKey, nil)
	etags := []string{putPart(t, s, id, 1, first, nil), putPart(t, s, id, 2, second, nil)}
	require.Equal(t, http.StatusOK, complete(s, id, completeBody(etags), nil).status)

	got := copyTo(s, "bucket/big", nil)
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	sum := md5.Sum(slices.Concat(first, second))
	assert.Contains(t, string(got.body), hex.EncodeToString(sum[:]))
	assert.NotContains(t, string(got.body), "-2")

	read := get(s, http.MethodGet, destPath+"?partNumber=2", nil)
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, read.status, "a copy has one part")
}

func TestCopyOfASpilledObjectWithAnotherChecksum(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	body := strings.Repeat("0123456789abcdef", (2<<20)/16)
	put(t, s, objectPath, body, nil)

	got := copyTo(s, sourceRef, map[string]string{"X-Amz-Checksum-Algorithm": "SHA256"})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Contains(t, string(got.body), "<ChecksumSHA256>")

	read := get(s, http.MethodGet, destPath, map[string]string{headerChecksumMode: checksumModeOn})
	require.Equal(t, http.StatusOK, read.status)
	assert.Equal(t, body, string(read.body))
	assert.NotEmpty(t, read.header.Get("X-Amz-Checksum-Sha256"))
	assert.Empty(t, read.header.Get("X-Amz-Checksum-Crc64nvme"))
}
