package s3_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	headerETag         = "ETag"
	headerLastModified = "Last-Modified"
	headerIfMatch      = "If-Match"
	headerIfNoneMatch  = "If-None-Match"
	headerCopySource   = "X-Amz-Copy-Source"
	codeNoSuchKey      = "NoSuchKey"
	codePrecondition   = "PreconditionFailed"
	codeNotImplemented = "NotImplemented"
	objectBody         = "0123456789"
)

// seed makes the bucket and an object under objectPath, and returns its ETag.
func seed(t *testing.T, s *server, header map[string]string) string {
	t.Helper()

	createBucket(t, s)

	return put(t, s, objectPath, objectBody, header)
}

func put(t *testing.T, s *server, target, body string, header map[string]string) string {
	t.Helper()

	got := s.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: []byte(body), header: header})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return got.header.Get(headerETag)
}

func get(s *server, method, target string, header map[string]string) reply {
	return s.do(call{method: method, target: target, region: regionUSEast1, header: header})
}

func TestObjectHeadersRoundTrip(t *testing.T) {
	s := newServer(t)
	seed(t, s, map[string]string{
		headerContentType:       contentTypePlain,
		"Content-Language":      "de",
		"Content-Disposition":   `attachment; filename="a.txt"`,
		"Content-Encoding":      encodingIdentity,
		"Cache-Control":         "max-age=60",
		"Expires":               "Wed, 21 Oct 2037 07:28:00 GMT",
		headerWebsiteRedirect:   "/elsewhere",
		"X-Amz-Meta-Mixed-Case": "Value",
		headerTagging:           "a=1&b=2",
	})

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		got := get(s, method, objectPath, nil)
		require.Equal(t, http.StatusOK, got.status, method)

		assert.Equal(t, contentTypePlain, got.header.Get(headerContentType), method)
		assert.Equal(t, "de", got.header.Get("Content-Language"), method)
		assert.Equal(t, `attachment; filename="a.txt"`, got.header.Get("Content-Disposition"), method)
		assert.Equal(t, encodingIdentity, got.header.Get("Content-Encoding"), method)
		assert.Equal(t, "max-age=60", got.header.Get("Cache-Control"), method)
		assert.Equal(t, "Wed, 21 Oct 2037 07:28:00 GMT", got.header.Get("Expires"), method)
		assert.Equal(t, "/elsewhere", got.header.Get(headerWebsiteRedirect), method)
		assert.Equal(t, "Value", got.header.Get("X-Amz-Meta-Mixed-Case"), method)
	}

	got := get(s, http.MethodGet, objectPath, nil)
	assert.Equal(t, "2", got.header.Get("X-Amz-Tagging-Count"))
}

func TestRequestIDHeaders(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	for name, got := range map[string]reply{
		"success": get(s, http.MethodGet, objectPath, nil),
		"error":   get(s, http.MethodGet, "/bucket/missing", nil),
		"head":    get(s, http.MethodHead, objectPath, nil),
	} {
		assert.NotEmpty(t, got.header.Get("X-Amz-Request-Id"), name)
		assert.NotEmpty(t, got.header.Get("X-Amz-Id-2"), name)
		assert.Equal(t, "AmazonS3", got.header.Get("Server"), name)
		// With x-amzn-RequestId botocore would drop HostId from the response
		// metadata, and the recorded AWS snapshots have it.
		assert.Empty(t, got.header.Get("X-Amzn-Requestid"), name)
	}
}

func TestAnObjectsBytesAreNeverRewritten(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	// A body that looks like an S3 response with a root S3 renames.
	body := `<?xml version="1.0"?><DeleteObjectsOutput><Deleted><Key>k</Key></Deleted></DeleteObjectsOutput>`
	put(t, s, objectPath, body, map[string]string{headerContentType: "application/xml"})

	got := get(s, http.MethodGet, objectPath, nil)
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, body, string(got.body))
}

func TestDeleteAnswersNoContentWithoutBodyHeaders(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	got := get(s, http.MethodDelete, objectPath, nil)
	assert.Equal(t, http.StatusNoContent, got.status)
	assert.Empty(t, got.body)
	assert.Empty(t, got.header.Get(headerContentType))
	assert.Empty(t, got.header.Get("Content-Length"))

	// Deleting what is not there answers the same.
	again := get(s, http.MethodDelete, objectPath, nil)
	assert.Equal(t, http.StatusNoContent, again.status)
}

func TestMetadata(t *testing.T) {
	tests := []struct {
		name       string
		giveHeader map[string]string
		wantStatus int
		wantCode   string
		wantHeader map[string]string
	}{
		{
			name:       "an encoded word is stored decoded and sent encoded again",
			giveHeader: map[string]string{headerMetaName: "=?UTF-8?Q?caf=C3=A9?="},
			wantStatus: http.StatusOK,
			wantHeader: map[string]string{headerMetaName: "=?UTF-8?Q?caf=C3=A9?="},
		},
		{
			name:       "an encoded word that holds only safe characters is sent plain",
			giveHeader: map[string]string{headerMetaName: "=?UTF-8?B?YWJj?="},
			wantStatus: http.StatusOK,
			wantHeader: map[string]string{headerMetaName: "abc"},
		},
		{
			name:       "a control character is sent base64 encoded",
			giveHeader: map[string]string{headerMetaName: "=?UTF-8?B?AAEC?="},
			wantStatus: http.StatusOK,
			wantHeader: map[string]string{headerMetaName: "=?UTF-8?B?AAEC?="},
		},
		{
			name:       "plain values pass through",
			giveHeader: map[string]string{headerMetaName: "a b_c"},
			wantStatus: http.StatusOK,
			wantHeader: map[string]string{headerMetaName: "a b_c"},
		},
		{
			name:       "more than 2 KB of metadata is refused",
			giveHeader: map[string]string{"X-Amz-Meta-Big": strings.Repeat("v", 2100)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "MetadataTooLarge",
		},
		{
			name:       "exactly 2 KB is accepted",
			giveHeader: map[string]string{"X-Amz-Meta-Big": strings.Repeat("v", 2048-len("big"))},
			wantStatus: http.StatusOK,
		},
		{
			name:       "a redirect that is not a path or a URL is refused",
			giveHeader: map[string]string{headerWebsiteRedirect: "elsewhere"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
		{
			name:       "a redirect to a URL is accepted",
			giveHeader: map[string]string{headerWebsiteRedirect: "https://example.com/x"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "an unknown storage class is refused",
			giveHeader: map[string]string{headerStorageClass: "COLD"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidStorageClass",
		},
		{
			name:       "a malformed tagging header is refused",
			giveHeader: map[string]string{headerTagging: "a=1&a=2"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			got := s.do(call{
				method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte("x"), header: tt.giveHeader,
			})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))

			if tt.wantCode != "" {
				assert.Equal(t, tt.wantCode, got.err(t).Code)

				return
			}

			read := get(s, http.MethodGet, objectPath, nil)
			require.Equal(t, http.StatusOK, read.status)

			for name, want := range tt.wantHeader {
				assert.Equal(t, want, read.header.Get(name), name)
			}
		})
	}
}

func TestStoredMetadataKeysAreLowerCase(t *testing.T) {
	s := newServer(t)
	seed(t, s, map[string]string{"X-Amz-Meta-TEST_META_1": "foo", "X-Amz-Meta-__meta_2": "bar"})

	got := get(s, http.MethodHead, objectPath, nil)
	// The wire carries the keys lower case, as S3 does; the client canonicalizes
	// the header names it reads.
	var names []string

	for name := range got.header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
			names = append(names, strings.ToLower(name))
		}
	}

	assert.ElementsMatch(t, []string{"x-amz-meta-test_meta_1", "x-amz-meta-__meta_2"}, names)
}

func TestRangeGet(t *testing.T) {
	tests := []struct {
		name       string
		giveRange  string
		wantStatus int
		wantBody   string
		wantRange  string
	}{
		{"first bytes", "bytes=0-3", http.StatusPartialContent, "0123", "bytes 0-3/10"},
		{"middle", "bytes=2-4", http.StatusPartialContent, "234", "bytes 2-4/10"},
		{"open ended", "bytes=7-", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"end past the object is clamped", "bytes=7-99", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"suffix", "bytes=-4", http.StatusPartialContent, "6789", "bytes 6-9/10"},
		{"suffix longer than the object", "bytes=-99", http.StatusPartialContent, objectBody, "bytes 0-9/10"},
		{"single byte", "bytes=9-9", http.StatusPartialContent, "9", "bytes 9-9/10"},
		{"start past the end", "bytes=10-", http.StatusRequestedRangeNotSatisfiable, "", ""},
		{"zero suffix", "bytes=-0", http.StatusRequestedRangeNotSatisfiable, "", ""},
		{"several ranges answer the whole object", "bytes=0-1,4-5", http.StatusOK, objectBody, ""},
		{"an unreadable range answers the whole object", "bytes=a-b", http.StatusOK, objectBody, ""},
		{"a range that ends before it starts answers the whole object", "bytes=5-2", http.StatusOK, objectBody, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			seed(t, s, nil)

			got := get(s, http.MethodGet, objectPath, map[string]string{headerRange: tt.giveRange})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))

			switch tt.wantStatus {
			case http.StatusRequestedRangeNotSatisfiable:
				e := got.err(t)
				assert.Equal(t, "InvalidRange", e.Code)
				assert.Contains(t, string(got.body), "<ActualObjectSize>10</ActualObjectSize>")
				assert.Contains(t, string(got.body), "<RangeRequested>"+tt.giveRange+"</RangeRequested>")
			default:
				assert.Equal(t, tt.wantBody, string(got.body))
				assert.Equal(t, tt.wantRange, got.header.Get("Content-Range"))
				assert.Equal(t, strconv.Itoa(len(tt.wantBody)), got.header.Get("Content-Length"))
			}

			head := get(s, http.MethodHead, objectPath, map[string]string{headerRange: tt.giveRange})
			assert.Equal(t, tt.wantStatus, head.status)
			assert.Equal(t, tt.wantRange, head.header.Get("Content-Range"))
		})
	}
}

func TestRangeGetOfASpilledBody(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	body := bytes.Repeat([]byte("0123456789abcdef"), (2<<20)/16)
	put(t, s, objectPath, string(body), nil)

	got := get(s, http.MethodGet, objectPath, map[string]string{headerRange: "bytes=1048570-1048585"})
	require.Equal(t, http.StatusPartialContent, got.status)
	assert.Equal(t, body[1048570:1048586], got.body)
}

func TestPartNumberGet(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	one := get(s, http.MethodGet, objectPath+"?partNumber=1", nil)
	require.Equal(t, http.StatusPartialContent, one.status, string(one.body))
	assert.Equal(t, objectBody, string(one.body))
	assert.Equal(t, "bytes 0-9/10", one.header.Get("Content-Range"))

	two := get(s, http.MethodGet, objectPath+"?partNumber=2", nil)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, two.status, string(two.body))
	assert.Equal(t, "InvalidPartNumber", two.err(t).Code)

	zero := get(s, http.MethodGet, objectPath+"?partNumber=0", nil)
	assert.Equal(t, http.StatusBadRequest, zero.status)
	assert.Equal(t, "InvalidArgument", zero.err(t).Code)

	both := get(s, http.MethodGet, objectPath+"?partNumber=1", map[string]string{headerRange: "bytes=0-1"})
	require.Equal(t, http.StatusBadRequest, both.status)
	assert.Equal(t, codeInvalidRequest, both.err(t).Code)
}

// TestPartNumberGetOfAMultipartObject reads each part of an object completed
// from two parts: its range, the object's PartsCount, and with ChecksumMode the
// part's own CRC32 of a COMPOSITE object.
func TestPartNumberGetOfAMultipartObject(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	first, second := bytes.Repeat([]byte("a"), partSize), []byte("the tail")
	crc1, crc2 := crc32.ChecksumIEEE(first), crc32.ChecksumIEEE(second)

	id := startUpload(t, s, multipartKey, map[string]string{headerAlgorithm: nameCRC32})
	etags := []string{
		putPart(t, s, id, 1, first, map[string]string{headerChecksumCRC32: crcBase64(crc1)}),
		putPart(t, s, id, 2, second, map[string]string{headerChecksumCRC32: crcBase64(crc2)}),
	}
	done := complete(s, id, completeBody(etags,
		"<ChecksumCRC32>"+crcBase64(crc1)+"</ChecksumCRC32>", "<ChecksumCRC32>"+crcBase64(crc2)+"</ChecksumCRC32>"), nil)
	require.Equal(t, http.StatusOK, done.status, string(done.body))

	size := len(first) + len(second)
	enabled := map[string]string{headerChecksumMode: checksumModeOn}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		one := get(s, method, multipartKey+"?partNumber=1", enabled)
		require.Equal(t, http.StatusPartialContent, one.status, method)
		assert.Equal(t, fmt.Sprintf("bytes 0-%d/%d", len(first)-1, size), one.header.Get("Content-Range"), method)
		assert.Equal(t, strconv.Itoa(len(first)), one.header.Get("Content-Length"), method)
		assert.Equal(t, "2", one.header.Get(readPartsCount), method)
		assert.Equal(t, crcBase64(crc1), one.header.Get(readChecksumCRC32), method)
		assert.Equal(t, "COMPOSITE", one.header.Get(readChecksumType), method)

		two := get(s, method, multipartKey+"?partNumber=2", enabled)
		require.Equal(t, http.StatusPartialContent, two.status, method)
		assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", len(first), size-1, size), two.header.Get("Content-Range"), method)
		assert.Equal(t, crcBase64(crc2), two.header.Get(readChecksumCRC32), method)
	}

	body := get(s, http.MethodGet, multipartKey+"?partNumber=2", nil)
	assert.Equal(t, second, body.body)

	third := get(s, http.MethodGet, multipartKey+"?partNumber=3", nil)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, third.status)
	assert.Equal(t, "InvalidPartNumber", third.err(t).Code)
	assert.Contains(t, string(third.body), "<ActualPartCount>2</ActualPartCount>")
}

// TestPartNumberReadsPartsByTheirOwnNumber completes an upload from parts 1,
// 3 and 5, as SQ-152 probed it on LocalStack 8b9a79f: GET and HEAD with
// partNumber, and GetObjectAttributes, all name a part by the number it was
// uploaded as, and a number no part has answers InvalidPartNumber.
func TestPartNumberReadsPartsByTheirOwnNumber(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	bodies := map[int][]byte{
		1: bytes.Repeat([]byte("a"), partSize), 3: bytes.Repeat([]byte("b"), partSize), 5: []byte("tail5"),
	}
	numbers := []int{1, 3, 5}
	id := startUpload(t, s, multipartKey, map[string]string{headerAlgorithm: nameCRC32})

	var manifest strings.Builder

	manifest.WriteString("<CompleteMultipartUpload>")

	for _, number := range numbers {
		sum := crcBase64(crc32.ChecksumIEEE(bodies[number]))
		etag := putPart(t, s, id, number, bodies[number], map[string]string{headerChecksumCRC32: sum})
		fmt.Fprintf(&manifest, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag><ChecksumCRC32>%s</ChecksumCRC32></Part>",
			number, etag, sum)
	}

	manifest.WriteString("</CompleteMultipartUpload>")

	done := complete(s, id, []byte(manifest.String()), nil)
	require.Equal(t, http.StatusOK, done.status, string(done.body))

	size := len(bodies[1]) + len(bodies[3]) + len(bodies[5])
	offsets := map[int]int{1: 0, 3: partSize, 5: 2 * partSize}
	enabled := map[string]string{headerChecksumMode: checksumModeOn}

	for _, number := range []int{1, 2, 3, 4, 5, 6} {
		body, found := bodies[number]

		for _, method := range []string{http.MethodGet, http.MethodHead} {
			name := fmt.Sprintf("%s partNumber=%d", method, number)
			got := get(s, method, fmt.Sprintf("%s?partNumber=%d", multipartKey, number), enabled)

			if !found {
				require.Equal(t, http.StatusRequestedRangeNotSatisfiable, got.status, name)

				if method == http.MethodGet {
					assert.Equal(t, "InvalidPartNumber", got.err(t).Code, name)
					assert.Contains(t, string(got.body), "<ActualPartCount>3</ActualPartCount>", name)
				}

				continue
			}

			require.Equal(t, http.StatusPartialContent, got.status, name)
			assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", offsets[number], offsets[number]+len(body)-1, size),
				got.header.Get("Content-Range"), name)
			assert.Equal(t, "3", got.header.Get(readPartsCount), name)
			assert.Equal(t, crcBase64(crc32.ChecksumIEEE(body)), got.header.Get(readChecksumCRC32), name)

			if method == http.MethodGet {
				assert.Equal(t, body, got.body, name)
			}
		}
	}

	attrs := decodeXML[objectAttributes](t, get(s, http.MethodGet, multipartKey+"?attributes",
		map[string]string{headerAttributes: "ObjectParts"}))
	require.NotNil(t, attrs.ObjectParts)
	require.Len(t, attrs.ObjectParts.Parts, 3)

	for i, number := range numbers {
		assert.Equal(t, int32(number), attrs.ObjectParts.Parts[i].Number)
	}
}

type attributesChecksum struct {
	CRC32 string `xml:"ChecksumCRC32"`
	Type  string `xml:"ChecksumType"`
}

type attributesPart struct {
	Number int32  `xml:"PartNumber"`
	Size   int64  `xml:"Size"`
	CRC32  string `xml:"ChecksumCRC32"`
}

type attributesParts struct {
	Total      int32            `xml:"PartsCount"`
	Truncated  bool             `xml:"IsTruncated"`
	MaxParts   int32            `xml:"MaxParts"`
	NextMarker int32            `xml:"NextPartNumberMarker"`
	Parts      []attributesPart `xml:"Part"`
}

type objectAttributes struct {
	ETag         string             `xml:"ETag"`
	StorageClass string             `xml:"StorageClass"`
	ObjectSize   int64              `xml:"ObjectSize"`
	Checksum     attributesChecksum `xml:"Checksum"`
	ObjectParts  *attributesParts   `xml:"ObjectParts"`
}

func TestGetObjectAttributes(t *testing.T) {
	const all = "ETag, StorageClass,ObjectSize,ObjectParts,Checksum"

	t.Run("an object put whole has no parts", func(t *testing.T) {
		s := newServer(t)
		etag := seed(t, s, nil)

		got := get(s, http.MethodGet, objectPath+"?attributes", map[string]string{"X-Amz-Object-Attributes": all})
		require.Equal(t, http.StatusOK, got.status, string(got.body))

		attrs := decodeXML[objectAttributes](t, got)
		assert.Equal(t, etag, `"`+attrs.ETag+`"`)
		assert.Equal(t, "STANDARD", attrs.StorageClass)
		assert.Equal(t, int64(len(objectBody)), attrs.ObjectSize)
		assert.Equal(t, "FULL_OBJECT", attrs.Checksum.Type)
		assert.Nil(t, attrs.ObjectParts)
		assert.NotEmpty(t, got.header.Get(headerLastModified))
	})

	t.Run("a COMPOSITE multipart object lists its parts a page at a time", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		first, second := bytes.Repeat([]byte("a"), partSize), []byte("the tail")
		crc1, crc2 := crc32.ChecksumIEEE(first), crc32.ChecksumIEEE(second)
		id := startUpload(t, s, multipartKey, map[string]string{headerAlgorithm: nameCRC32})
		etags := []string{
			putPart(t, s, id, 1, first, map[string]string{headerChecksumCRC32: crcBase64(crc1)}),
			putPart(t, s, id, 2, second, map[string]string{headerChecksumCRC32: crcBase64(crc2)}),
		}
		done := complete(s, id, completeBody(etags,
			"<ChecksumCRC32>"+crcBase64(crc1)+"</ChecksumCRC32>", "<ChecksumCRC32>"+crcBase64(crc2)+"</ChecksumCRC32>"), nil)
		require.Equal(t, http.StatusOK, done.status, string(done.body))

		headers := map[string]string{"X-Amz-Object-Attributes": all, "X-Amz-Max-Parts": "1"}
		page := decodeXML[objectAttributes](t, get(s, http.MethodGet, multipartKey+"?attributes", headers))
		require.NotNil(t, page.ObjectParts)
		assert.Equal(t, int32(2), page.ObjectParts.Total)
		assert.True(t, page.ObjectParts.Truncated)
		assert.Equal(t, int32(1), page.ObjectParts.NextMarker)
		require.Len(t, page.ObjectParts.Parts, 1)
		assert.Equal(t, crcBase64(crc1), page.ObjectParts.Parts[0].CRC32)
		assert.Equal(t, "COMPOSITE", page.Checksum.Type)
		assert.NotContains(t, page.Checksum.CRC32, "-", "the part count is not part of the attribute")

		headers["X-Amz-Part-Number-Marker"] = "1"
		last := decodeXML[objectAttributes](t, get(s, http.MethodGet, multipartKey+"?attributes", headers))
		require.Len(t, last.ObjectParts.Parts, 1)
		assert.Equal(t, int32(2), last.ObjectParts.Parts[0].Number)
		assert.False(t, last.ObjectParts.Truncated)
	})

	t.Run("a request without attributes is refused", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := get(s, http.MethodGet, objectPath+"?attributes", nil)
		assert.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, "InvalidArgument", got.err(t).Code)
	})
}

func TestRangeOfAnEmptyObject(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	put(t, s, objectPath, "", nil)

	got := get(s, http.MethodGet, objectPath, map[string]string{headerRange: "bytes=0-"})
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, got.status)

	whole := get(s, http.MethodGet, objectPath, nil)
	assert.Equal(t, http.StatusOK, whole.status)
	assert.Empty(t, whole.body)
}

func TestReadPreconditions(t *testing.T) {
	s := newServer(t)
	etag := seed(t, s, nil)

	head := get(s, http.MethodHead, objectPath, nil)
	modified := head.header.Get(headerLastModified)
	require.NotEmpty(t, modified)

	lastModified, err := time.Parse(http.TimeFormat, modified)
	require.NoError(t, err)

	// A moment past the object's second, so "modified since" can hold.
	time.Sleep(1100 * time.Millisecond)

	earlier := lastModified.Add(-time.Hour).Format(http.TimeFormat)
	future := time.Now().Add(24 * time.Hour).Format(http.TimeFormat)
	now := time.Now().UTC().Format(http.TimeFormat)

	tests := []struct {
		name          string
		giveHeader    map[string]string
		wantStatus    int
		wantCondition string
	}{
		{"If-Match with the ETag", map[string]string{headerIfMatch: etag}, http.StatusOK, ""},
		{"If-Match with the ETag unquoted", map[string]string{headerIfMatch: strings.Trim(etag, `"`)}, http.StatusOK, ""},
		{"If-Match with any", map[string]string{headerIfMatch: "*"}, http.StatusOK, ""},
		{"If-Match with a listed ETag", map[string]string{headerIfMatch: `"other", ` + etag}, http.StatusOK, ""},
		{"If-Match with another ETag", map[string]string{headerIfMatch: etagOther}, 412, headerIfMatch},
		{"If-None-Match with another ETag", map[string]string{headerIfNoneMatch: etagOther}, http.StatusOK, ""},
		{"If-None-Match with the ETag", map[string]string{headerIfNoneMatch: etag}, http.StatusNotModified, ""},
		{
			"If-None-Match with the ETag unquoted",
			map[string]string{headerIfNoneMatch: strings.Trim(etag, `"`)},
			http.StatusNotModified, "",
		},
		{"If-Unmodified-Since after the change", map[string]string{headerIfUnmodifiedSince: now}, http.StatusOK, ""},
		{
			"If-Unmodified-Since before the change",
			map[string]string{headerIfUnmodifiedSince: earlier},
			412, headerIfUnmodifiedSince,
		},
		{"If-Unmodified-Since at the change", map[string]string{headerIfUnmodifiedSince: modified}, http.StatusOK, ""},
		{"If-Modified-Since before the change", map[string]string{headerIfModifiedSince: earlier}, http.StatusOK, ""},
		{"If-Modified-Since after the change", map[string]string{headerIfModifiedSince: now}, http.StatusNotModified, ""},
		{"If-Modified-Since at the change", map[string]string{headerIfModifiedSince: modified}, http.StatusNotModified, ""},
		{"If-Modified-Since in the future is ignored", map[string]string{headerIfModifiedSince: future}, http.StatusOK, ""},
		{
			"If-Match decides before the others",
			map[string]string{headerIfMatch: etagOther, headerIfNoneMatch: etag, headerIfModifiedSince: now},
			412, "If-Match",
		},
		{
			"If-Unmodified-Since decides before If-None-Match",
			map[string]string{headerIfUnmodifiedSince: earlier, headerIfNoneMatch: etag},
			412, headerIfUnmodifiedSince,
		},
		{
			// AWS's GetObject and HeadObject documentation; LocalStack answers 412.
			"a matching If-Match overrides a failed If-Unmodified-Since",
			map[string]string{headerIfMatch: etag, headerIfUnmodifiedSince: earlier},
			http.StatusOK, "",
		},
		{
			"a failing If-Match is not overridden by If-Unmodified-Since",
			map[string]string{headerIfMatch: etagOther, headerIfUnmodifiedSince: now},
			412, headerIfMatch,
		},
		{
			"every condition holds",
			map[string]string{
				headerIfMatch: etag, headerIfUnmodifiedSince: now, headerIfNoneMatch: etagOther, headerIfModifiedSince: earlier,
			},
			http.StatusOK, "",
		},
	}

	for _, tt := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+tt.name, func(t *testing.T) {
				got := get(s, method, objectPath, tt.giveHeader)
				require.Equal(t, tt.wantStatus, got.status, string(got.body))

				switch tt.wantStatus {
				case http.StatusNotModified:
					assert.Empty(t, got.body)
					assert.Empty(t, got.header.Get(headerContentType))
				case http.StatusPreconditionFailed:
					if method == http.MethodGet {
						e := got.err(t)
						assert.Equal(t, codePrecondition, e.Code)
						assert.Contains(t, string(got.body), "<Condition>"+tt.wantCondition+"</Condition>")
					}
				default:
					if method == http.MethodGet {
						assert.Equal(t, objectBody, string(got.body))
					}
				}
			})
		}
	}
}

func TestConditionalWrites(t *testing.T) {
	tests := []struct {
		name       string
		giveHeader map[string]string
		wantStatus int
		wantCode   string
		wantBody   string
	}{
		{"If-None-Match * on an existing key", map[string]string{headerIfNoneMatch: "*"}, 412, codePrecondition, objectBody},
		{"If-Match with the current ETag", map[string]string{headerIfMatch: "etag"}, 200, "", "new"},
		{"If-Match with another ETag", map[string]string{headerIfMatch: etagOther}, 412, codePrecondition, objectBody},
		{"If-Match * is not implemented", map[string]string{headerIfMatch: "*"}, 501, codeNotImplemented, objectBody},
		{
			"If-None-Match with an ETag is not implemented",
			map[string]string{headerIfNoneMatch: etagX},
			501, codeNotImplemented, objectBody,
		},
		{
			"both headers are not implemented",
			map[string]string{headerIfMatch: etagX, headerIfNoneMatch: "*"},
			501, codeNotImplemented, objectBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			etag := seed(t, s, nil)

			header := tt.giveHeader
			if header[headerIfMatch] == "etag" {
				header = map[string]string{headerIfMatch: etag}
			}

			got := s.do(call{
				method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte("new"), header: header,
			})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))

			if tt.wantCode != "" {
				assert.Equal(t, tt.wantCode, got.err(t).Code)
			}

			read := get(s, http.MethodGet, objectPath, nil)
			assert.Equal(t, tt.wantBody, string(read.body))
		})
	}
}

func TestConditionalWriteOnAMissingKey(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	ifMatch := s.do(call{
		method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte("x"),
		header: map[string]string{headerIfMatch: `"abc"`},
	})
	require.Equal(t, http.StatusNotFound, ifMatch.status)
	assert.Equal(t, codeNoSuchKey, ifMatch.err(t).Code)

	first := s.do(call{
		method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte("x"),
		header: map[string]string{headerIfNoneMatch: "*"},
	})
	assert.Equal(t, http.StatusOK, first.status, string(first.body))
}

func TestConcurrentCreateOnlyWritesOneWinner(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	const writers = 16

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = map[int]int{}
	)

	for i := range writers {
		wg.Go(func() {
			got := s.do(call{
				method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(strconv.Itoa(i)),
				header: map[string]string{headerIfNoneMatch: "*"},
			})

			mu.Lock()
			statuses[got.status]++
			mu.Unlock()
		})
	}

	wg.Wait()

	assert.Equal(t, map[int]int{http.StatusOK: 1, http.StatusPreconditionFailed: writers - 1}, statuses)
}

func TestDeleteWithConditionsIsNotImplemented(t *testing.T) {
	for name, header := range map[string]map[string]string{
		"If-Match":                          {headerIfMatch: "*"},
		"x-amz-if-match-size":               {"X-Amz-If-Match-Size": "10"},
		"x-amz-if-match-last-modified-time": {"X-Amz-If-Match-Last-Modified-Time": "Wed, 21 Oct 2037 07:28:00 GMT"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			seed(t, s, nil)

			got := get(s, http.MethodDelete, objectPath, header)
			require.Equal(t, http.StatusNotImplemented, got.status, string(got.body))
			assert.Equal(t, codeNotImplemented, got.err(t).Code)
			assert.Contains(t, string(got.body), "<Header>"+name+"</Header>")

			assert.Equal(t, http.StatusOK, get(s, http.MethodHead, objectPath, nil).status)
		})
	}
}

func TestDeleteObjectVersionOfAnUnversionedBucket(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	got := get(s, http.MethodDelete, objectPath+"?versionId=abc", nil)
	require.Equal(t, http.StatusBadRequest, got.status)
	assert.Equal(t, "InvalidArgument", got.err(t).Code)

	null := get(s, http.MethodDelete, objectPath+"?versionId=null", nil)
	assert.Equal(t, http.StatusNoContent, null.status)
}

func TestGetOfAnArchivedObject(t *testing.T) {
	s := newServer(t)
	seed(t, s, map[string]string{headerStorageClass: classGlacier})

	head := get(s, http.MethodHead, objectPath, nil)
	assert.Equal(t, http.StatusOK, head.status)
	assert.Equal(t, classGlacier, head.header.Get(headerStorageClass))

	got := get(s, http.MethodGet, objectPath, nil)
	require.Equal(t, http.StatusForbidden, got.status)
	assert.Equal(t, "InvalidObjectState", got.err(t).Code)
}

func TestGetResponseOverrides(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	got := get(s, http.MethodGet, objectPath+"?response-content-type=text/csv&response-cache-control=no-store"+
		"&response-content-disposition=attachment&response-content-language=fr&response-content-encoding=gzip"+
		"&response-expires=Wed,%2021%20Oct%202037%2007:28:00%20GMT",
		map[string]string{headerAcceptEncoding: encodingIdentity})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Equal(t, "text/csv", got.header.Get(headerContentType))
	assert.Equal(t, "no-store", got.header.Get("Cache-Control"))
	assert.Equal(t, "attachment", got.header.Get("Content-Disposition"))
	assert.Equal(t, "fr", got.header.Get("Content-Language"))
	assert.Equal(t, "gzip", got.header.Get("Content-Encoding"))
	assert.Equal(t, "Wed, 21 Oct 2037 07:28:00 GMT", got.header.Get("Expires"))

	bad := get(s, http.MethodGet, objectPath+"?response-content-type=%E2%80%94", nil)
	require.Equal(t, http.StatusBadRequest, bad.status, string(bad.body))
	assert.Equal(t, "InvalidArgument", bad.err(t).Code)
}

func TestChecksumModeOnARange(t *testing.T) {
	s := newServer(t)

	sum := crc32.ChecksumIEEE([]byte(objectBody))
	checksum := base64.StdEncoding.EncodeToString([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
	seed(t, s, map[string]string{headerChecksumCRC32: checksum})

	mode := map[string]string{headerChecksumMode: checksumModeOn}
	whole := get(s, http.MethodGet, objectPath, mode)
	assert.Equal(t, checksum, whole.header.Get(readChecksumCRC32))

	mode[headerRange] = "bytes=0-3"
	partial := get(s, http.MethodGet, objectPath, mode)
	require.Equal(t, http.StatusPartialContent, partial.status)
	assert.Empty(t, partial.header.Get(readChecksumCRC32), "a checksum is of the whole object")

	mode[headerRange] = "bytes=0-9"
	all := get(s, http.MethodGet, objectPath, mode)
	require.Equal(t, http.StatusPartialContent, all.status)
	assert.Equal(t, checksum, all.header.Get(readChecksumCRC32))

	assert.Empty(t, get(s, http.MethodGet, objectPath, nil).header.Get(readChecksumCRC32))
}

func TestDeleteObjects(t *testing.T) {
	body := func(quiet bool, keys ...string) string {
		var sb strings.Builder

		sb.WriteString(`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)

		if quiet {
			sb.WriteString("<Quiet>true</Quiet>")
		}

		for _, key := range keys {
			sb.WriteString("<Object><Key>" + key + "</Key></Object>")
		}

		sb.WriteString("</Delete>")

		return sb.String()
	}

	post := func(s *server, payload string, header map[string]string) reply {
		sum, _ := md5Of([]byte(payload))
		h := map[string]string{headerContentMD5: base64.StdEncoding.EncodeToString(sum)}
		maps.Copy(h, header)

		return s.do(call{
			method: http.MethodPost, target: targetDelete, region: regionUSEast1, body: []byte(payload), header: h,
		})
	}

	t.Run("deleted keys are listed, missing ones included", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := post(s, body(false, "key", "missing-1", "missing-2"), nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Contains(t, string(got.body), "<DeleteResult")
		assert.Equal(t, 3, strings.Count(string(got.body), "<Deleted>"), string(got.body))
		assert.Contains(t, string(got.body), "<Key>missing-2</Key>")
		assert.NotContains(t, string(got.body), "<Error>")
		assert.Equal(t, http.StatusNotFound, get(s, http.MethodHead, objectPath, nil).status)
	})

	t.Run("quiet mode answers nothing for a delete", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := post(s, body(true, "key", "missing"), nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.NotContains(t, string(got.body), "<Deleted>")
		assert.NotContains(t, string(got.body), "<Error>")
		assert.Equal(t, http.StatusNotFound, get(s, http.MethodHead, objectPath, nil).status)
	})

	t.Run("a version of an unversioned bucket is an error for that key", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		payload := `<Delete><Object><Key>key</Key><VersionId>abc</VersionId></Object>` +
			`<Object><Key>other</Key></Object></Delete>`
		got := post(s, payload, nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Contains(t, string(got.body), "<Code>NoSuchVersion</Code>")
		assert.Contains(t, string(got.body), "<VersionId>abc</VersionId>")
		assert.Equal(t, 1, strings.Count(string(got.body), "<Deleted>"))
		assert.Equal(t, http.StatusOK, get(s, http.MethodHead, objectPath, nil).status)
	})

	t.Run("keys with special characters", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)
		put(t, s, "/bucket/a%20b%2Bc", "x", nil)

		got := post(s, body(false, "a b+c"), nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, http.StatusNotFound, get(s, http.MethodHead, "/bucket/a%20b%2Bc", nil).status)
	})

	t.Run("no integrity header is accepted, as LocalStack does", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := s.do(call{
			method: http.MethodPost, target: "/bucket/?delete&x-id=DeleteObjects", region: regionUSEast1,
			body: []byte(body(false, "key")),
		})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Contains(t, string(got.body), "<Deleted>")
		assert.Equal(t, http.StatusNotFound, get(s, http.MethodHead, objectPath, nil).status)
	})

	t.Run("a checksum header is accepted", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := s.do(call{
			method: http.MethodPost, target: targetDelete, region: regionUSEast1, body: []byte(body(false, "key")),
			header: map[string]string{"X-Amz-Sdk-Checksum-Algorithm": nameCRC32, headerChecksumCRC32: "AAAAAA=="},
		})
		assert.Equal(t, http.StatusOK, got.status, string(got.body))
	})

	t.Run("a Content-MD5 that is not a digest is refused", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := post(s, body(false, "key"), map[string]string{headerContentMD5: "not-base64"})
		require.Equal(t, http.StatusBadRequest, got.status, string(got.body))
		assert.Equal(t, "InvalidDigest", got.err(t).Code)
	})

	t.Run("an empty list and too long a list are malformed", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		empty := post(s, body(false), nil)
		require.Equal(t, http.StatusBadRequest, empty.status, string(empty.body))
		assert.Equal(t, codeMalformedXML, empty.err(t).Code)

		keys := make([]string, 1001)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d", i)
		}

		long := post(s, body(false, keys...), nil)
		require.Equal(t, http.StatusBadRequest, long.status, string(long.body))
		assert.Equal(t, codeMalformedXML, long.err(t).Code)

		keys = keys[:1000]
		assert.Equal(t, http.StatusOK, post(s, body(true, keys...), nil).status)
	})

	t.Run("a missing bucket", func(t *testing.T) {
		s := newServer(t)

		got := post(s, body(false, "key"), nil)
		require.Equal(t, http.StatusNotFound, got.status, string(got.body))
		assert.Equal(t, "NoSuchBucket", got.err(t).Code)
	})

	t.Run("the bypass header needs Object Lock", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := post(s, body(false, "key"), map[string]string{"X-Amz-Bypass-Governance-Retention": "true"})
		require.Equal(t, http.StatusBadRequest, got.status, string(got.body))
		assert.Equal(t, "InvalidArgument", got.err(t).Code)
	})
}
