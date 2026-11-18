package s3_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/sigv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Requests and answers LocalStack 8b9a79f gave with
// S3_SKIP_SIGNATURE_VALIDATION=0 (verification/SQ-109/out.txt): SigV4 URLs
// of probe-bucket/k that expired a minute after 2026-11-18T12:18:23Z, signed
// path-style and on the virtual host, and what LocalStack calculated for them.
const (
	pathHost  = "s3.localhost.localstack.cloud:4566"
	vhostHost = "probe-bucket.s3.localhost.localstack.cloud:4566"

	goldenQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=test%2F20261118%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20261118T121823Z&X-Amz-SignedHeaders=host&X-Amz-Expires=60"
	goldenCanonicalQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=test%2F20261118%2Fus-east-1%2Fs3%2F" +
		"aws4_request&X-Amz-Date=20261118T121823Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host"
	goldenScope = "AWS4-HMAC-SHA256\n20261118T121823Z\n20261118/us-east-1/s3/aws4_request\n"

	pathSignature  = "c89d4b75d3983c367889fb47776e11ec088986e3fdd49ac609a67c3641389c8f"
	vhostSignature = "7914f2493c42ff45da17ee8c28ec59ebcf245c2b535169a1b541e0981c4a500e"

	presignBucket = "/presign"
	presignKey    = "table1-partitioned/date=2023-06-28/test.csv"
	presignVhost  = "presign.s3.localhost.localstack.cloud:4566"
	formHost      = "presign.s3." + localstackHost
	contentType   = "Content-Type"
	textPlain     = "text/plain"

	fieldKey           = "key"
	fieldBucket        = "bucket"
	fieldETag          = "etag"
	fieldAccessKeyID   = "AWSAccessKeyId"
	accessKeyTest      = "test"
	headerCacheControl = "Cache-Control"
	headerContentSHA   = "X-Amz-Content-Sha256"
	conditionKey       = "$key"
	conditionLength    = "content-length-range"
)

// presignError is an S3 error with the members a presign check adds.
type presignError struct {
	Code                  string `xml:"Code"`
	Message               string `xml:"Message"`
	AWSAccessKeyID        string `xml:"AWSAccessKeyId"`
	CanonicalRequest      string `xml:"CanonicalRequest"`
	CanonicalRequestBytes string `xml:"CanonicalRequestBytes"`
	StringToSign          string `xml:"StringToSign"`
	StringToSignBytes     string `xml:"StringToSignBytes"`
	SignatureProvided     string `xml:"SignatureProvided"`
	Expires               string `xml:"Expires"`
	AmzExpires            string `xml:"X-Amz-Expires"`
	HostID                string `xml:"HostId"`
	ProposedSize          string `xml:"ProposedSize"`
}

func (r reply) presignErr(t *testing.T) presignError {
	t.Helper()

	var e presignError
	require.NoError(t, xml.Unmarshal(r.body, &e), string(r.body))

	return e
}

// newPresignServer is newServer with S3_SKIP_SIGNATURE_VALIDATION=0 and the
// gateway on port 4566. It returns the gateway's handler too, whose response
// headers keep the case S3 wrote them in.
func newPresignServer(t *testing.T) (*server, http.Handler) {
	t.Helper()

	provider := s3.New(func() s3.Settings {
		return s3.Settings{LocalstackHost: localstackHost, ValidateSignatures: true, GatewayPort: 4566}
	}, s3.NewEphemeral(t.TempDir()))
	t.Cleanup(provider.Close)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	s := &server{t: t, url: srv.URL}

	got := s.do(call{method: http.MethodPut, target: presignBucket, region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return s, gw.Handler()
}

// TestPresignedURLReplaysLocalStackVectors replays URLs LocalStack checked.
// Their signatures match, so they fail only on their expiry, which is checked
// last; with a changed signature they fail with exactly what LocalStack
// calculated.
func TestPresignedURLReplaysLocalStackVectors(t *testing.T) {
	t.Parallel()

	s, _ := newPresignServer(t)

	tests := []struct {
		name      string
		host      string
		target    string
		signature string
		wantPath  string
		wantHash  string
	}{
		{
			name: "path style", host: pathHost, target: "/probe-bucket/k", signature: pathSignature,
			wantPath: "/probe-bucket/k", wantHash: "ab5b0ce12fb03277066954f84708591f098cf626f172e3a0d9e1344ecedc40d0",
		},
		{
			name: "virtual host", host: vhostHost, target: "/k", signature: vhostSignature,
			wantPath: "/k", wantHash: "cf477baaf6c05e1fd90fc67cf11fcccb47b5e2ce1734ade682fe00a24e75b194",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name+" signature matches and has expired", func(t *testing.T) {
			t.Parallel()

			got := s.do(call{
				method: http.MethodGet, host: tt.host,
				target: tt.target + "?" + goldenQuery + "&X-Amz-Signature=" + tt.signature,
			})
			require.Equal(t, http.StatusForbidden, got.status, string(got.body))

			e := got.presignErr(t)
			assert.Equal(t, codeAccessDenied, e.Code)
			assert.Equal(t, "Request has expired", e.Message)
			assert.Equal(t, "2026-11-18T12:19:23.000Z", e.Expires)
			assert.Equal(t, "60", e.AmzExpires)
		})

		t.Run(tt.name+" changed signature", func(t *testing.T) {
			t.Parallel()

			got := s.do(call{
				method: http.MethodGet, host: tt.host,
				target: tt.target + "?" + goldenQuery + "&X-Amz-Signature=AAAA" + tt.signature,
			})
			require.Equal(t, http.StatusForbidden, got.status, string(got.body))

			e := got.presignErr(t)
			assert.Equal(t, "SignatureDoesNotMatch", e.Code)
			assert.Equal(t, accessKeyTest, e.AWSAccessKeyID)
			assert.Equal(t, "GET\n"+tt.wantPath+"\n"+goldenCanonicalQuery+"\nhost:"+tt.host+"\n\nhost\nUNSIGNED-PAYLOAD",
				e.CanonicalRequest)
			assert.Equal(t, goldenScope+tt.wantHash, e.StringToSign)
			assert.Equal(t, "AAAA"+tt.signature, e.SignatureProvided)
			assert.True(t, strings.HasPrefix(e.StringToSignBytes, "41 5753 342D 484D 4143"), e.StringToSignBytes)
			assert.NotEmpty(t, e.HostID)
		})
	}
}

func TestPresignedURLRoundTrip(t *testing.T) {
	t.Parallel()

	s, handler := newPresignServer(t)
	body := []byte("a,b\n1,2\n")
	now := time.Now()

	put := s.do(call{
		method: http.MethodPut, host: presignVhost, body: body,
		target: "/" + presignKey + "?" + signV4(http.MethodPut, presignVhost, "/"+presignKey, now),
	})
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	t.Run("SigV4 on the virtual host", func(t *testing.T) {
		t.Parallel()

		got := s.do(call{
			method: http.MethodGet, host: presignVhost,
			target: "/" + presignKey + "?" + signV4(http.MethodGet, presignVhost, "/"+presignKey, now),
		})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		require.Equal(t, body, got.body)
	})

	t.Run("SigV4 signed for another port of the host", func(t *testing.T) {
		t.Parallel()

		target := presignBucket + "/" + presignKey + "?" +
			signV4(http.MethodGet, "s3.localhost.localstack.cloud", presignBucket+"/"+presignKey, now)
		got := s.do(call{method: http.MethodGet, host: pathHost, target: target})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
	})

	t.Run("SigV4 that has expired", func(t *testing.T) {
		t.Parallel()

		target := presignBucket + "/" + presignKey + "?" +
			signV4(http.MethodGet, pathHost, presignBucket+"/"+presignKey, now.Add(-time.Hour))
		got := s.do(call{method: http.MethodGet, host: pathHost, target: target})
		require.Equal(t, http.StatusForbidden, got.status, string(got.body))
		require.Equal(t, "Request has expired", got.presignErr(t).Message)
	})

	t.Run("SigV4 with an x-amz header it did not sign", func(t *testing.T) {
		t.Parallel()

		target := presignBucket + "/" + presignKey + "?" +
			signV4(http.MethodGet, pathHost, presignBucket+"/"+presignKey, now)
		got := s.do(call{
			method: http.MethodGet, host: pathHost, target: target,
			header: map[string]string{"X-Amz-Meta-Wrong": "w", headerContentSHA: sigv4.UnsignedPayload},
		})
		require.Equal(t, http.StatusForbidden, got.status, string(got.body))
		require.Contains(t, string(got.body), "<HeadersNotSigned>x-amz-meta-wrong</HeadersNotSigned>")
	})

	t.Run("SigV2", func(t *testing.T) {
		t.Parallel()

		path := presignBucket + "/" + presignKey
		got := s.do(call{method: http.MethodGet, host: pathHost, target: path + "?" + signV2(path, now.Add(time.Minute))})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		require.Equal(t, body, got.body)
	})

	t.Run("SigV2 with a changed signature", func(t *testing.T) {
		t.Parallel()

		path := presignBucket + "/" + presignKey
		expires := now.Add(time.Minute)
		query := strings.Replace(signV2(path, expires), "Signature=", "Signature=AAAA", 1)

		got := s.do(call{method: http.MethodGet, host: pathHost, target: path + "?" + query})
		require.Equal(t, http.StatusForbidden, got.status, string(got.body))

		e := got.presignErr(t)
		require.Equal(t, "SignatureDoesNotMatch", e.Code)
		require.Equal(t, "GET\n\n\n"+strconv.FormatInt(expires.Unix(), 10)+"\n"+sigv4.EscapePath(path), e.StringToSign)
	})

	t.Run("GetObject answers ETag in S3's case", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/"+presignKey+"?"+signV4(http.MethodGet, presignVhost, "/"+presignKey, now), nil)
		req.Host = presignVhost

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Header(), "ETag")
		require.NotContains(t, rec.Header(), "Etag")
	})
}

func TestPostObject(t *testing.T) {
	t.Parallel()

	s := newServer(t)

	created := s.do(call{method: http.MethodPut, target: presignBucket, region: regionUSEast1})
	require.Equal(t, http.StatusOK, created.status, string(created.body))

	file := []byte("hello post")

	tests := []struct {
		name       string
		fields     [][2]string
		wantStatus int
		wantCode   string
		wantStored string
	}{
		{
			name: "no status answers 204", fields: [][2]string{{fieldKey, "k204"}},
			wantStatus: http.StatusNoContent, wantStored: "k204",
		},
		{
			name: "a policy the form meets",
			fields: signedForm(t, "kpolicy", map[string]string{fieldBucket: "presign"},
				[]any{"starts-with", conditionKey, "kp"}, []any{conditionLength, 1, 100}),
			wantStatus: http.StatusNoContent, wantStored: "kpolicy",
		},
		{
			name:       "a file over content-length-range",
			fields:     signedForm(t, "kbig", []any{conditionLength, 1, 5}),
			wantStatus: http.StatusBadRequest, wantCode: "EntityTooLarge",
		},
		{
			name:       "a condition the form fails",
			fields:     signedForm(t, "kfail", []any{"eq", conditionKey, "other"}),
			wantStatus: http.StatusForbidden, wantCode: codeAccessDenied,
		},
		{
			name:       "a policy without its signature fields",
			fields:     [][2]string{{fieldKey, "knosig"}, {"policy", policy(t, nil)}},
			wantStatus: http.StatusForbidden, wantCode: codeAccessDenied,
		},
		{
			name: "a form without a key field", fields: [][2]string{{"acl", "private"}},
			wantStatus: http.StatusBadRequest, wantCode: cfgInvalidArgument,
		},
		{
			name: "fields over 20 KiB before the file", fields: manyFields(30, 1<<10),
			wantStatus: http.StatusBadRequest, wantCode: "MaxPostPreDataLengthExceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := postForm(t, s, formHost, "/", tt.fields, file)
			require.Equal(t, tt.wantStatus, got.status, string(got.body))

			if tt.wantCode != "" {
				require.Equal(t, tt.wantCode, got.presignErr(t).Code)

				return
			}

			require.Empty(t, got.body)
			require.NotEmpty(t, got.header.Get("ETag"))

			stored := s.do(call{method: http.MethodGet, target: presignBucket + "/" + tt.wantStored, region: regionUSEast1})
			require.Equal(t, http.StatusOK, stored.status, string(stored.body))
			require.Equal(t, file, stored.body)
		})
	}

	t.Run("201 answers a PostResponse", func(t *testing.T) {
		t.Parallel()

		got := postForm(t, s, formHost, "/", [][2]string{{fieldKey, "k201"}, {"success_action_status", "201"}}, file)
		require.Equal(t, http.StatusCreated, got.status, string(got.body))

		var resp struct {
			Bucket string `xml:"Bucket"`
			Key    string `xml:"Key"`
			ETag   string `xml:"ETag"`
		}
		require.NoError(t, xml.Unmarshal(got.body, &resp), string(got.body))
		require.Equal(t, "presign", resp.Bucket)
		require.Equal(t, "k201", resp.Key)
		require.Equal(t, got.header.Get("ETag"), resp.ETag)
	})

	t.Run("success_action_redirect answers 303 with no body", func(t *testing.T) {
		t.Parallel()

		got := postForm(t, s, formHost, "/",
			[][2]string{{fieldKey, "k303"}, {"success_action_redirect", "http://example.com/done?a=1"}}, file)
		require.Equal(t, http.StatusSeeOther, got.status, string(got.body))
		require.Empty(t, got.body)

		location, err := url.Parse(got.header.Get("Location"))
		require.NoError(t, err)
		require.Equal(t, "example.com", location.Host)
		want := url.Values{"a": {"1"}, fieldKey: {"k303"}, fieldBucket: {"presign"}, fieldETag: {got.header.Get("ETag")}}
		require.Equal(t, want, location.Query())
	})

	t.Run("path style", func(t *testing.T) {
		t.Parallel()

		got := postForm(t, s, localstackHost, presignBucket, [][2]string{{fieldKey, "kpath"}}, file)
		require.Equal(t, http.StatusNoContent, got.status, string(got.body))
	})

	t.Run("not a form", func(t *testing.T) {
		t.Parallel()

		got := s.do(call{
			method: http.MethodPost, host: formHost, target: "/", body: file,
			header: map[string]string{contentType: textPlain},
		})
		require.Equal(t, http.StatusPreconditionFailed, got.status, string(got.body))
	})

	t.Run("form fields keep their RFC 2047 words and go out in ISO-8859-1", func(t *testing.T) {
		t.Parallel()

		got := postForm(t, s, formHost, "/", [][2]string{
			{fieldKey, "kunicode"}, {headerCacheControl, "a—é"}, {"x-amz-meta-b", "=?UTF-8?B?YWJj?="},
		}, file)
		require.Equal(t, http.StatusNoContent, got.status, string(got.body))

		head := s.do(call{method: http.MethodHead, target: presignBucket + "/kunicode", region: regionUSEast1})
		require.Equal(t, http.StatusOK, head.status)
		require.Equal(t, "a \xe9", head.header.Get(headerCacheControl))
		require.Equal(t, "=?UTF-8?B?YWJj?=", head.header.Get("X-Amz-Meta-B"))
	})
}

// postForm posts a browser upload form: fields in order, then file.
func postForm(t *testing.T, s *server, host, target string, fields [][2]string, file []byte) reply {
	t.Helper()

	var body bytes.Buffer

	mw := multipart.NewWriter(&body)
	for _, field := range fields {
		require.NoError(t, mw.WriteField(field[0], field[1]))
	}

	part, err := mw.CreateFormFile("file", "f.txt")
	require.NoError(t, err)

	_, err = part.Write(file)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.url+target, &body)
	require.NoError(t, err)

	req.Host = host
	req.Header.Set(contentType, mw.FormDataContentType())

	// A 303 is the answer under test, not a page to follow.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return reply{status: resp.StatusCode, header: resp.Header, body: got}
}

// policy is a base64 POST policy that expires in an hour.
func policy(t testing.TB, conditions []any) string {
	t.Helper()

	document, err := json.Marshal(map[string]any{
		"expiration": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		"conditions": conditions,
	})
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(document)
}

// signedForm is the fields of a SigV2 form for key with a policy of
// conditions. POST signatures are not checked, as in LocalStack.
func signedForm(t testing.TB, key string, conditions ...any) [][2]string {
	t.Helper()

	return [][2]string{
		{fieldKey, key}, {fieldAccessKeyID, accessKeyTest}, {"signature", "x"}, {"policy", policy(t, conditions)},
	}
}

func manyFields(n, size int) [][2]string {
	fields := make([][2]string, 0, n+1)
	fields = append(fields, [2]string{fieldKey, "k"})

	for i := range n {
		fields = append(fields, [2]string{"x-amz-meta-f" + strconv.Itoa(i), strings.Repeat("v", size)})
	}

	return fields
}

// signV4 is the query of a URL that botocore presigns for host with the
// "test" secret, valid for a minute and signing only the host.
func signV4(method, host, path string, at time.Time) string {
	amzDate := at.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	query := sigv4.CanonicalQuery([]sigv4.Pair{
		{Key: "X-Amz-Algorithm", Value: sigv4.Algorithm},
		{Key: "X-Amz-Credential", Value: "test/" + day + "/us-east-1/s3/aws4_request"},
		{Key: "X-Amz-Date", Value: amzDate},
		{Key: "X-Amz-Expires", Value: "60"},
		{Key: "X-Amz-SignedHeaders", Value: "host"},
	})
	block, signed := sigv4.CanonicalHeaders([]sigv4.Header{{Name: "host", Value: host}})
	canonical := sigv4.CanonicalRequest(method, sigv4.EscapePath(path), query, block, signed, sigv4.UnsignedPayload)
	key := sigv4.SigningKey(accessKeyTest, day, regionUSEast1, "s3")

	return query + "&X-Amz-Signature=" +
		sigv4.Signature(key, sigv4.StringToSign(amzDate, sigv4.Scope(day, regionUSEast1, "s3"), canonical))
}

// signV2 is the query of a SigV2 URL for a GET of path that botocore presigns
// with the "test" secret.
func signV2(path string, expires time.Time) string {
	stamp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha1.New, []byte(accessKeyTest))
	mac.Write([]byte("GET\n\n\n" + stamp + "\n" + sigv4.EscapePath(path)))

	return url.Values{
		fieldAccessKeyID: {accessKeyTest},
		"Signature":      {base64.StdEncoding.EncodeToString(mac.Sum(nil))},
		"Expires":        {stamp},
	}.Encode()
}
