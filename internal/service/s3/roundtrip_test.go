package s3_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	localstackHost = "localhost.localstack.cloud:4566"
	regionUSEast1  = "us-east-1"
	regionEUWest1  = "eu-west-1"

	bucketAPath      = "/bucket-a"
	objectPath       = "/bucket/key"
	headerContentMD5 = "Content-MD5"
	// zeroCRC32 is a CRC32 of four zero bytes, which no body here has.
	zeroCRC32 = "AAAAAA=="
)

// server runs the real gateway with S3 alone registered.
type server struct {
	t   *testing.T
	url string
}

func newServer(t *testing.T) *server {
	t.Helper()

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		s3.NewEphemeral(t.TempDir()))
	t.Cleanup(provider.Close)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	return &server{t: t, url: srv.URL}
}

// call is one request: signed for S3 in region unless region is empty, sent to
// host when it is set, which addresses a virtual host.
type call struct {
	method string
	host   string
	target string
	region string
	header map[string]string
	body   []byte
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

// s3Error is the body of an S3 error, with the members these tests read.
type s3Error struct {
	Code       string `xml:"Code"`
	Message    string `xml:"Message"`
	BucketName string `xml:"BucketName"`
	Key        string `xml:"Key"`
	RequestID  string `xml:"RequestId"`
}

func (s *server) do(c call) reply {
	s.t.Helper()

	req, err := http.NewRequestWithContext(s.t.Context(), c.method, s.url+c.target, bytes.NewReader(c.body))
	require.NoError(s.t, err)

	if c.host != "" {
		req.Host = c.host
	}

	if c.region != "" {
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/"+c.region+
			"/s3/aws4_request, SignedHeaders=host, Signature=0")
	}

	for name, value := range c.header {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(s.t, err)

	defer func() { require.NoError(s.t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	return reply{status: resp.StatusCode, header: resp.Header, body: body}
}

func (r reply) err(t *testing.T) s3Error {
	t.Helper()

	var e s3Error
	require.NoError(t, xml.Unmarshal(r.body, &e), string(r.body))

	return e
}

func md5Of(body []byte) ([]byte, string) {
	sum := md5.Sum(body)

	return sum[:], `"` + hex.EncodeToString(sum[:]) + `"`
}

func createBucket(t *testing.T, s *server) {
	t.Helper()

	got := s.do(call{method: http.MethodPut, target: "/bucket", region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
}

func TestBucketLifecycle(t *testing.T) {
	s := newServer(t)

	created := s.do(call{method: http.MethodPut, target: bucketAPath, region: regionUSEast1})
	require.Equal(t, http.StatusOK, created.status, string(created.body))
	assert.Equal(t, bucketAPath, created.header.Get("Location"))
	assert.Empty(t, created.header.Get("Content-Type"), "a successful PUT carries no body")

	again := s.do(call{method: http.MethodPut, target: bucketAPath, region: regionUSEast1})
	assert.Equal(t, http.StatusOK, again.status, "CreateBucket is idempotent in us-east-1")

	head := s.do(call{method: http.MethodHead, target: bucketAPath, region: regionUSEast1})
	require.Equal(t, http.StatusOK, head.status)
	assert.Equal(t, regionUSEast1, head.header.Get("X-Amz-Bucket-Region"))
	assert.Equal(t, "arn:aws:s3:::bucket-a", head.header.Get("X-Amz-Bucket-Arn"))
	assert.Equal(t, "application/xml", head.header.Get("Content-Type"))
	assert.NotEmpty(t, head.header.Get("X-Amz-Id-2"))

	located := s.do(call{
		method: http.MethodPut, target: "/bucket-b", region: regionEUWest1,
		body: []byte(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint>` +
			`</CreateBucketConfiguration>`),
	})
	require.Equal(t, http.StatusOK, located.status, string(located.body))
	assert.Equal(t, "http://bucket-b.s3."+localstackHost+"/", located.header.Get("Location"))

	location := s.do(call{method: http.MethodGet, target: "/bucket-b?location", region: regionEUWest1})
	require.Equal(t, http.StatusOK, location.status)
	assert.Contains(t, string(location.body), `>eu-west-1</LocationConstraint>`)

	illegal := s.do(call{method: http.MethodPut, target: "/bucket-c", region: regionEUWest1})
	require.Equal(t, http.StatusBadRequest, illegal.status)
	assert.Equal(t, "IllegalLocationConstraintException", illegal.err(t).Code)

	listed := s.do(call{method: http.MethodGet, target: "/?max-buckets=1", region: regionUSEast1})
	require.Equal(t, http.StatusOK, listed.status)
	assert.Contains(t, string(listed.body), "<Name>bucket-a</Name>")
	assert.NotContains(t, string(listed.body), "<Name>bucket-b</Name>")
	assert.Contains(t, string(listed.body),
		"<ContinuationToken>"+base64.URLEncoding.EncodeToString([]byte("bucket-b"))+"</ContinuationToken>")

	deleted := s.do(call{method: http.MethodDelete, target: bucketAPath, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, deleted.status)
	assert.Empty(t, deleted.header.Get("Content-Type"))

	missing := s.do(call{method: http.MethodDelete, target: bucketAPath, region: regionUSEast1})
	require.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, s3Error{
		Code: "NoSuchBucket", Message: "The specified bucket does not exist", BucketName: "bucket-a",
		RequestID: missing.header.Get("X-Amz-Request-Id"),
	}, missing.err(t))
}

func TestObjectRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		giveHost   string
		givePrefix string
		giveKey    string
		wantKey    string
	}{
		{name: "path style", giveHost: "", givePrefix: "/bucket/", giveKey: "plain.txt", wantKey: "plain.txt"},
		{
			name: "path style, an escaped key", giveHost: "", givePrefix: "/bucket/",
			giveKey: "dir/sub%20file%2B%C3%BC.txt", wantKey: "dir/sub file+ü.txt",
		},
		{
			name: "virtual host", giveHost: "bucket.s3." + localstackHost, givePrefix: "/",
			giveKey: "dir/a%2Fb", wantKey: "dir/a/b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			body := []byte("hello, object")
			sum, etag := md5Of(body)
			target := tt.givePrefix + tt.giveKey

			put := s.do(call{
				method: http.MethodPut, host: tt.giveHost, target: target, region: regionUSEast1, body: body,
				header: map[string]string{
					"Content-Type": "text/plain", "X-Amz-Meta-Color": "Blue",
					headerContentMD5: base64.StdEncoding.EncodeToString(sum),
				},
			})
			require.Equal(t, http.StatusOK, put.status, string(put.body))
			assert.Equal(t, etag, put.header.Get("ETag"))
			assert.NotEmpty(t, put.header.Get("X-Amz-Checksum-Crc64nvme"))

			for _, method := range []string{http.MethodGet, http.MethodHead} {
				got := s.do(call{method: method, host: tt.giveHost, target: target, region: regionUSEast1})
				require.Equal(t, http.StatusOK, got.status, method)
				assert.Equal(t, etag, got.header.Get("ETag"), method)
				assert.Equal(t, "Blue", got.header.Get("X-Amz-Meta-Color"), method)
				assert.Equal(t, "text/plain", got.header.Get("Content-Type"), method)
				assert.Equal(t, "13", got.header.Get("Content-Length"), method)
				assert.Equal(t, "bytes", got.header.Get("Accept-Ranges"), method)
				assert.Equal(t, "AES256", got.header.Get("X-Amz-Server-Side-Encryption"), method)
				assert.NotEmpty(t, got.header.Get("Last-Modified"), method)
				assert.Empty(t, got.header.Get("X-Amz-Checksum-Crc64nvme"), "no checksum unless asked for")
			}

			got := s.do(call{method: http.MethodGet, host: tt.giveHost, target: target, region: regionUSEast1})
			assert.Equal(t, body, got.body)

			// The key the object was stored under is the decoded one, whatever
			// the addressing: path style reaches it too.
			pathStyle := s.do(call{
				method: http.MethodHead, target: "/bucket/" + tt.giveKey, region: regionUSEast1,
			})
			assert.Equal(t, http.StatusOK, pathStyle.status)

			deleted := s.do(call{method: http.MethodDelete, host: tt.giveHost, target: target, region: regionUSEast1})
			assert.Equal(t, http.StatusNoContent, deleted.status)

			gone := s.do(call{method: http.MethodGet, host: tt.giveHost, target: target, region: regionUSEast1})
			require.Equal(t, http.StatusNotFound, gone.status)
			assert.Equal(t, "NoSuchKey", gone.err(t).Code)
			assert.Equal(t, tt.wantKey, gone.err(t).Key)

			headGone := s.do(call{method: http.MethodHead, host: tt.giveHost, target: target, region: regionUSEast1})
			assert.Equal(t, http.StatusNotFound, headGone.status)
			assert.Empty(t, headGone.body)
		})
	}
}

func TestLargeObjectStreamsThroughASpilledBlob(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	body := bytes.Repeat([]byte("0123456789abcdef"), (3<<20)/16)
	_, etag := md5Of(body)

	put := s.do(call{method: http.MethodPut, target: "/bucket/large", region: regionUSEast1, body: body})
	require.Equal(t, http.StatusOK, put.status, string(put.body))
	assert.Equal(t, etag, put.header.Get("ETag"))

	got := s.do(call{method: http.MethodGet, target: "/bucket/large", region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status)
	assert.True(t, bytes.Equal(body, got.body))
}

func TestPutObjectRefusesABodyItsDigestsDoNotMatch(t *testing.T) {
	body := []byte("payload")
	sum, _ := md5Of(body)
	otherSum, _ := md5Of([]byte("other"))

	tests := []struct {
		name       string
		giveHeader map[string]string
		wantStatus int
		wantCode   string
	}{
		{
			name: "a Content-MD5 that is not one", giveHeader: map[string]string{headerContentMD5: "bm90LW1kNQ=="},
			wantStatus: http.StatusBadRequest, wantCode: "InvalidDigest",
		},
		{
			name:       "a Content-MD5 of another body",
			giveHeader: map[string]string{headerContentMD5: base64.StdEncoding.EncodeToString(otherSum)},
			wantStatus: http.StatusBadRequest, wantCode: "BadDigest",
		},
		{
			name: "a CRC32 of another body", giveHeader: map[string]string{"X-Amz-Checksum-Crc32": zeroCRC32},
			wantStatus: http.StatusBadRequest, wantCode: "BadDigest",
		},
		{
			name: "a SHA256 too short", giveHeader: map[string]string{"X-Amz-Checksum-Sha256": zeroCRC32},
			wantStatus: http.StatusBadRequest, wantCode: "InvalidRequest",
		},
		{
			name: "two checksums",
			giveHeader: map[string]string{
				"X-Amz-Checksum-Crc32": zeroCRC32, "X-Amz-Checksum-Crc32c": zeroCRC32,
				headerContentMD5: base64.StdEncoding.EncodeToString(sum),
			},
			wantStatus: http.StatusBadRequest, wantCode: "InvalidRequest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			put := s.do(call{
				method: http.MethodPut, target: objectPath, region: regionUSEast1, body: body, header: tt.giveHeader,
			})
			require.Equal(t, tt.wantStatus, put.status, string(put.body))
			assert.Equal(t, tt.wantCode, put.err(t).Code)

			got := s.do(call{method: http.MethodGet, target: objectPath, region: regionUSEast1})
			assert.Equal(t, http.StatusNotFound, got.status, "a refused body stores nothing")
		})
	}
}

func TestDeleteBucketRefusesANonEmptyBucket(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	put := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte("x")})
	require.Equal(t, http.StatusOK, put.status)

	refused := s.do(call{method: http.MethodDelete, target: "/bucket", region: regionUSEast1})
	require.Equal(t, http.StatusConflict, refused.status)
	assert.Equal(t, "BucketNotEmpty", refused.err(t).Code)
	assert.Equal(t, "bucket", refused.err(t).BucketName)
}

func TestEmptyBodyWithExpectContinue(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	put := s.do(call{
		method: http.MethodPut, target: "/bucket/empty", region: regionUSEast1,
		header: map[string]string{"Expect": "100-continue"},
	})
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	_, etag := md5Of(nil)
	got := s.do(call{method: http.MethodGet, target: "/bucket/empty", region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, etag, got.header.Get("ETag"))
	assert.Empty(t, got.body)
}

func TestRequestsS3DoesNotServeYetAnswer501(t *testing.T) {
	tests := []struct {
		name       string
		giveHost   string
		giveTarget string
		wantBody   string
	}{
		{
			name: "s3control", giveHost: "", giveTarget: "/v20180820/configuration/publicAccessBlock",
			wantBody: "Service 's3control' is not enabled.",
		},
		{
			name: "a website", giveHost: "bucket.s3-website.us-east-1.amazonaws.com", giveTarget: "/index.html",
			wantBody: "for service 's3' not yet implemented",
		},
		{
			name: "an operation not implemented yet", giveHost: "", giveTarget: "/bucket?versioning",
			wantBody: "for service 's3' not yet implemented",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)

			got := s.do(call{method: http.MethodGet, host: tt.giveHost, target: tt.giveTarget, region: regionUSEast1})
			assert.Equal(t, http.StatusNotImplemented, got.status)
			assert.Contains(t, string(got.body), tt.wantBody)
		})
	}
}
