package s3_test

import (
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	websiteHost         = "bucket.s3-website." + localstackHost
	websiteTarget       = "/bucket?website"
	codeInvalidArgument = "InvalidArgument"
	argIndexDocument    = "IndexDocument"
	keyPath             = "/actual/key.html"
	typeHTML            = "text/html"

	indexConfig = `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`
)

func putWebsite(t *testing.T, s *server, body string) {
	t.Helper()

	put := s.do(call{method: http.MethodPut, target: websiteTarget, region: regionUSEast1, body: []byte(body)})
	require.Equal(t, http.StatusOK, put.status, string(put.body))
}

func putObject(t *testing.T, s *server, key, body, contentType string, header map[string]string) {
	t.Helper()

	all := map[string]string{headerContentType: contentType}
	maps.Copy(all, header)

	put := s.do(call{
		method: http.MethodPut, target: "/bucket/" + key, region: regionUSEast1, body: []byte(body), header: all,
	})
	require.Equal(t, http.StatusOK, put.status, string(put.body))
}

// site sends a request to the website host and shows a redirect as it is,
// without following it.
func site(s *server, method, target string, header map[string]string) reply {
	s.t.Helper()

	req, err := http.NewRequestWithContext(s.t.Context(), method, s.url+target, nil)
	require.NoError(s.t, err)

	req.Host = websiteHost

	for name, value := range header {
		req.Header.Set(name, value)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(s.t, err)

	defer func() { require.NoError(s.t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	return reply{status: resp.StatusCode, header: resp.Header, body: body}
}

func TestBucketWebsiteCRUD(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	missing := s.do(call{method: http.MethodGet, target: websiteTarget, region: regionUSEast1})
	require.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, s3Error{
		Code: "NoSuchWebsiteConfiguration", Message: "The specified bucket does not have a website configuration",
		BucketName: "bucket", RequestID: missing.header.Get("X-Amz-Request-Id"),
	}, missing.err(t))

	deleted := s.do(call{method: http.MethodDelete, target: websiteTarget, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, deleted.status)

	putWebsite(t, s, indexConfig)

	got := s.do(call{method: http.MethodGet, target: websiteTarget, region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Contains(t, string(got.body), "<IndexDocument><Suffix>index.html</Suffix></IndexDocument>")

	deleted = s.do(call{method: http.MethodDelete, target: websiteTarget, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, deleted.status)

	gone := s.do(call{method: http.MethodGet, target: websiteTarget, region: regionUSEast1})
	assert.Equal(t, http.StatusNotFound, gone.status)
}

// TestPutBucketWebsiteValidates follows test_validate_website_configuration.
func TestPutBucketWebsiteValidates(t *testing.T) {
	const (
		index         = `<IndexDocument><Suffix>index.html</Suffix></IndexDocument>`
		malformedText = "The XML you provided was not well-formed or did not validate against our published schema"
		badProtocol   = "Invalid protocol, protocol can be http or https. If not defined the protocol will be " +
			"selected automatically."
	)

	tests := []struct {
		name         string
		giveBody     string
		wantCode     string
		wantMessage  string
		wantArgument string
		wantValue    string
	}{
		{
			name:     "a slash in the suffix",
			giveBody: `<WebsiteConfiguration><IndexDocument><Suffix>/index.html</Suffix></IndexDocument></WebsiteConfiguration>`,
			wantCode: codeInvalidArgument, wantMessage: "The IndexDocument Suffix is not well formed",
			wantArgument: argIndexDocument, wantValue: "/index.html",
		},
		{
			name:     "an empty suffix",
			giveBody: `<WebsiteConfiguration><IndexDocument><Suffix></Suffix></IndexDocument></WebsiteConfiguration>`,
			wantCode: codeInvalidArgument, wantMessage: "The IndexDocument Suffix is not well formed",
			wantArgument: argIndexDocument,
		},
		{
			name: "RedirectAllRequestsTo with other fields",
			giveBody: `<WebsiteConfiguration><RedirectAllRequestsTo><HostName>test</HostName></RedirectAllRequestsTo>` +
				`<IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`,
			wantCode:     codeInvalidArgument,
			wantMessage:  "RedirectAllRequestsTo cannot be provided in conjunction with other Routing Rules.",
			wantArgument: "RedirectAllRequestsTo", wantValue: "not null",
		},
		{
			name:     "no IndexDocument",
			giveBody: `<WebsiteConfiguration><ErrorDocument><Key>/index.html</Key></ErrorDocument></WebsiteConfiguration>`,
			wantCode: codeInvalidArgument,
			wantMessage: "A value for IndexDocument Suffix must be provided if RedirectAllRequestsTo is " +
				"empty",
			wantArgument: argIndexDocument, wantValue: "null",
		},
		{
			name: "a protocol that is not http or https",
			giveBody: `<WebsiteConfiguration>` + index + `<RoutingRules><RoutingRule><Redirect><Protocol>protocol</Protocol>` +
				`</Redirect></RoutingRule></RoutingRules></WebsiteConfiguration>`,
			wantCode: codeInvalidRequest, wantMessage: badProtocol,
		},
		{
			// net/http panics on WriteHeader(42), so a rule with it would take the
			// request that matched it down with a 500.
			name: "a redirect code that is not a status code",
			giveBody: `<WebsiteConfiguration>` + index + `<RoutingRules><RoutingRule><Redirect>` +
				`<HttpRedirectCode>42</HttpRedirectCode></Redirect></RoutingRule></RoutingRules></WebsiteConfiguration>`,
			wantCode: codeInvalidRequest, wantMessage: "The HTTP redirect code must be a 3xx status code.",
		},
		{
			name: "a redirect code over 999",
			giveBody: `<WebsiteConfiguration>` + index + `<RoutingRules><RoutingRule><Redirect>` +
				`<HttpRedirectCode>1000</HttpRedirectCode></Redirect></RoutingRule></RoutingRules></WebsiteConfiguration>`,
			wantCode: codeInvalidRequest, wantMessage: "The HTTP redirect code must be a 3xx status code.",
		},
		{
			name: "a prefix and a key to replace",
			giveBody: `<WebsiteConfiguration>` + index + `<RoutingRules><RoutingRule><Redirect>` +
				`<ReplaceKeyPrefixWith>prefix</ReplaceKeyPrefixWith><ReplaceKeyWith>key-name</ReplaceKeyWith>` +
				`</Redirect></RoutingRule></RoutingRules></WebsiteConfiguration>`,
			wantCode: codeInvalidRequest, wantMessage: "You can only define ReplaceKeyPrefix or ReplaceKey but not both.",
		},
		{
			name: "an empty condition",
			giveBody: `<WebsiteConfiguration>` + index + `<RoutingRules><RoutingRule><Condition></Condition><Redirect>` +
				`<ReplaceKeyPrefixWith>prefix</ReplaceKeyPrefixWith></Redirect></RoutingRule></RoutingRules>` +
				`</WebsiteConfiguration>`,
			wantCode: codeInvalidRequest,
			wantMessage: "Condition cannot be empty. To redirect all requests without a condition, the condition " +
				"element shouldn't be present.",
		},
		{
			name:     "empty routing rules",
			giveBody: `<WebsiteConfiguration>` + index + `<RoutingRules></RoutingRules></WebsiteConfiguration>`,
			wantCode: codeMalformedXML, wantMessage: malformedText,
		},
		{
			name:     "an error document without a key",
			giveBody: `<WebsiteConfiguration>` + index + `<ErrorDocument></ErrorDocument></WebsiteConfiguration>`,
			wantCode: codeMalformedXML, wantMessage: malformedText,
		},
		{
			name: "RedirectAllRequestsTo without a host",
			giveBody: `<WebsiteConfiguration><RedirectAllRequestsTo><Protocol>http</Protocol>` +
				`</RedirectAllRequestsTo></WebsiteConfiguration>`,
			wantCode: codeMalformedXML, wantMessage: malformedText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			got := s.do(call{method: http.MethodPut, target: websiteTarget, region: regionUSEast1, body: []byte(tt.giveBody)})
			require.Equal(t, http.StatusBadRequest, got.status, string(got.body))

			e := got.err(t)
			assert.Equal(t, tt.wantCode, e.Code)
			assert.Equal(t, tt.wantMessage, e.Message)

			if tt.wantArgument != "" {
				assert.Contains(t, string(got.body), "<ArgumentName>"+tt.wantArgument+"</ArgumentName>")
			}

			if tt.wantValue != "" {
				assert.Contains(t, string(got.body), "<ArgumentValue>"+tt.wantValue+"</ArgumentValue>")
			} else {
				assert.NotContains(t, string(got.body), "<ArgumentValue>")
			}
		})
	}
}

func TestWebsiteServesObjects(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	putObject(t, s, "index.html", "index", typeHTML, nil)
	putObject(t, s, "dir/index.html", "dir index", typeHTML, nil)
	putObject(t, s, "actual/key.html", "key", typeHTML, nil)
	putObject(t, s, "js/key.js", "some js", "application/javascript; charset=utf-8", nil)
	putObject(t, s, "moved.html", "", typeHTML, map[string]string{"x-amz-website-redirect-location": keyPath})
	putWebsite(t, s, indexConfig)

	head := s.do(call{method: http.MethodHead, target: "/bucket/actual/key.html", region: regionUSEast1})
	etag := head.header.Get("ETag")
	require.NotEmpty(t, etag)

	tests := []struct {
		name        string
		giveTarget  string
		giveHeaders map[string]string
		wantStatus  int
		wantBody    string
		wantType    string
		wantHeader  map[string]string
	}{
		{
			name: "the root is the index document", giveTarget: "/", wantStatus: http.StatusOK, wantBody: "index",
			wantType: typeHTML,
		},
		{
			name: "an object", giveTarget: keyPath, wantStatus: http.StatusOK, wantBody: "key",
			wantType: typeHTML, wantHeader: map[string]string{"ETag": etag},
		},
		{
			name: "an object with a charset", giveTarget: "/js/key.js", wantStatus: http.StatusOK, wantBody: "some js",
			wantType: "application/javascript; charset=utf-8",
		},
		{
			name: "if-none-match", giveTarget: keyPath, giveHeaders: map[string]string{"If-None-Match": etag},
			wantStatus: http.StatusNotModified,
		},
		{
			name: "a folder with a slash", giveTarget: "/dir/", wantStatus: http.StatusOK, wantBody: "dir index",
			wantType: typeHTML,
		},
		{
			name: "a folder without a slash", giveTarget: "/dir", wantStatus: http.StatusFound,
			wantHeader: map[string]string{"Location": "/dir/"},
		},
		{
			name: "an object with a redirect location", giveTarget: "/moved.html", wantStatus: http.StatusMovedPermanently,
			wantHeader: map[string]string{"Location": keyPath},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := site(s, http.MethodGet, tt.giveTarget, tt.giveHeaders)
			require.Equal(t, tt.wantStatus, got.status, string(got.body))
			assert.Equal(t, tt.wantBody, string(got.body))

			if tt.wantType != "" {
				assert.Equal(t, tt.wantType, got.header.Get("Content-Type"))
			}

			for name, value := range tt.wantHeader {
				assert.Equal(t, value, got.header.Get(name), name)
			}

			assert.Empty(t, got.header.Get("X-Amz-Id-2"))
		})
	}
}

// TestWebsiteErrorPages pins the HTML pages byte for byte, as the snapshots of
// test_website_hosting_no_such_website, _404 and _http_methods record them.
func TestWebsiteErrorPages(t *testing.T) {
	const (
		hostID = "h6t23Wl2Ndijztq+COn9kvx32omFVRLLtwk36D6+2/CIYSey+Uox6kBxRgcnAASsgnGwctU6zzU="
		head   = "<html>\n<head><title>404 Not Found</title></head>\n<body>\n<h1>404 Not Found</h1>\n<ul>\n"
		tail   = "<li>HostId: " + hostID + "</li>\n</ul>\n<hr/>\n</body>\n</html>\n"
	)

	s := newServer(t)
	createBucket(t, s)

	noBucket := s.do(call{method: http.MethodGet, host: "gone.s3-website." + localstackHost, target: "/"})
	require.Equal(t, http.StatusNotFound, noBucket.status)
	assert.Equal(t, head+"<li>Code: NoSuchBucket</li>\n<li>Message: The specified bucket does not exist</li>\n"+
		"<li>BucketName: gone</li>\n<li>RequestId: "+requestIDOf(t, noBucket.body)+"</li>\n"+tail, string(noBucket.body))
	assert.Equal(t, "text/html; charset=utf-8", noBucket.header.Get("Content-Type"))

	noWebsite := site(s, http.MethodGet, keyPath, nil)
	require.Equal(t, http.StatusNotFound, noWebsite.status)
	assert.Equal(t, head+"<li>Code: NoSuchWebsiteConfiguration</li>\n"+
		"<li>Message: The specified bucket does not have a website configuration</li>\n"+
		"<li>BucketName: bucket</li>\n<li>RequestId: "+requestIDOf(t, noWebsite.body)+"</li>\n"+tail, string(noWebsite.body))

	putWebsite(t, s, indexConfig)

	noKey := site(s, http.MethodGet, "/", nil)
	require.Equal(t, http.StatusNotFound, noKey.status)
	assert.Equal(t, head+"<li>Code: NoSuchKey</li>\n<li>Message: The specified key does not exist.</li>\n"+
		"<li>Key: index.html</li>\n<li>RequestId: "+requestIDOf(t, noKey.body)+"</li>\n"+tail, string(noKey.body))

	putWebsite(t, s, `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument>`+
		`<ErrorDocument><Key>error.html</Key></ErrorDocument></WebsiteConfiguration>`)

	noErrorDocument := site(s, http.MethodGet, "/", nil)
	require.Equal(t, http.StatusNotFound, noErrorDocument.status)
	assert.Equal(t, head+"<li>Code: NoSuchKey</li>\n<li>Message: The specified key does not exist.</li>\n"+
		"<li>Key: index.html</li>\n<li>RequestId: "+requestIDOf(t, noErrorDocument.body)+"</li>\n"+
		"<li>HostId: "+hostID+"</li>\n</ul>\n"+
		"<h3>An Error Occurred While Attempting to Retrieve a Custom Error Document</h3>\n<ul>\n"+
		"<li>Code: NoSuchKey</li>\n<li>Message: The specified key does not exist.</li>\n"+
		"<li>Key: error.html</li>\n</ul>\n<hr/>\n</body>\n</html>\n", string(noErrorDocument.body))

	putObject(t, s, "error.html", "error", typeHTML, nil)

	withErrorDocument := site(s, http.MethodGet, "/missing", nil)
	assert.Equal(t, http.StatusNotFound, withErrorDocument.status)
	assert.Equal(t, "error", string(withErrorDocument.body))
	assert.Equal(t, typeHTML, withErrorDocument.header.Get("Content-Type"))

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodHead, http.MethodPut} {
		got := site(s, method, "/", nil)
		require.Equal(t, http.StatusMethodNotAllowed, got.status, method)

		if method == http.MethodHead {
			continue
		}

		assert.Equal(t, "<html>\n<head><title>405 Method Not Allowed</title></head>\n<body>\n"+
			"<h1>405 Method Not Allowed</h1>\n<ul>\n<li>Code: MethodNotAllowed</li>\n"+
			"<li>Message: The specified method is not allowed against this resource.</li>\n"+
			"<li>Method: "+method+"</li>\n<li>ResourceType: OBJECT</li>\n"+
			"<li>RequestId: "+requestIDOf(t, got.body)+"</li>\n<li>HostId: "+hostID+"</li>\n</ul>\n<hr/>\n</body>\n</html>\n",
			string(got.body), method)
	}
}

// TestWebsiteErrorPagesEscapeTheKey checks that a key, and the error document's
// key, reach the 404 page as text and never as markup.
func TestWebsiteErrorPagesEscapeTheKey(t *testing.T) {
	const (
		script        = "<script>x</script>"
		escaped       = "&lt;script&gt;x&lt;/script&gt;"
		errorDocument = "<img src=x onerror=alert(1)>"
	)

	s := newServer(t)
	createBucket(t, s)
	putWebsite(t, s, indexConfig)

	noKey := site(s, http.MethodGet, "/%3Cscript%3Ex%3C/script%3E", nil)
	require.Equal(t, http.StatusNotFound, noKey.status)
	assert.Contains(t, string(noKey.body), "<li>Key: "+escaped+"</li>")
	assert.Contains(t, string(noKey.body), "&lt;script&gt;")
	assert.NotContains(t, string(noKey.body), "<script>")

	putWebsite(t, s, `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument>`+
		`<ErrorDocument><Key>&lt;img src=x onerror=alert(1)&gt;</Key></ErrorDocument></WebsiteConfiguration>`)

	noErrorDocument := site(s, http.MethodGet, "/%3Cscript%3Ex%3C/script%3E", nil)
	require.Equal(t, http.StatusNotFound, noErrorDocument.status)
	assert.Contains(t, string(noErrorDocument.body), "<li>Key: "+escaped+"</li>")
	assert.Contains(t, string(noErrorDocument.body), "<li>Key: &lt;img src=x onerror=alert(1)&gt;</li>")
	assert.NotContains(t, string(noErrorDocument.body), "<img")
	assert.NotContains(t, string(noErrorDocument.body), errorDocument)
}

// requestIDOf reads the RequestId line of an error page.
func requestIDOf(t *testing.T, page []byte) string {
	t.Helper()

	_, after, ok := strings.Cut(string(page), "<li>RequestId: ")
	require.True(t, ok, string(page))

	id, _, _ := strings.Cut(after, "</li>")

	return id
}

// TestWebsiteRedirects follows test_routing_rules_*, test_object_website_redirect_location
// and test_website_hosting_redirect_all.
func TestWebsiteRedirects(t *testing.T) {
	const routing = `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument>` +
		`<ErrorDocument><Key>error.html</Key></ErrorDocument><RoutingRules>` +
		`<RoutingRule><Condition><KeyPrefixEquals>both-prefixed/</KeyPrefixEquals>` +
		`<HttpErrorCodeReturnedEquals>404</HttpErrorCodeReturnedEquals></Condition>` +
		`<Redirect><ReplaceKeyWith>redirected-both.html</ReplaceKeyWith></Redirect></RoutingRule>` +
		`<RoutingRule><Condition><KeyPrefixEquals>host/</KeyPrefixEquals></Condition>` +
		`<Redirect><HostName>random-hostname</HostName></Redirect></RoutingRule>` +
		`<RoutingRule><Condition><KeyPrefixEquals>replace-prefix/</KeyPrefixEquals></Condition>` +
		`<Redirect><ReplaceKeyPrefixWith>replaced-prefix/</ReplaceKeyPrefixWith></Redirect></RoutingRule>` +
		`<RoutingRule><Condition><KeyPrefixEquals>docs/</KeyPrefixEquals></Condition>` +
		`<Redirect><ReplaceKeyPrefixWith></ReplaceKeyPrefixWith></Redirect></RoutingRule>` +
		`<RoutingRule><Condition><KeyPrefixEquals>protocol/</KeyPrefixEquals></Condition>` +
		`<Redirect><Protocol>https</Protocol></Redirect></RoutingRule>` +
		`<RoutingRule><Condition><KeyPrefixEquals>code/</KeyPrefixEquals></Condition>` +
		`<Redirect><HttpRedirectCode>307</HttpRedirectCode></Redirect></RoutingRule>` +
		`<RoutingRule><Condition><HttpErrorCodeReturnedEquals>404</HttpErrorCodeReturnedEquals></Condition>` +
		`<Redirect><ReplaceKeyWith>redirected.html</ReplaceKeyWith></Redirect></RoutingRule>` +
		`</RoutingRules></WebsiteConfiguration>`

	s := newServer(t)
	createBucket(t, s)
	putWebsite(t, s, routing)
	putObject(t, s, "redirected.html", "redirected", typeHTML, nil)
	putObject(t, s, "redirected-both.html", "redirected-both", typeHTML, nil)
	putObject(t, s, "test.html", "test", typeHTML, nil)
	putObject(t, s, "error.html", "error", typeHTML, nil)

	base := "http://" + websiteHost

	tests := []struct {
		name         string
		giveTarget   string
		wantStatus   int
		wantLocation string
		wantBody     string
	}{
		{
			name: "a host", giveTarget: "/host/key", wantStatus: http.StatusMovedPermanently,
			wantLocation: "http://random-hostname/host/key",
		},
		{
			name: "a replaced prefix", giveTarget: "/replace-prefix/key", wantStatus: http.StatusMovedPermanently,
			wantLocation: base + "/replaced-prefix/key",
		},
		{
			name: "an empty replacement removes the prefix", giveTarget: "/docs/test.html",
			wantStatus: http.StatusMovedPermanently, wantLocation: base + "/test.html",
		},
		{
			name: "only the first prefix is replaced", giveTarget: "/docs/docs/test.html",
			wantStatus: http.StatusMovedPermanently, wantLocation: base + "/docs/test.html",
		},
		{
			name: "a protocol", giveTarget: "/protocol/key", wantStatus: http.StatusMovedPermanently,
			wantLocation: "https://" + websiteHost + "/protocol/key",
		},
		{name: "a code", giveTarget: "/code/key", wantStatus: http.StatusTemporaryRedirect, wantLocation: base + "/code/key"},
		{
			name: "a rule for a 404 when the key is missing", giveTarget: "/non-existent",
			wantStatus: http.StatusMovedPermanently, wantLocation: base + "/redirected.html",
		},
		{
			name: "a rule for a prefix and a 404", giveTarget: "/both-prefixed/x",
			wantStatus: http.StatusMovedPermanently, wantLocation: base + "/redirected-both.html",
		},
		{name: "an existing key is served", giveTarget: "/test.html", wantStatus: http.StatusOK, wantBody: "test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := site(s, http.MethodGet, tt.giveTarget, nil)
			require.Equal(t, tt.wantStatus, got.status, string(got.body))
			assert.Equal(t, tt.wantLocation, got.header.Get("Location"))
			assert.Equal(t, tt.wantBody, string(got.body))
		})
	}
}

func TestWebsiteRedirectAllRequestsTo(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	putWebsite(t, s, `<WebsiteConfiguration><RedirectAllRequestsTo><HostName>example.com</HostName>`+
		`<Protocol>https</Protocol></RedirectAllRequestsTo></WebsiteConfiguration>`)

	got := site(s, http.MethodGet, "/some/key?a=b", nil)
	require.Equal(t, http.StatusMovedPermanently, got.status)
	assert.Equal(t, "https://example.com/some/key?a=b", got.header.Get("Location"))
	assert.Empty(t, got.body)
}

func TestWebsiteObjectRedirectLocationInErrorDocument(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	putWebsite(t, s, `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument>`+
		`<ErrorDocument><Key>error.html</Key></ErrorDocument></WebsiteConfiguration>`)
	putObject(t, s, "error.html", "error_redirected", typeHTML,
		map[string]string{"x-amz-website-redirect-location": "/another/error.html"})
	putObject(t, s, "another/error.html", "error", typeHTML, nil)

	got := site(s, http.MethodGet, "/", nil)
	require.Equal(t, http.StatusMovedPermanently, got.status)
	assert.Equal(t, "/another/error.html", got.header.Get("Location"))

	head := s.do(call{method: http.MethodHead, target: "/bucket/error.html", region: regionUSEast1})
	assert.Equal(t, "/another/error.html", head.header.Get("X-Amz-Website-Redirect-Location"))
}
