package s3

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/sigv4"
	"github.com/stretchr/testify/require"
)

// Vectors LocalStack 8b9a79f answered with S3_SKIP_SIGNATURE_VALIDATION=0
// (verification/SQ-109/out.txt): the SigV2 string to sign of a GET of
// probe-bucket/k, the signature of it with the secret "test", and its
// StringToSignBytes member.
const (
	goldenStringToSignV2 = "GET\n\n\n1795004361\n/probe-bucket/k"
	goldenSignatureV2    = "vMCCHmZzGpblAgHguCkxh3c0FEc="
	goldenBytesV2        = "4745 540A 0A0A 3137 3935 3030 3433 3631 0A2F 7072 6F62 652D 6275 636B 6574 2F6B"
	goldenExpiresV2      = 1795004361
	goldenHost           = "s3.localhost.localstack.cloud:4566"
	presignedPath        = "/b/k"
	wantDenied           = "AccessDenied"
)

func Test_hexBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		give string
		want string
	}{
		{name: "no bytes", give: "", want: ""},
		{name: "even length groups two bytes", give: goldenStringToSignV2, want: goldenBytesV2},
		{
			name: "odd length leads with one byte",
			give: "AWS4-HMAC-SHA256\n",
			want: "41 5753 342D 484D 4143 2D53 4841 3235 360A",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, hexBytes(tt.give))
		})
	}
}

func Test_signV2(t *testing.T) {
	t.Parallel()

	t.Run("signs the string LocalStack signed", func(t *testing.T) {
		t.Parallel()

		got := stringToSignV2(http.MethodGet, map[string]string{}, goldenExpiresV2, "/probe-bucket/k")
		require.Equal(t, goldenStringToSignV2, got)
		require.Equal(t, goldenSignatureV2, signV2(presignSecret, got))
	})

	t.Run("x-amz headers sort after the expiry", func(t *testing.T) {
		t.Parallel()

		headers := map[string]string{"x-amz-meta-wrong": "w", "x-amz-meta-foo": " bar ", "content-type": "text/plain"}
		require.Equal(t, "PUT\n\ntext/plain\n1795004361\nx-amz-meta-foo:bar\nx-amz-meta-wrong:w\n/probe-bucket/k2",
			stringToSignV2(http.MethodPut, headers, goldenExpiresV2, "/probe-bucket/k2"))
	})

	t.Run("subresources follow the path, a bare name without a value", func(t *testing.T) {
		t.Parallel()

		args := parseQueryArgs("versionId=v1&uploads&Signature=s&acl")
		require.Equal(t, "/b/k%20x?acl&uploads&versionId=v1", resourceV2("/b/k x", args))
	})
}

func Test_secretOf(t *testing.T) {
	t.Parallel()

	require.Equal(t, presignSecret, secretOf("AKIAIOSFODNN7EXAMPLE"))
	require.Equal(t, internalSecret, secretOf("949334387222"))
	require.Equal(t, internalSecret, secretOf(internalSecret))
}

func Test_hostPermutations(t *testing.T) {
	t.Parallel()

	const https = "h:443"

	require.Equal(t, []string{"h:80", https, "h"}, hostPermutations("h:4566", 4566))
	require.Equal(t, []string{"h:80", https, "h:4566"}, hostPermutations("h", 4566))
	require.Equal(t, []string{https, "h"}, hostPermutations("h:80", 0))
}

func Test_checkPresigned(t *testing.T) {
	t.Parallel()

	validating := New(func() Settings { return Settings{ValidateSignatures: true, GatewayPort: 4566} }, nil)
	skipping := New(func() Settings { return Settings{} }, nil)
	now := time.Now().UTC()

	tests := []struct {
		name     string
		p        *Provider
		give     string
		wantCode string
		wantMsg  string
	}{
		{
			name: "a fresh SigV4 URL passes", p: validating,
			give: presignV4(goldenHost, now, 60),
		},
		{
			name: "a fresh SigV4 URL signed with port 443 passes", p: validating,
			give: presignV4("s3.localhost.localstack.cloud:443", now, 60),
		},
		{
			name: "a SigV4 URL that has expired is refused", p: validating,
			give:     presignV4(goldenHost, now.Add(-time.Hour), 60),
			wantCode: wantDenied, wantMsg: requestExpired,
		},
		{
			name: "an expired SigV4 URL passes when signatures are skipped", p: skipping,
			give: presignV4(goldenHost, now.Add(-time.Hour), 60),
		},
		{
			name: "a tampered SigV4 signature is refused", p: validating,
			give:     strings.Replace(presignV4(goldenHost, now, 60), "Signature=", "Signature=0", 1),
			wantCode: "SignatureDoesNotMatch", wantMsg: signatureMismatch,
		},
		{
			name: "SigV4 over a week is refused", p: validating,
			give:     presignV4(goldenHost, now, maxPresignExpires+1),
			wantCode: "AuthorizationQueryParametersError",
		},
		{
			name: "a SigV4 URL missing a parameter is refused even when skipping", p: skipping,
			give:     "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=test%2F20261118%2Fus-east-1%2Fs3%2Faws4_request",
			wantCode: "AuthorizationQueryParametersError",
		},
		{
			name: "a fresh SigV2 URL passes", p: validating,
			give: presignV2(now.Add(time.Minute)),
		},
		{
			name: "an expired SigV2 URL is refused", p: validating,
			give:     presignV2(now.Add(-time.Minute)),
			wantCode: wantDenied, wantMsg: requestExpired,
		},
		{
			name: "a SigV2 URL missing its signature is refused", p: skipping,
			give: "AWSAccessKeyId=test&Expires=1", wantCode: wantDenied,
		},
		{name: "a query that is no presign passes", p: validating, give: "versionId=1&acl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			refused := tt.p.checkPresigned(presignedRequest(tt.give))
			if tt.wantCode == "" {
				require.Nil(t, refused)

				return
			}

			require.NotNil(t, refused)
			require.Equal(t, tt.wantCode, refused.err.Code)

			if tt.wantMsg != "" {
				require.Equal(t, tt.wantMsg, refused.err.Message)
			}
		})
	}
}

// Test_presignedV4 checks that a presigned query's x-amz-* parameters move
// into the headers and that an unsigned x-amz-content-sha256 is left out of
// HeadersNotSigned and the payload hash, as S3 ignores it.
func Test_presignedV4(t *testing.T) {
	t.Parallel()

	r := presignedRequest("X-Amz-SignedHeaders=host&x-amz-meta-a=1&x-amz-checksum-crc32=x")
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	r.Header.Set("X-Amz-Meta-Wrong", "w")

	req := newPresignedV4(r, parseQueryArgs(r.URL.RawQuery))
	require.Equal(t, []member{{"x-amz-meta-a", "1"}}, req.moved)
	require.Equal(t, []string{"x-amz-meta-wrong"}, req.unsigned)
	require.Equal(t, sigv4.UnsignedPayload, req.payload)

	signed := presignedRequest("X-Amz-SignedHeaders=host%3Bx-amz-content-sha256")
	signed.Header.Set("X-Amz-Content-Sha256", "abc")
	require.Equal(t, "abc", newPresignedV4(signed, parseQueryArgs(signed.URL.RawQuery)).payload)
}

func Test_readForm(t *testing.T) {
	t.Parallel()

	t.Run("many empty fields are refused after a bounded read", func(t *testing.T) {
		t.Parallel()

		const parts = 100_000

		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)

		var (
			wg      sync.WaitGroup
			written int
		)

		wg.Go(func() {
			for i := range parts {
				if mw.WriteField(fmt.Sprintf("field-%06d", i), "") != nil {
					return
				}

				written = i + 1
			}

			_ = pw.Close()
		})

		body := &countingReader{r: pr}
		_, _, err := readForm(body, mw.Boundary())
		_ = pr.CloseWithError(io.ErrClosedPipe)

		wg.Wait()

		var refused *awserr.Error
		require.ErrorAs(t, err, &refused)
		require.Equal(t, "MaxPostPreDataLengthExceeded", refused.Code)
		// The multipart reader reads ahead at most its 4 KiB buffer.
		require.LessOrEqual(t, body.n, int64(maxPreData+8<<10))
		require.Less(t, written, parts/100)
	})

	t.Run("fields just under the limit reach the file", func(t *testing.T) {
		t.Parallel()

		var body bytes.Buffer

		mw := multipart.NewWriter(&body)
		require.NoError(t, mw.WriteField("Key", "k"))
		require.NoError(t, mw.WriteField("policy", strings.Repeat("a", maxPreData-1024)))

		file, err := mw.CreateFormFile("file", "f.txt")
		require.NoError(t, err)

		_, err = file.Write(bytes.Repeat([]byte("x"), 64<<10))
		require.NoError(t, err)
		require.NoError(t, mw.Close())

		form, part, err := readForm(&body, mw.Boundary())
		require.NoError(t, err)
		require.NotNil(t, part)
		require.Equal(t, "k", form["key"])
		require.Equal(t, "f.txt", part.FileName())
	})
}

func Test_latin1(t *testing.T) {
	t.Parallel()

	require.Equal(t, "plain", latin1("plain"))
	require.Equal(t, "a \xe9-b", latin1("a—é-b"))
	require.Equal(t, "x ", latin1("x\xff"))
}

func Test_asSent(t *testing.T) {
	t.Parallel()

	for _, give := range []string{"plain", "=?UTF-8?B?YWJj?=", "=?UTF-8?Q?actually-ascii?=", "ÄMÄZÕÑ S3"} {
		require.Equal(t, give, decodeHeaderRFC2047(asSent(give)), give)
	}

	require.Equal(t, "plain", asSent("plain"))
}

// FuzzParseQueryArgs checks that any query parses, that what it parses to
// encodes and parses back the same, and that the presign check takes it
// without panicking.
func FuzzParseQueryArgs(f *testing.F) {
	for _, seed := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=test%2F20261118%2Fus-east-1%2Fs3%2Faws4_request" +
			"&X-Amz-Date=20261118T121823Z&X-Amz-SignedHeaders=host&X-Amz-Expires=60&X-Amz-Signature=c89d",
		"AWSAccessKeyId=test&Signature=vMCCHmZzGpblAgHguCkxh3c0FEc%3D&Expires=1795004361",
		"a=1&a=2&&b&=c&%zz=%4&x+y=%2B",
	} {
		f.Add(seed)
	}

	p := New(func() Settings { return Settings{ValidateSignatures: true, GatewayPort: 4566} }, nil)

	f.Fuzz(func(t *testing.T, raw string) {
		args := parseQueryArgs(raw)
		require.Len(t, args.values, len(args.names))

		var encoded strings.Builder

		for i, name := range args.names {
			if i > 0 {
				encoded.WriteByte('&')
			}

			encoded.WriteString(url.QueryEscape(name) + "=" + url.QueryEscape(args.values[name]))
		}

		require.Equal(t, args, parseQueryArgs(encoded.String()))

		_ = p.checkPresigned(presignedRequest(raw))
	})
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err
}

func presignedRequest(rawQuery string) *http.Request {
	return &http.Request{
		Method: http.MethodGet,
		Host:   goldenHost,
		URL:    &url.URL{Path: presignedPath, RawQuery: rawQuery},
		Header: http.Header{},
	}
}

// presignV4 is the query of a path-style GET URL that botocore presigns for host
// with the "test" secret, signing only the host.
func presignV4(host string, at time.Time, expires int) string {
	amzDate := at.UTC().Format(amzDateLayout)
	day := amzDate[:8]
	params := []sigv4.Pair{
		{Key: paramAlgorithm, Value: sigv4.Algorithm},
		{Key: paramCredential, Value: "test/" + day + "/us-east-1/s3/aws4_request"},
		{Key: paramDate, Value: amzDate},
		{Key: paramExpires, Value: strconv.Itoa(expires)},
		{Key: "X-Amz-SignedHeaders", Value: "host"},
	}
	query := sigv4.CanonicalQuery(params)
	block, signed := sigv4.CanonicalHeaders([]sigv4.Header{{Name: "host", Value: host}})
	canonical := sigv4.CanonicalRequest(http.MethodGet, presignedPath, query, block, signed, sigv4.UnsignedPayload)
	key := sigv4.SigningKey(presignSecret, day, "us-east-1", serviceName)
	signature := sigv4.Signature(key, sigv4.StringToSign(amzDate, sigv4.Scope(day, "us-east-1", serviceName), canonical))

	return query + "&" + paramSignatureV4 + "=" + signature
}

// presignV2 is the query of a SigV2 GET URL botocore presigns with the "test"
// secret.
func presignV2(expires time.Time) string {
	stringToSign := stringToSignV2(http.MethodGet, map[string]string{}, expires.Unix(), presignedPath)

	return url.Values{
		paramAccessKeyV2: {presignAccessKey},
		"Signature":      {signV2(presignSecret, stringToSign)},
		paramExpiresV2:   {strconv.FormatInt(expires.Unix(), 10)},
	}.Encode()
}
