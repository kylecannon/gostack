package s3_test

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	multipartKey = "/bucket/big"
	mib          = 1 << 20
	partSize     = 5 * mib

	headerChecksumType = "x-amz-checksum-type"

	headerAlgorithm       = "x-amz-checksum-algorithm"
	headerChecksumCRC32   = "x-amz-checksum-crc32"
	headerChecksumCRC32C  = "x-amz-checksum-crc32c"
	typeFullObject        = "FULL_OBJECT"
	headerContentEncoding = "Content-Encoding"
	headerContentType     = "Content-Type"
)

type initiated struct {
	UploadID string `xml:"UploadId"`
}

type completed struct {
	ETag           string `xml:"ETag"`
	Location       string `xml:"Location"`
	ChecksumCRC32  string `xml:"ChecksumCRC32"`
	ChecksumCRC32C string `xml:"ChecksumCRC32C"`
	ChecksumCRC64  string `xml:"ChecksumCRC64NVME"`
	ChecksumType   string `xml:"ChecksumType"`
}

type listedPart struct {
	PartNumber int32  `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
	Size       int64  `xml:"Size"`
}

type listedParts struct {
	IsTruncated          bool         `xml:"IsTruncated"`
	NextPartNumberMarker int32        `xml:"NextPartNumberMarker"`
	Parts                []listedPart `xml:"Part"`
}

type listedUpload struct {
	Key      string `xml:"Key"`
	UploadID string `xml:"UploadId"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listedUploads struct {
	Uploads        []listedUpload `xml:"Upload"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

func decodeXML[T any](t *testing.T, got reply) T {
	t.Helper()

	var v T
	require.NoError(t, xml.Unmarshal(got.body, &v), string(got.body))

	return v
}

// startUpload creates an upload of path with the extra headers and returns
// its id.
func startUpload(t *testing.T, s *server, path string, header map[string]string) string {
	t.Helper()

	got := s.do(call{method: http.MethodPost, target: path + "?uploads", region: regionUSEast1, header: header})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return decodeXML[initiated](t, got).UploadID
}

// putPart uploads one part and returns its quoted ETag.
func putPart(t *testing.T, s *server, id string, number int, body []byte, header map[string]string) string {
	t.Helper()

	got := s.do(call{
		method: http.MethodPut, target: fmt.Sprintf("%s?partNumber=%d&uploadId=%s", multipartKey, number, id),
		region: regionUSEast1, body: body, header: header,
	})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return got.header.Get("ETag")
}

// completeBody lists parts by number and ETag, with extra elements per part.
func completeBody(etags []string, extra ...string) []byte {
	var b strings.Builder

	b.WriteString("<CompleteMultipartUpload>")

	for i, etag := range etags {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag>", i+1, etag)

		if i < len(extra) {
			b.WriteString(extra[i])
		}

		b.WriteString("</Part>")
	}

	b.WriteString("</CompleteMultipartUpload>")

	return []byte(b.String())
}

func complete(s *server, id string, body []byte, header map[string]string) reply {
	return s.do(call{
		method: http.MethodPost, target: multipartKey + "?uploadId=" + id, region: regionUSEast1, body: body, header: header,
	})
}

// multipartETag is md5 of the part md5s, then -N.
func multipartETag(parts ...[]byte) string {
	all := md5.New()

	for _, part := range parts {
		sum := md5.Sum(part)
		all.Write(sum[:])
	}

	return `"` + hex.EncodeToString(all.Sum(nil)) + "-" + strconv.Itoa(len(parts)) + `"`
}

func crcBase64(sum uint32) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))
}

func TestMultipartUploadRoundTrip(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	first, second := bytes.Repeat([]byte("a"), partSize), []byte("tail of the object")
	id := startUpload(t, s, multipartKey, map[string]string{headerContentType: "text/plain", "x-amz-meta-color": "blue"})

	etags := []string{putPart(t, s, id, 1, first, nil), putPart(t, s, id, 2, second, nil)}

	listed := decodeXML[listedParts](t, s.do(call{
		method: http.MethodGet, target: multipartKey + "?uploadId=" + id + "&max-parts=1", region: regionUSEast1,
	}))
	require.Len(t, listed.Parts, 1)
	assert.True(t, listed.IsTruncated)
	assert.Equal(t, int32(1), listed.NextPartNumberMarker)
	assert.Equal(t, int64(partSize), listed.Parts[0].Size)

	got := complete(s, id, completeBody(etags), nil)
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	done := decodeXML[completed](t, got)
	assert.Equal(t, multipartETag(first, second), done.ETag)
	assert.Equal(t, typeFullObject, done.ChecksumType, "an upload with no checksum gets a full-object CRC64NVME")
	assert.NotEmpty(t, done.ChecksumCRC64)
	assert.Equal(t, "http://bucket.s3."+localstackHost+"/big", done.Location)

	object := s.do(call{method: http.MethodGet, target: multipartKey, region: regionUSEast1})
	require.Equal(t, http.StatusOK, object.status)
	assert.Equal(t, slices.Concat(first, second), object.body)
	assert.Equal(t, done.ETag, object.header.Get("ETag"))
	assert.Equal(t, "text/plain", object.header.Get("Content-Type"))
	assert.Equal(t, "blue", object.header.Get("X-Amz-Meta-Color"))

	again := complete(s, id, completeBody(etags), nil)
	assert.Equal(t, "NoSuchUpload", again.err(t).Code, "a completed upload is gone")
}

func TestCompleteRefusesALaterPartUnderTheMinimumAndKeepsTheUpload(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	id := startUpload(t, s, multipartKey, nil)
	etags := []string{putPart(t, s, id, 1, []byte("small"), nil), putPart(t, s, id, 2, []byte("x"), nil)}

	tests := []struct {
		name     string
		giveBody []byte
		wantCode string
	}{
		{"part under 5 MiB before the last", completeBody(etags), "EntityTooSmall"},
		{
			"parts out of order", []byte("<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>" + etags[1] +
				"</ETag></Part><Part><PartNumber>1</PartNumber><ETag>" + etags[0] + "</ETag></Part></CompleteMultipartUpload>"),
			"InvalidPartOrder",
		},
		{"wrong ETag", completeBody([]string{`"00000000000000000000000000000000"`}), "InvalidPart"},
		{"no parts", []byte("<CompleteMultipartUpload></CompleteMultipartUpload>"), codeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := complete(s, id, tt.giveBody, nil)
			require.Equal(t, http.StatusBadRequest, got.status, string(got.body))
			assert.Equal(t, tt.wantCode, got.err(t).Code)
		})
	}

	got := complete(s, id, completeBody(etags[:1]), nil)
	require.Equal(t, http.StatusOK, got.status, "a refused completion leaves the upload: %s", got.body)
}

func TestMultipartChecksums(t *testing.T) {
	first, second := bytes.Repeat([]byte("b"), partSize), []byte("second part")
	crc1, crc2 := crc32.ChecksumIEEE(first), crc32.ChecksumIEEE(second)

	t.Run("composite CRC32 is the CRC of the part CRCs", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		id := startUpload(t, s, multipartKey, map[string]string{headerAlgorithm: "CRC32"})
		etags := []string{
			putPart(t, s, id, 1, first, map[string]string{headerChecksumCRC32: crcBase64(crc1)}),
			putPart(t, s, id, 2, second, map[string]string{headerChecksumCRC32: crcBase64(crc2)}),
		}

		got := complete(s, id, completeBody(etags,
			"<ChecksumCRC32>"+crcBase64(crc1)+"</ChecksumCRC32>", "<ChecksumCRC32>"+crcBase64(crc2)+"</ChecksumCRC32>"), nil)
		require.Equal(t, http.StatusOK, got.status, string(got.body))

		raw := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, crc1), crc2)
		done := decodeXML[completed](t, got)
		assert.Equal(t, crcBase64(crc32.ChecksumIEEE(raw))+"-2", done.ChecksumCRC32)
		assert.Equal(t, "COMPOSITE", done.ChecksumType)
	})

	t.Run("full-object CRC32C is the CRC of the whole body", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		castagnoli := crc32.MakeTable(crc32.Castagnoli)
		id := startUpload(t, s, multipartKey, map[string]string{
			headerAlgorithm: "CRC32C", headerChecksumType: typeFullObject,
		})
		etags := []string{
			putPart(t, s, id, 1, first,
				map[string]string{headerChecksumCRC32C: crcBase64(crc32.Checksum(first, castagnoli))}),
			putPart(t, s, id, 2, second,
				map[string]string{headerChecksumCRC32C: crcBase64(crc32.Checksum(second, castagnoli))}),
		}
		whole := crcBase64(crc32.Checksum(slices.Concat(first, second), castagnoli))

		wrong := complete(s, id, completeBody(etags), map[string]string{
			headerChecksumCRC32C: zeroCRC32, headerChecksumType: typeFullObject,
		})
		assert.Equal(t, "BadDigest", wrong.err(t).Code)

		got := complete(s, id, completeBody(etags), map[string]string{
			headerChecksumCRC32C: whole, headerChecksumType: typeFullObject,
		})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, whole, decodeXML[completed](t, got).ChecksumCRC32C)
	})

	t.Run("a part without the upload's checksum is refused", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		id := startUpload(t, s, multipartKey, map[string]string{headerAlgorithm: "SHA256"})
		got := s.do(call{
			method: http.MethodPut, target: multipartKey + "?partNumber=1&uploadId=" + id, region: regionUSEast1,
			body: second, header: map[string]string{headerChecksumCRC32: crcBase64(crc2)},
		})
		assert.Equal(t, codeInvalidRequest, got.err(t).Code)
		assert.Contains(t, got.err(t).Message, "expected checksum Type: sha256, actual checksum Type: crc32")
	})
}

func TestUploadPartCopyStreamsARange(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	source := []byte("0123456789abcdefghij")
	require.Equal(t, http.StatusOK, s.do(call{
		method: http.MethodPut, target: "/bucket/source", region: regionUSEast1, body: source,
	}).status)

	id := startUpload(t, s, multipartKey, nil)
	copyPart := func(header map[string]string) reply {
		header["x-amz-copy-source"] = "/bucket/source"

		return s.do(call{
			method: http.MethodPut, target: multipartKey + "?partNumber=1&uploadId=" + id, region: regionUSEast1, header: header,
		})
	}

	refused := copyPart(map[string]string{"x-amz-copy-source-if-match": `"nope"`})
	require.Equal(t, http.StatusPreconditionFailed, refused.status, string(refused.body))

	outside := copyPart(map[string]string{"x-amz-copy-source-range": "bytes=5-99"})
	assert.Equal(t, "InvalidArgument", outside.err(t).Code)

	got := copyPart(map[string]string{"x-amz-copy-source-range": "bytes=5-9"})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	var result struct {
		ETag string `xml:"ETag"`
	}
	require.NoError(t, xml.Unmarshal(got.body, &result))

	_, etag := md5Of(source[5:10])
	assert.Equal(t, etag, result.ETag)

	require.Equal(t, http.StatusOK, complete(s, id, completeBody([]string{result.ETag}), nil).status)
	assert.Equal(t, source[5:10], s.do(call{method: http.MethodGet, target: multipartKey, region: regionUSEast1}).body)
}

func TestListAndAbortMultipartUploads(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	ids := map[string]string{}
	for _, key := range []string{"a/1", "a/2", "b"} {
		ids[key] = startUpload(t, s, "/bucket/"+key, nil)
	}

	listed := decodeXML[listedUploads](t, s.do(call{
		method: http.MethodGet, target: "/bucket?uploads&delimiter=/", region: regionUSEast1,
	}))
	require.Len(t, listed.Uploads, 1)
	assert.Equal(t, "b", listed.Uploads[0].Key)
	require.Len(t, listed.CommonPrefixes, 1)
	assert.Equal(t, "a/", listed.CommonPrefixes[0].Prefix)

	aborted := s.do(call{method: http.MethodDelete, target: "/bucket/b?uploadId=" + ids["b"], region: regionUSEast1})
	require.Equal(t, http.StatusNoContent, aborted.status, string(aborted.body))

	again := s.do(call{method: http.MethodDelete, target: "/bucket/b?uploadId=" + ids["b"], region: regionUSEast1})
	assert.Equal(t, "NoSuchUpload", again.err(t).Code)

	listed = decodeXML[listedUploads](t, s.do(call{
		method: http.MethodGet, target: "/bucket?uploads&prefix=a/", region: regionUSEast1,
	}))
	require.Len(t, listed.Uploads, 2)
	assert.Equal(t, ids["a/1"], listed.Uploads[0].UploadID)
}

// TestUploadPartWithExpectContinueAndEmptyBody is the rustack regression for
// UploadPart: an empty part sent with Expect: 100-continue is stored.
func TestUploadPartWithExpectContinueAndEmptyBody(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	id := startUpload(t, s, multipartKey, nil)
	etag := putPart(t, s, id, 1, nil, map[string]string{"Expect": "100-continue"})

	_, empty := md5Of(nil)
	assert.Equal(t, empty, etag)
}

func TestAWSChunkedBodies(t *testing.T) {
	const signed = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"

	chunked := func(payload, trailer string) []byte {
		body := fmt.Sprintf("%x;chunk-signature=0\r\n%s\r\n0;chunk-signature=0\r\n", len(payload), payload)
		if trailer != "" {
			body += trailer + "\r\n\r\n"
		}

		return []byte(body)
	}
	headers := func(payload string, extra ...string) map[string]string {
		h := map[string]string{
			"X-Amz-Content-Sha256": signed + "-TRAILER", headerContentEncoding: "aws-chunked",
			"X-Amz-Decoded-Content-Length": strconv.Itoa(len(payload)),
		}
		for i := 0; i < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}

		return h
	}

	t.Run("a trailer checksum is checked after the body", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		payload := "Hello Blob"
		sum := sha256.Sum256([]byte(payload))
		good := base64.StdEncoding.EncodeToString(sum[:])
		h := headers(payload, "X-Amz-Trailer", "x-amz-checksum-sha256")

		bad := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1, header: h,
			body: chunked(payload, "x-amz-checksum-sha256:wrongchecksum"),
		})
		require.Equal(t, http.StatusBadRequest, bad.status)
		assert.Equal(t, "Value for x-amz-checksum-sha256 header is invalid.", bad.err(t).Message)
		absent := s.do(call{method: http.MethodGet, target: objectPath, region: regionUSEast1})
		assert.Equal(t, http.StatusNotFound, absent.status)

		mismatch := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1, header: h,
			body: chunked(payload, "x-amz-checksum-sha256:"+base64.StdEncoding.EncodeToString(make([]byte, 32))),
		})
		assert.Equal(t, "BadDigest", mismatch.err(t).Code)

		put := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1, header: h,
			body: chunked(payload, "x-amz-checksum-sha256:"+good+"\r\nx-amz-trailer-signature:0"),
		})
		require.Equal(t, http.StatusOK, put.status, string(put.body))
		assert.Equal(t, good, put.header.Get("X-Amz-Checksum-Sha256"))

		got := s.do(call{method: http.MethodGet, target: objectPath, region: regionUSEast1})
		assert.Equal(t, payload, string(got.body))
	})

	t.Run("aws-chunked leaves the stored Content-Encoding", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		put := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1,
			header: headers("gz", headerContentEncoding, "aws-chunked,gzip"), body: chunked("gz", ""),
		})
		require.Equal(t, http.StatusOK, put.status, string(put.body))

		got := s.do(call{
			method: http.MethodGet, target: objectPath, region: regionUSEast1,
			header: map[string]string{"Accept-Encoding": "gzip"},
		})
		assert.Equal(t, "gzip", got.header.Get(headerContentEncoding))
		assert.Equal(t, "gz", string(got.body))
	})

	t.Run("an empty payload with a trailer", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		put := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1,
			header: map[string]string{
				headerContentEncoding: "aws-chunked", "X-Amz-Decoded-Content-Length": "0",
				"X-Amz-Trailer": headerChecksumCRC32,
			}, body: []byte("0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"),
		})
		require.Equal(t, http.StatusOK, put.status, string(put.body))
		assert.Empty(t, s.do(call{method: http.MethodGet, target: objectPath, region: regionUSEast1}).body)
	})

	t.Run("a signed stream without its decoded length is refused", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		for _, length := range []string{"", "test"} {
			h := map[string]string{"X-Amz-Content-Sha256": signed}
			if length != "" {
				h["X-Amz-Decoded-Content-Length"] = length
			}

			got := s.do(call{
				method: http.MethodPut, target: objectPath, region: regionUSEast1, header: h,
				body: []byte("test"),
			})
			require.Equal(t, http.StatusForbidden, got.status, string(got.body))
			assert.Equal(t, "SignatureDoesNotMatch", got.err(t).Code)
		}
	})

	t.Run("a broken part stream fails and the part can be sent again", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		id := startUpload(t, s, multipartKey, nil)
		target := multipartKey + "?partNumber=1&uploadId=" + id
		payload := "Hello Blob"

		broken := s.do(call{
			method: http.MethodPut, target: target, region: regionUSEast1, header: headers(payload),
			body: []byte("\r\n" + payload + "\r\n0;chunk-signature=0\r\n"),
		})
		require.Equal(t, http.StatusInternalServerError, broken.status, string(broken.body))

		etag := putPart(t, s, id, 1, chunked(payload, ""), headers(payload))
		_, want := md5Of([]byte(payload))
		assert.Equal(t, want, etag)
	})
}
