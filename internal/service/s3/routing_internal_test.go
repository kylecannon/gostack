package s3

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHost          = "localhost.localstack.cloud:4566"
	gatewayHost       = "localhost:4566"
	bucketName        = "bucket"
	bucketPath        = "/" + bucketName
	objectPath        = bucketPath + "/key"
	healthPath        = "/_localstack/health"
	headerContentType = "Content-Type"
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()

	blobs := NewEphemeral(t.TempDir())
	t.Cleanup(blobs.Close)

	return New(func() Settings { return Settings{LocalstackHost: testHost} }, blobs)
}

func TestClaimHost(t *testing.T) {
	tests := []struct {
		name     string
		giveHost string
		want     bool
	}{
		{name: "S3's own host", giveHost: "s3." + testHost, want: true},
		{name: "S3's own host without a port", giveHost: "s3.localhost.localstack.cloud", want: true},
		{name: "a virtual host", giveHost: "bucket.s3." + testHost, want: true},
		{name: "a regional AWS virtual host", giveHost: "bucket.s3.us-west-2.amazonaws.com", want: true},
		{name: "a website host", giveHost: "bucket.s3-website.us-east-1.amazonaws.com", want: true},
		{name: "a dashed website host", giveHost: "bucket.s3-website-us-east-1.amazonaws.com", want: true},
		{name: "the gateway's own host", giveHost: gatewayHost, want: false},
		{name: "another service's host", giveHost: "sqs.us-east-1." + testHost, want: false},
		{name: "s3control's host prefix", giveHost: "s3control." + testHost, want: false},
	}

	p := newTestProvider(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, p.claimHost(tt.giveHost))
		})
	}
}

func TestPreprocessOwnsS3RequestsAndRewritesVirtualHosts(t *testing.T) {
	tests := []struct {
		name        string
		giveHost    string
		giveTarget  string
		giveBucket  string
		wantHost    string
		wantPath    string
		wantEscaped string
		wantOwner   string
		// giveMarked is the gateway's mark for the service that claimed the host.
		giveMarked bool
	}{
		{
			name: "a key with escapes", giveHost: "bucket.s3." + testHost, giveTarget: "/dir%2Fname%20x?versionId=1",
			giveBucket: "", wantHost: "s3." + testHost, wantPath: "/bucket/dir/name x",
			wantEscaped: "/bucket/dir%2Fname%20x", wantOwner: serviceName,
		},
		{
			name: "the bucket itself", giveHost: "bucket.s3." + testHost, giveTarget: "/", giveBucket: "",
			wantHost: "s3." + testHost, wantPath: "/bucket/", wantEscaped: "/bucket/", wantOwner: serviceName,
		},
		{
			name: "a dotted bucket on a regional host", giveHost: "my.dotted.bucket.s3.eu-west-1.amazonaws.com",
			giveTarget: "/k", giveBucket: "", wantHost: "s3.eu-west-1.amazonaws.com", wantPath: "/my.dotted.bucket/k",
			wantEscaped: "/my.dotted.bucket/k", wantOwner: serviceName,
		},
		{
			name: "a path on S3's own host, which the gateway marked", giveHost: "s3." + testHost,
			giveTarget: objectPath, giveBucket: "", giveMarked: true,
			wantHost: "s3." + testHost, wantPath: objectPath, wantEscaped: objectPath, wantOwner: serviceName,
		},
		{
			name: "a path on S3's own host is not owned by the host alone", giveHost: "s3." + testHost,
			giveTarget: objectPath, giveBucket: "",
			wantHost: "s3." + testHost, wantPath: objectPath, wantEscaped: objectPath, wantOwner: "",
		},
		{
			name: "path style to an existing bucket", giveHost: gatewayHost, giveTarget: objectPath,
			giveBucket: bucketName, wantHost: gatewayHost, wantPath: objectPath, wantEscaped: objectPath,
			wantOwner: serviceName,
		},
		{
			name: "path style to an unknown bucket", giveHost: gatewayHost, giveTarget: objectPath, giveBucket: "",
			wantHost: gatewayHost, wantPath: objectPath, wantEscaped: objectPath, wantOwner: "",
		},
		{
			name: "the gateway's own path", giveHost: "bucket.s3." + testHost, giveTarget: healthPath,
			giveBucket: "", wantHost: "bucket.s3." + testHost, wantPath: healthPath, wantEscaped: healthPath,
			wantOwner: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t)
			if tt.giveBucket != "" {
				p.names.claim(tt.giveBucket, "000000000000")
			}

			original := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.giveTarget, nil)
			original.Host = tt.giveHost

			if tt.giveMarked {
				original = service.Own(original, serviceName)
			}

			r := original
			if rewritten := p.preprocess(original); rewritten != nil {
				r = rewritten
			}

			assert.Equal(t, tt.wantHost, r.Host)
			assert.Equal(t, tt.wantPath, r.URL.Path)
			assert.Equal(t, tt.wantEscaped, r.URL.EscapedPath())
			assert.Equal(t, tt.wantOwner, service.OwnerOf(r))
			assert.Equal(t, tt.giveHost, original.Host, "the gateway's request is left as it was")
		})
	}
}

func TestIsControlCall(t *testing.T) {
	tests := []struct {
		name       string
		giveHost   string
		givePath   string
		giveBucket string
		want       bool
	}{
		{name: "an s3control path", giveHost: gatewayHost, givePath: "/v20180820/configuration/x", want: true},
		{name: "the s3control root", giveHost: gatewayHost, givePath: "/v20180820", want: true},
		{name: "a bucket that starts like it", giveHost: gatewayHost, givePath: "/v20180820abc/k", want: false},
		{name: "a key under a virtual host", giveHost: "bucket.s3." + testHost, givePath: "/v20180820/x", want: false},
		{name: "a control path on S3's own host", giveHost: "s3." + testHost, givePath: "/v20180820/x", want: false},
		{
			name: "a control path on an s3-control host, which S3's pattern claims", want: false,
			giveHost: "000000000000.s3-control." + testHost, givePath: "/v20180820/configuration/publicAccessBlock",
		},
		{
			name: "an existing bucket named like it", giveHost: gatewayHost, givePath: "/v20180820/key",
			giveBucket: "v20180820", want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t)
			if tt.giveBucket != "" {
				p.names.claim(tt.giveBucket, "000000000000")
			}

			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.givePath, nil)
			r.Host = tt.giveHost

			// The gateway marks the service that claims the host before its
			// Preprocess runs.
			if p.claimHost(r.Host) {
				r = service.Own(r, serviceName)
			}

			if rewritten := p.preprocess(r); rewritten != nil {
				r = rewritten
			}

			assert.Equal(t, tt.want, isControlCall(r))
		})
	}
}

// multipartBody returns a form with a text field for each of fields and then a
// file under fileField.
func multipartBody(t *testing.T, fields []string, fileField string) (string, string) {
	t.Helper()

	var buf bytes.Buffer

	w := multipart.NewWriter(&buf)
	for _, name := range fields {
		require.NoError(t, w.WriteField(name, "value"))
	}

	file, err := w.CreateFormFile(fileField, "upload.bin")
	require.NoError(t, err)

	_, err = file.Write(bytes.Repeat([]byte("x"), 2*sniffLimit))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return buf.String(), w.FormDataContentType()
}

func TestFallbackPortsLegacyS3Rules(t *testing.T) {
	formWithKey, formWithKeyType := multipartBody(t, []string{"policy", "key"}, "file")
	formFileKey, formFileKeyType := multipartBody(t, []string{"policy"}, "key")
	formPresigned, formPresignedType := multipartBody(t, []string{"X-Amz-Signature"}, "file")

	tests := []struct {
		name       string
		giveMethod string
		giveTarget string
		giveHost   string
		giveHeader map[string]string
		giveBody   string
		want       bool
	}{
		{name: "GET of a key", giveMethod: http.MethodGet, giveTarget: objectPath, want: true},
		{name: "HEAD of a bucket", giveMethod: http.MethodHead, giveTarget: bucketPath, want: true},
		{name: "GET of the root", giveMethod: http.MethodGet, giveTarget: "/", want: false},
		{name: "PUT of a bucket", giveMethod: http.MethodPut, giveTarget: bucketPath, want: true},
		{
			name: "urlencoded POST with a key to a bucket", giveMethod: http.MethodPost, giveTarget: bucketPath,
			giveHeader: map[string]string{headerContentType: "application/x-www-form-urlencoded"}, giveBody: "key=a",
			want: true,
		},
		{
			name: "multipart POST with a key field to a bucket", giveMethod: http.MethodPost, giveTarget: bucketPath,
			giveHeader: map[string]string{headerContentType: formWithKeyType}, giveBody: formWithKey, want: true,
		},
		{
			name: "multipart POST whose key is a file", giveMethod: http.MethodPost, giveTarget: bucketPath,
			giveHeader: map[string]string{headerContentType: formFileKeyType}, giveBody: formFileKey, want: false,
		},
		{
			name: "multipart POST with a presign field", giveMethod: http.MethodPost, giveTarget: "/bucket/k",
			giveHeader: map[string]string{headerContentType: formPresignedType}, giveBody: formPresigned, want: true,
		},
		{name: "POST with a key to an object", giveMethod: http.MethodPost, giveTarget: "/bucket/k?key=a", want: false},
		{
			name: "the CLI unsigned", giveMethod: http.MethodDelete, giveTarget: objectPath,
			giveHeader: map[string]string{"User-Agent": "aws-cli/2.15.0 Python/3.11"}, want: true,
		},
		{name: "unsigned DELETE of an unknown bucket", giveMethod: http.MethodDelete, giveTarget: objectPath},
		{
			name: "a presigned URL", giveMethod: http.MethodDelete, giveTarget: "/bucket/key?X-Amz-Signature=abc",
			want: true,
		},
		{
			name: "DeleteObjects", giveMethod: http.MethodPost, giveTarget: "/bucket?delete",
			giveBody: "<Delete><Object><Key>a</Key></Object></Delete>", want: true,
		},
		{
			name: "a delete body with no key", giveMethod: http.MethodPost, giveTarget: "/bucket?delete",
			giveBody: "<Delete><Quiet>true</Quiet></Delete>", want: false,
		},
		{name: "PUT of a nested key", giveMethod: http.MethodPut, giveTarget: "/bucket/a/b", want: true},
		{
			name: "SigV2", giveMethod: http.MethodDelete, giveTarget: objectPath,
			giveHeader: map[string]string{"Authorization": "AWS AKIDEXAMPLE:c2lnbmF0dXJl"}, want: true,
		},
		{
			name: "a virtual host", giveMethod: http.MethodDelete, giveTarget: "/key",
			giveHost: "bucket.s3.amazonaws.com", want: true,
		},
		{
			name: "an SQS query call", giveMethod: http.MethodPost, giveTarget: "/",
			giveHeader: map[string]string{headerContentType: "application/x-www-form-urlencoded"},
			giveBody:   "Action=ListQueues", want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), tt.giveMethod, tt.giveTarget, strings.NewReader(tt.giveBody))
			if tt.giveHost != "" {
				r.Host = tt.giveHost
			}

			for name, value := range tt.giveHeader {
				r.Header.Set(name, value)
			}

			// The gateway sniffs at step 4 and reads a urlencoded form while it
			// detects; the body must reach the service whole after both.
			if sniffed := sniff(r); sniffed != nil {
				r = sniffed
			}

			_, _ = query.RequestForm(r)

			assert.Equal(t, tt.want, fallback(r))

			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			assert.Equal(t, tt.giveBody, string(body))
		})
	}
}
