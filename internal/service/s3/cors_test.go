package s3_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vhost        = "bucket.s3." + localstackHost
	appOrigin    = "https://app.localstack.cloud"
	corsPath     = "/bucket?cors"
	objectTarget = "/object"

	hOrigin             = "Origin"
	refusedOrigin       = "whatever"
	codeInvalidRequest  = "InvalidRequest"
	codeMalformedXML    = "MalformedXML"
	codeAccessForbidden = "AccessForbidden"
	reqMethod           = "Access-Control-Request-Method"
	reqHeaders          = "Access-Control-Request-Headers"
	resourceBucket      = "BUCKET"
	resourceObject      = "OBJECT"
	localOrigin         = "https://localhost:4200"

	corsAllowAll = `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin>` +
		`<AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`
)

// corsError is an S3 error with the members a preflight adds.
type corsError struct {
	Code         string `xml:"Code"`
	Message      string `xml:"Message"`
	HostID       string `xml:"HostId"`
	Method       string `xml:"Method"`
	ResourceType string `xml:"ResourceType"`
}

func (r reply) corsErr(t *testing.T) corsError {
	t.Helper()

	var e corsError
	require.NoError(t, xml.Unmarshal(r.body, &e), string(r.body))

	return e
}

// corsServer is a server with a bucket that holds one object.
func corsServer(t *testing.T) *server {
	t.Helper()

	s := newServer(t)
	createBucket(t, s)

	put := s.do(call{
		method: http.MethodPut, target: "/bucket" + objectTarget, region: regionUSEast1, body: []byte("cors-test"),
	})
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	return s
}

func putCORS(t *testing.T, s *server, body string) {
	t.Helper()

	put := s.do(call{method: http.MethodPut, target: corsPath, region: regionUSEast1, body: []byte(body)})
	require.Equal(t, http.StatusOK, put.status, string(put.body))
}

func TestBucketCorsCRUD(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	missing := s.do(call{method: http.MethodGet, target: corsPath, region: regionUSEast1})
	require.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, s3Error{
		Code: "NoSuchCORSConfiguration", Message: "The CORS configuration does not exist", BucketName: "bucket",
		RequestID: missing.header.Get("X-Amz-Request-Id"),
	}, missing.err(t))

	deleted := s.do(call{method: http.MethodDelete, target: corsPath, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, deleted.status, "deleting rules that are not there is not an error")

	putCORS(t, s, `<CORSConfiguration><CORSRule><AllowedOrigin></AllowedOrigin><AllowedMethod>GET</AllowedMethod>`+
		`<AllowedMethod>HEAD</AllowedMethod><AllowedHeader>x-amz-meta-a</AllowedHeader>`+
		`<ExposeHeader>etag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>`)

	got := s.do(call{method: http.MethodGet, target: corsPath, region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Contains(t, string(got.body), "<AllowedOrigin/>", "an empty origin is kept")
	assert.Contains(t, string(got.body), "<AllowedMethod>GET</AllowedMethod><AllowedMethod>HEAD</AllowedMethod>")
	assert.Contains(t, string(got.body), "<MaxAgeSeconds>3000</MaxAgeSeconds>")

	deleted = s.do(call{method: http.MethodDelete, target: corsPath, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, deleted.status)

	gone := s.do(call{method: http.MethodGet, target: corsPath, region: regionUSEast1})
	assert.Equal(t, http.StatusNotFound, gone.status)

	other := s.do(call{method: http.MethodGet, target: "/nobucket?cors", region: regionUSEast1})
	assert.Equal(t, http.StatusNotFound, other.status)
	assert.Equal(t, "NoSuchBucket", other.err(t).Code)
}

func TestPutBucketCorsValidates(t *testing.T) {
	tests := []struct {
		name       string
		giveBody   string
		wantCode   string
		wantStatus int
		wantMsg    string
	}{
		{
			name: "an unsupported method",
			giveBody: `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>MYMETHOD</AllowedMethod>` +
				`</CORSRule></CORSConfiguration>`,
			wantCode: codeInvalidRequest, wantStatus: http.StatusBadRequest,
			wantMsg: "Found unsupported HTTP method in CORS config. Unsupported method is MYMETHOD",
		},
		{
			name: "no rules", giveBody: `<CORSConfiguration></CORSConfiguration>`,
			wantCode: codeMalformedXML, wantStatus: http.StatusBadRequest,
			wantMsg: "The XML you provided was not well-formed or did not validate against our published schema",
		},
		{
			name: "a rule without origins",
			giveBody: `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod></CORSRule>` +
				`</CORSConfiguration>`,
			wantCode: codeMalformedXML, wantStatus: http.StatusBadRequest,
			wantMsg: "The XML you provided was not well-formed or did not validate against our published schema",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			got := s.do(call{method: http.MethodPut, target: corsPath, region: regionUSEast1, body: []byte(tt.giveBody)})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))
			e := got.err(t)
			assert.Equal(t, tt.wantCode, e.Code)
			assert.Equal(t, tt.wantMsg, e.Message)
		})
	}
}

// TestPreflightWithoutRules follows test_cors_http_options_no_config and
// test_cors_no_config_localstack_allowed.
func TestPreflightWithoutRules(t *testing.T) {
	tests := []struct {
		name         string
		giveHost     string
		giveHeaders  map[string]string
		wantStatus   int
		wantCode     string
		wantMessage  string
		wantMethod   string
		wantResource string
		wantOrigin   string
	}{
		{
			name: "no origin", giveHost: vhost, giveHeaders: nil,
			wantStatus: http.StatusBadRequest, wantCode: "BadRequest",
			wantMessage: "Insufficient information. Origin request header needed.",
		},
		{
			name: "an origin the gateway does not allow", giveHost: vhost,
			giveHeaders: map[string]string{hOrigin: refusedOrigin, reqMethod: http.MethodPut},
			wantStatus:  http.StatusForbidden, wantCode: codeAccessForbidden,
			wantMessage: "CORSResponse: CORS is not enabled for this bucket.", wantMethod: "PUT", wantResource: resourceBucket,
		},
		{
			name: "an origin the gateway does not allow, no method", giveHost: vhost,
			giveHeaders: map[string]string{hOrigin: refusedOrigin},
			wantStatus:  http.StatusForbidden, wantCode: codeAccessForbidden,
			wantMessage: "CORSResponse: CORS is not enabled for this bucket.", wantMethod: "OPTIONS",
			wantResource: resourceBucket,
		},
		{
			name: "a bucket that does not exist", giveHost: "nobucket.s3." + localstackHost,
			giveHeaders: map[string]string{hOrigin: refusedOrigin},
			wantStatus:  http.StatusForbidden, wantCode: codeAccessForbidden,
			wantMessage: "CORSResponse: Bucket not found", wantMethod: "OPTIONS", wantResource: resourceBucket,
		},
		{
			name: "an origin the gateway allows", giveHost: vhost,
			giveHeaders: map[string]string{hOrigin: appOrigin, reqMethod: http.MethodPut},
			wantStatus:  http.StatusOK, wantOrigin: appOrigin,
		},
		{
			name: "an allowed origin, bucket that does not exist", giveHost: "nobucket.s3." + localstackHost,
			giveHeaders: map[string]string{hOrigin: appOrigin},
			wantStatus:  http.StatusOK, wantOrigin: appOrigin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := corsServer(t)

			got := s.do(call{method: http.MethodOptions, host: tt.giveHost, target: objectTarget, header: tt.giveHeaders})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))

			if tt.wantStatus == http.StatusOK {
				assert.Equal(t, tt.wantOrigin, got.header.Get("Access-Control-Allow-Origin"))
				assert.Empty(t, got.body)
				assert.Empty(t, got.header.Get("Content-Type"))
				assert.NotEmpty(t, got.header.Get("X-Amz-Request-Id"))

				return
			}

			e := got.corsErr(t)
			assert.Equal(t, corsError{
				Code: tt.wantCode, Message: tt.wantMessage, HostID: e.HostID, Method: tt.wantMethod,
				ResourceType: tt.wantResource,
			}, e)
			assert.NotEmpty(t, e.HostID)
		})
	}
}

// TestCORSRuleMatching follows test_cors_match_origins, test_cors_options_*,
// test_cors_match_methods and test_cors_match_headers.
func TestCORSRuleMatching(t *testing.T) {
	const rule = `<CORSConfiguration><CORSRule>` +
		`<AllowedOrigin>https://localhost:4200</AllowedOrigin><AllowedOrigin>http://*.origin.com</AllowedOrigin>` +
		`<AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod>` +
		`<AllowedHeader>X-Amz-Meta-One</AllowedHeader><AllowedHeader>content-type</AllowedHeader>` +
		`<ExposeHeader>etag</ExposeHeader><ExposeHeader>x-amz-request-id</ExposeHeader>` +
		`<MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule></CORSConfiguration>`

	tests := []struct {
		name        string
		giveMethod  string
		giveHeaders map[string]string
		wantStatus  int
		wantOrigin  string
		wantHeaders string
	}{
		{
			name: "a preflight from the origin", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{hOrigin: localOrigin, reqMethod: http.MethodPut},
			wantStatus:  http.StatusOK, wantOrigin: localOrigin,
		},
		{
			name: "a preflight from a wildcard origin", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{hOrigin: "http://test.origin.com", reqMethod: http.MethodGet},
			wantStatus:  http.StatusOK, wantOrigin: "http://test.origin.com",
		},
		{
			name: "a preflight from the origin without a subdomain", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{hOrigin: "http://.origin.com", reqMethod: http.MethodGet},
			wantStatus:  http.StatusForbidden,
		},
		{
			name: "a preflight from another scheme", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{hOrigin: "http://localhost:4200", reqMethod: http.MethodPut},
			wantStatus:  http.StatusForbidden,
		},
		{
			name: "a preflight for a method the rule does not allow", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{hOrigin: localOrigin, reqMethod: "DELETE"},
			wantStatus:  http.StatusForbidden,
		},
		{
			name: "a preflight with allowed headers, in any case", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{
				hOrigin: localOrigin, reqMethod: http.MethodGet,
				reqHeaders: "X-Amz-Meta-ONE, Content-Type",
			},
			wantStatus: http.StatusOK, wantOrigin: localOrigin, wantHeaders: "x-amz-meta-one, content-type",
		},
		{
			name: "a preflight with a header the rule does not allow", giveMethod: http.MethodOptions,
			giveHeaders: map[string]string{
				hOrigin: localOrigin, reqMethod: http.MethodGet,
				reqHeaders: "x-amz-meta-one, x-other",
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "a GET from the origin", giveMethod: http.MethodGet,
			giveHeaders: map[string]string{hOrigin: localOrigin},
			wantStatus:  http.StatusOK, wantOrigin: localOrigin,
		},
		{
			name: "a GET from another origin", giveMethod: http.MethodGet,
			giveHeaders: map[string]string{hOrigin: "http://localhost:4200"},
			wantStatus:  http.StatusOK,
		},
		{
			name: "a GET with a referer only", giveMethod: http.MethodGet,
			giveHeaders: map[string]string{"Referer": localOrigin},
			wantStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := corsServer(t)
			putCORS(t, s, rule)

			got := s.do(call{method: tt.giveMethod, host: vhost, target: objectTarget, header: tt.giveHeaders})
			require.Equal(t, tt.wantStatus, got.status, string(got.body))
			assert.Equal(t, tt.wantOrigin, got.header.Get("Access-Control-Allow-Origin"))
			assert.Equal(t, tt.wantHeaders, got.header.Get("Access-Control-Allow-Headers"))

			if tt.wantOrigin == "" {
				assert.Empty(t, got.header.Get("Access-Control-Allow-Methods"))

				return
			}

			assert.Equal(t, "true", got.header.Get("Access-Control-Allow-Credentials"))
			assert.Equal(t, "GET, PUT", got.header.Get("Access-Control-Allow-Methods"))
			assert.Equal(t, "etag, x-amz-request-id", got.header.Get("Access-Control-Expose-Headers"))
			assert.Equal(t, "3000", got.header.Get("Access-Control-Max-Age"))
			assert.Equal(t, "Origin, Access-Control-Request-Headers, Access-Control-Request-Method",
				got.header.Get("Vary"))
		})
	}
}

func TestCORSWildcardOriginSendsNoCredentials(t *testing.T) {
	s := corsServer(t)
	putCORS(t, s, corsAllowAll)

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		got := s.do(call{
			method: method, host: vhost, target: objectTarget,
			header: map[string]string{hOrigin: "http://random:1234", reqMethod: http.MethodGet},
		})
		require.Equal(t, http.StatusOK, got.status, method)
		assert.Equal(t, "*", got.header.Get("Access-Control-Allow-Origin"), method)
		assert.Empty(t, got.header.Get("Access-Control-Allow-Credentials"), method)
		assert.Empty(t, got.header.Get("Access-Control-Max-Age"), method)
		assert.Empty(t, got.header.Get("Access-Control-Expose-Headers"), method)
	}

	// The rule names no AllowedHeaders, so a preflight asking for one does not match.
	got := s.do(call{
		method: http.MethodOptions, host: vhost, target: objectTarget,
		header: map[string]string{
			hOrigin: "localhost:4566", reqMethod: http.MethodGet,
			reqHeaders: "x-amz-request-payer",
		},
	})
	require.Equal(t, http.StatusForbidden, got.status)
	assert.Equal(t, corsError{
		Code: codeAccessForbidden, HostID: got.corsErr(t).HostID, Method: http.MethodGet, ResourceType: resourceObject,
		Message: "CORSResponse: This CORS request is not allowed. This is usually because the evalution of Origin, " +
			"request method / Access-Control-Request-Method or Access-Control-Request-Headers are not whitelisted " +
			"by the resource's CORS spec.",
	}, got.corsErr(t))
}

func TestCORSOnPathStyleRequestsToExistingBuckets(t *testing.T) {
	s := corsServer(t)
	putCORS(t, s, corsAllowAll)

	got := s.do(call{
		method: http.MethodGet, target: "/bucket" + objectTarget, region: regionUSEast1,
		header: map[string]string{hOrigin: "http://random:1234"},
	})
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, "*", got.header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin, Access-Control-Request-Headers, Access-Control-Request-Method", got.header.Get("Vary"))
}

func TestCORSDefaultsForCreateBucketAndListBuckets(t *testing.T) {
	s := newServer(t)

	// An S3 virtual host owns the request, so S3 applies the gateway's policy to CreateBucket.
	denied := s.do(call{
		method: http.MethodPut, host: "newbucket.s3." + localstackHost, target: "/", region: regionUSEast1,
		header: map[string]string{hOrigin: "http://evil.example"},
	})
	assert.Equal(t, http.StatusForbidden, denied.status)
	assertNoCORSHeaders(t, denied)
	assertNoBucket(t, s, "newbucket")

	allowed := s.do(call{
		method: http.MethodPut, host: "newbucket2.s3." + localstackHost, target: "/", region: regionUSEast1,
		header: map[string]string{hOrigin: appOrigin},
	})
	require.Equal(t, http.StatusOK, allowed.status)
	assert.Equal(t, appOrigin, allowed.header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", allowed.header.Get("Access-Control-Allow-Credentials"))
}

const (
	evilOrigin    = "http://evil.example"
	writtenTarget = "/written"
)

// assertNoCORSHeaders checks that the response has no Access-Control-* header,
// which a response to an origin that is not allowed never carries.
func assertNoCORSHeaders(t *testing.T, got reply) {
	t.Helper()

	for name := range got.header {
		assert.False(t, strings.HasPrefix(strings.ToLower(name), "access-control-"), "unexpected header %s", name)
	}
}

// assertNoBucket checks through an allowed origin that the bucket was never created.
func assertNoBucket(t *testing.T, s *server, bucket string) {
	t.Helper()

	got := s.do(call{method: http.MethodHead, target: "/" + bucket, region: regionUSEast1})
	assert.Equal(t, http.StatusNotFound, got.status)
}

// An actual request from an origin the gateway does not allow, to a bucket with
// no CORS rules, runs and gets no CORS headers, as LocalStack's handle_cors
// returns at cors.py:176 and leaves the same-origin policy to keep the response
// from the page.
func TestCORSDisallowedOriginBucketWithoutRules(t *testing.T) {
	s := corsServer(t)

	// The virtual host and the path-style request each write their own key.
	for host, target := range map[string]string{vhost: writtenTarget, "": "/bucket/written-path"} {
		put := s.do(call{
			method: http.MethodPut, host: host, target: target, region: regionUSEast1, body: []byte("body"),
			header: map[string]string{hOrigin: evilOrigin},
		})
		require.Equal(t, http.StatusOK, put.status, "host %q: %s", host, put.body)
		assertNoCORSHeaders(t, put)
	}

	for _, target := range []string{writtenTarget, "/written-path"} {
		got := s.do(call{
			method: http.MethodGet, host: vhost, target: target, region: regionUSEast1,
			header: map[string]string{hOrigin: evilOrigin},
		})
		require.Equal(t, http.StatusOK, got.status, target)
		assert.Equal(t, "body", string(got.body))
		assertNoCORSHeaders(t, got)
	}
}

// The same request to a bucket whose rules match nothing runs and gets no CORS
// headers either (cors.py:190-192).
func TestCORSDisallowedOriginBucketWithRulesThatMatchNothing(t *testing.T) {
	s := corsServer(t)
	putCORS(t, s, `<CORSConfiguration><CORSRule><AllowedOrigin>https://good.example</AllowedOrigin>`+
		`<AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)

	put := s.do(call{
		method: http.MethodPut, host: vhost, target: writtenTarget, region: regionUSEast1, body: []byte("body"),
		header: map[string]string{hOrigin: evilOrigin},
	})
	require.Equal(t, http.StatusOK, put.status, string(put.body))
	assertNoCORSHeaders(t, put)

	got := s.do(call{
		method: http.MethodGet, host: vhost, target: writtenTarget, region: regionUSEast1,
		header: map[string]string{hOrigin: evilOrigin},
	})
	require.Equal(t, http.StatusOK, got.status)
	assertNoCORSHeaders(t, got)

	// The rule still serves the origin it names.
	matched := s.do(call{
		method: http.MethodGet, host: vhost, target: writtenTarget, region: regionUSEast1,
		header: map[string]string{hOrigin: "https://good.example"},
	})
	require.Equal(t, http.StatusOK, matched.status)
	assert.Equal(t, "https://good.example", matched.header.Get("Access-Control-Allow-Origin"))
}

// A bucket that does not exist: a virtual host is S3's by the pre-router, so the
// call runs and fails as NoSuchBucket with no CORS headers (cors.py:149-176). A
// path-style call is not S3's, so the gateway's CORS refuses it before any
// operation runs, whatever the operation.
func TestCORSDisallowedOriginBucketThatDoesNotExist(t *testing.T) {
	s := newServer(t)

	missing := s.do(call{
		method: http.MethodGet, host: "nobucket.s3." + localstackHost, target: "/key", region: regionUSEast1,
		header: map[string]string{hOrigin: evilOrigin},
	})
	require.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, "NoSuchBucket", missing.err(t).Code)
	assertNoCORSHeaders(t, missing)

	refused := s.do(call{
		method: http.MethodPut, target: "/pathbucket", region: regionUSEast1,
		header: map[string]string{hOrigin: evilOrigin},
	})
	assert.Equal(t, http.StatusForbidden, refused.status)
	assertNoCORSHeaders(t, refused)
	assertNoBucket(t, s, "pathbucket")
}

// ListBuckets and CreateBucket from an origin the gateway does not allow are
// refused with 403 before the operation runs (cors.py:301-310), on a host S3
// claims and on a plain one. The refusal has no CORS headers, and a refused
// CreateBucket creates nothing.
func TestCORSDisallowedOriginListAndCreateBucket(t *testing.T) {
	s := corsServer(t)

	for _, host := range []string{"s3." + localstackHost, ""} {
		list := s.do(call{
			method: http.MethodGet, host: host, target: "/", region: regionUSEast1,
			header: map[string]string{hOrigin: evilOrigin},
		})
		assert.Equal(t, http.StatusForbidden, list.status, "ListBuckets on host %q", host)
		assert.Empty(t, list.body)
		assertNoCORSHeaders(t, list)
	}

	for _, host := range []string{"newbucket.s3." + localstackHost, "s3." + localstackHost} {
		target := "/newbucket"
		if strings.HasPrefix(host, "newbucket.") {
			target = "/"
		}

		create := s.do(call{
			method: http.MethodPut, host: host, target: target, region: regionUSEast1,
			header: map[string]string{hOrigin: evilOrigin},
		})
		assert.Equal(t, http.StatusForbidden, create.status, "CreateBucket on host %q", host)
		assert.Empty(t, create.body)
		assertNoCORSHeaders(t, create)
		assertNoBucket(t, s, "newbucket")
	}

	// A rule that names the origin does not make CreateBucket on its bucket
	// allowed: LocalStack refuses it from s3_cors_request_handler.
	putCORS(t, s, `<CORSConfiguration><CORSRule><AllowedOrigin>`+evilOrigin+`</AllowedOrigin>`+
		`<AllowedMethod>PUT</AllowedMethod></CORSRule></CORSConfiguration>`)

	exists := s.do(call{
		method: http.MethodPut, host: vhost, target: "/", region: regionUSEast1,
		header: map[string]string{hOrigin: evilOrigin},
	})
	assert.Equal(t, http.StatusForbidden, exists.status)
	assertNoCORSHeaders(t, exists)

	// An allowed origin lists the buckets and gets the gateway's CORS headers.
	allowed := s.do(call{
		method: http.MethodGet, host: "s3." + localstackHost, target: "/", region: regionUSEast1,
		header: map[string]string{hOrigin: appOrigin},
	})
	require.Equal(t, http.StatusOK, allowed.status)
	assert.Equal(t, appOrigin, allowed.header.Get("Access-Control-Allow-Origin"))
}
