package s3

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	// websiteHostID is the HostId of the HTML pages a website sends
	// (website_hosting.py).
	websiteHostID = "h6t23Wl2Ndijztq+COn9kvx32omFVRLLtwk36D6+2/CIYSey+Uox6kBxRgcnAASsgnGwctU6zzU="

	contentTypeHTML = "text/html; charset=utf-8"
	websiteHostPart = ".s3-website."

	maxRoutingRules = 50

	messageNoSuchWebsite = "The specified bucket does not have a website configuration"
	messageNoSuchBucket  = "The specified bucket does not exist"
	messageNoSuchKey     = "The specified key does not exist."
	messageBadProtocol   = "Invalid protocol, protocol can be http or https. If not defined the protocol will " +
		"be selected automatically."
)

// websiteProtocols are the protocols a website redirect may name.
var websiteProtocols = []string{"http", "https"}

// PutBucketWebsite stores the website configuration of the bucket after
// validating it as LocalStack's validate_website_configuration does.
func (p *Provider) PutBucketWebsite(
	ctx context.Context, in *s3api.PutBucketWebsiteInput,
) (*s3api.PutBucketWebsiteOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	err = validateWebsite(in.WebsiteConfiguration)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.gone {
		return nil, noSuchBucket(b.Name)
	}

	b.Website = in.WebsiteConfiguration

	return &s3api.PutBucketWebsiteOutput{}, nil
}

// GetBucketWebsite returns the website configuration of the bucket.
func (p *Provider) GetBucketWebsite(
	ctx context.Context, in *s3api.GetBucketWebsiteInput,
) (*s3api.GetBucketWebsiteOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.gone {
		return nil, noSuchBucket(b.Name)
	}

	if b.Website == nil {
		return nil, (&s3api.NoSuchWebsiteConfiguration{BucketName: &b.Name}).Err(messageNoSuchWebsite)
	}

	return &s3api.GetBucketWebsiteOutput{
		ErrorDocument:         b.Website.ErrorDocument,
		IndexDocument:         b.Website.IndexDocument,
		RedirectAllRequestsTo: b.Website.RedirectAllRequestsTo,
		RoutingRules:          b.Website.RoutingRules,
	}, nil
}

// DeleteBucketWebsite removes the website configuration of the bucket. A bucket
// without one is not an error.
func (p *Provider) DeleteBucketWebsite(
	ctx context.Context, in *s3api.DeleteBucketWebsiteInput,
) (*s3api.DeleteBucketWebsiteOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.gone {
		return nil, noSuchBucket(b.Name)
	}

	b.Website = nil

	return &s3api.DeleteBucketWebsiteOutput{}, nil
}

// validateWebsite is validate_website_configuration (validation.py:252). A
// field counts as given when it is present, an empty one included, as the
// decoded dictionary of LocalStack does.
func validateWebsite(config *s3api.WebsiteConfiguration) error {
	if config == nil {
		return malformedConfig()
	}

	if redirectAll := config.RedirectAllRequestsTo; redirectAll != nil &&
		(redirectAll.HostName != nil || redirectAll.Protocol != nil) {
		return validateRedirectAll(config, redirectAll)
	}

	if index := config.IndexDocument; index == nil || index.Suffix == nil {
		return invalidArgument("A value for IndexDocument Suffix must be provided if RedirectAllRequestsTo is empty",
			"IndexDocument", new("null"))
	}

	if suffix := *config.IndexDocument.Suffix; suffix == "" || strings.Contains(suffix, "/") {
		var value *string
		if suffix != "" {
			value = &suffix
		}

		return invalidArgument("The IndexDocument Suffix is not well formed", "IndexDocument", value)
	}

	if config.ErrorDocument != nil && deref(config.ErrorDocument.Key) == "" {
		return malformedConfig()
	}

	if config.RoutingRules != nil {
		return validateRoutingRules(config.RoutingRules)
	}

	return nil
}

func validateRedirectAll(config *s3api.WebsiteConfiguration, redirectAll *s3api.RedirectAllRequestsTo) error {
	others := 0
	for _, present := range []bool{
		config.ErrorDocument != nil, config.IndexDocument != nil, config.RoutingRules != nil,
	} {
		if present {
			others++
		}
	}

	if others > 0 {
		return invalidArgument("RedirectAllRequestsTo cannot be provided in conjunction with other Routing Rules.",
			"RedirectAllRequestsTo", new("not null"))
	}

	if redirectAll.HostName == nil {
		return malformedConfig()
	}

	if protocol := deref(redirectAll.Protocol); protocol != "" && !slices.Contains(websiteProtocols, protocol) {
		return invalidConfigRequest(messageBadProtocol)
	}

	return nil
}

func validateRoutingRules(rules []s3api.RoutingRule) error {
	if len(rules) == 0 || len(rules) > maxRoutingRules {
		return malformedConfig()
	}

	for _, rule := range rules {
		var redirect s3api.Redirect
		if rule.Redirect != nil {
			redirect = *rule.Redirect
		}

		if redirect.ReplaceKeyPrefixWith != nil && redirect.ReplaceKeyWith != nil {
			return invalidConfigRequest("You can only define ReplaceKeyPrefix or ReplaceKey but not both.")
		}

		if c := rule.Condition; c != nil && c.KeyPrefixEquals == nil && c.HttpErrorCodeReturnedEquals == nil {
			return invalidConfigRequest("Condition cannot be empty. To redirect all requests without a condition, " +
				"the condition element shouldn't be present.")
		}

		if protocol := deref(redirect.Protocol); protocol != "" && !slices.Contains(websiteProtocols, protocol) {
			return invalidConfigRequest(messageBadProtocol)
		}
	}

	return nil
}

// websiteBucket returns the bucket a website host names, whichever account owns
// it, and its website configuration. A website request is not signed.
func (p *Provider) websiteBucket(name string) (*Bucket, *s3api.WebsiteConfiguration, bool) {
	owner, ok := p.names.owner(name)
	if !ok {
		return nil, nil, false
	}

	b := p.state(owner).get(name)
	if b == nil {
		return nil, nil, false
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.gone {
		return nil, nil, false
	}

	return b, b.Website, true
}

// siteObject is an object a website serves: its current version and, when it
// was asked for, the open body, which the caller closes.
type siteObject struct {
	version *Version
	body    io.ReadSeekCloser
}

// siteExists is HeadObject for a website: whether the key has a current
// version that is not a delete marker.
func siteExists(b *Bucket, key string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	object, ok := b.Objects[key]

	return !b.gone && ok && object.current() != nil && !object.current().DeleteMarker
}

// siteGet is GetObject for a website: the current version of the key, unless
// it is missing or a delete marker, with its body open. The body is opened under
// the bucket lock, so a PutObject cannot remove it first.
func (p *Provider) siteGet(b *Bucket, key string) (*siteObject, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	object, ok := b.Objects[key]
	if b.gone || !ok || object.current() == nil || object.current().DeleteMarker {
		return nil, noSuchKey(key)
	}

	v := object.current()

	body, err := p.blobs.Open(v.BlobID)
	if err != nil {
		return nil, err //nolint:wrapcheck // surfaces as the website's 500 page.
	}

	return &siteObject{version: v, body: body}, nil
}

// pageURL is the URL of the request as the client wrote it, which a redirect
// rewrites: LocalStack's request.url.
func pageURL(r *http.Request) *url.URL {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	return &url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
}

// serveWebsite answers a request to <bucket>.s3-website.<domain>, as LocalStack's
// S3WebsiteHostingHandler does (website_hosting.py): anything but GET is a 405
// page, a missing bucket, website or key is a 404 page, and the rest is the
// object, an index document, a redirect or the error document.
func (p *Provider) serveWebsite(w http.ResponseWriter, rc *request.Context) {
	r := rc.Request
	bucketName, _, _ := strings.Cut(r.Host, websiteHostPart)

	if r.Method != http.MethodGet {
		writePage(w, http.StatusMethodNotAllowed, methodNotAllowedPage(r.Method, rc.RequestID))

		return
	}

	b, config, exists := p.websiteBucket(bucketName)

	switch {
	case !exists:
		writePage(w, http.StatusNotFound, notFoundPage("NoSuchBucket", messageNoSuchBucket, "BucketName", bucketName,
			rc.RequestID, ""))
	case config == nil:
		writePage(w, http.StatusNotFound, notFoundPage("NoSuchWebsiteConfiguration", messageNoSuchWebsite,
			"BucketName", bucketName, rc.RequestID, ""))
	default:
		p.serveSite(w, rc, b, config)
	}
}

func (p *Provider) serveSite(
	w http.ResponseWriter, rc *request.Context, b *Bucket, config *s3api.WebsiteConfiguration,
) {
	r := rc.Request
	page := pageURL(r)

	if redirectAll := config.RedirectAllRequestsTo; redirectAll != nil {
		page.Host = deref(redirectAll.HostName)
		if protocol := deref(redirectAll.Protocol); protocol != "" {
			page.Scheme = protocol
		}

		redirect(w, http.StatusMovedPermanently, page.String())

		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/")

	// Rules that match the key alone come before any lookup.
	if rule := findRule(config.RoutingRules, key, 0); rule != nil && key != "" {
		redirectByRule(w, page, key, rule)

		return
	}

	// A URL that ends in a slash names a folder, whose index document is served.
	folder := strings.HasSuffix(page.String(), "/")
	index := ""

	if config.IndexDocument != nil {
		index = deref(config.IndexDocument.Suffix)
	}

	if key == "" || folder {
		key += index
	}

	site, err := p.siteGet(b, key)
	if err != nil {
		p.siteMissing(w, rc, b, config, page, key)

		return
	}

	defer site.body.Close() //nolint:errcheck // a read-only blob.

	if location := site.version.WebsiteRedirectLocation; location != "" {
		redirect(w, http.StatusMovedPermanently, location)

		return
	}

	etag := quoted(site.version.ETag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	writeObject(w, http.StatusOK, site)
}

// siteMissing answers a key that is not there: the folder's index document, a
// rule that matches the 404, the error document, or the 404 page.
func (p *Provider) siteMissing(
	w http.ResponseWriter, rc *request.Context, b *Bucket, config *s3api.WebsiteConfiguration,
	page *url.URL, key string,
) {
	folder := strings.HasSuffix(page.String(), "/")

	if !folder && config.IndexDocument != nil {
		if siteExists(b, key+"/"+deref(config.IndexDocument.Suffix)) {
			redirect(w, http.StatusFound, "/"+key+"/")

			return
		}
	}

	if rule := findRule(config.RoutingRules, key, http.StatusNotFound); rule != nil {
		redirectByRule(w, page, strings.TrimPrefix(page.Path, "/"), rule)

		return
	}

	doc := config.ErrorDocument
	if doc == nil {
		writePage(w, http.StatusNotFound, notFoundPage("NoSuchKey", messageNoSuchKey, "Key", key, rc.RequestID, ""))

		return
	}

	errorKey := deref(doc.Key)

	site, err := p.siteGet(b, errorKey)
	if err != nil {
		writePage(w, http.StatusNotFound, notFoundPage("NoSuchKey", messageNoSuchKey, "Key", key, rc.RequestID, errorKey))

		return
	}

	defer site.body.Close() //nolint:errcheck // a read-only blob.

	if location := site.version.WebsiteRedirectLocation; location != "" {
		redirect(w, http.StatusMovedPermanently, location)

		return
	}

	writeObject(w, http.StatusNotFound, site)
}

// findRule returns the first routing rule that applies to key, and to the
// status when it is not zero (_find_matching_rule). A rule without a condition
// applies to everything.
func findRule(rules []s3api.RoutingRule, key string, status int) *s3api.RoutingRule {
	for i := range rules {
		rule := &rules[i]

		condition := rule.Condition
		if condition == nil {
			return rule
		}

		prefix, code := deref(condition.KeyPrefixEquals), deref(condition.HttpErrorCodeReturnedEquals)

		switch {
		case prefix != "" && code != "":
			if strings.HasPrefix(key, prefix) && strconv.Itoa(status) == code {
				return rule
			}
		case prefix != "":
			if strings.HasPrefix(key, prefix) {
				return rule
			}
		case code != "":
			if strconv.Itoa(status) == code {
				return rule
			}
		default:
			// A condition with neither member does not apply.
		}
	}

	return nil
}

// redirectByRule answers with the redirect a routing rule describes
// (_get_redirect_from_routing_rule): the request's URL with the rule's host,
// protocol and key applied. key is the key of the request.
func redirectByRule(w http.ResponseWriter, page *url.URL, key string, rule *s3api.RoutingRule) {
	to := *page

	target := s3api.Redirect{}
	if rule.Redirect != nil {
		target = *rule.Redirect
	}

	if host := deref(target.HostName); host != "" {
		to.Host = host
	}

	if protocol := deref(target.Protocol); protocol != "" {
		to.Scheme = protocol
	}

	switch {
	case deref(target.ReplaceKeyWith) != "":
		to.Path = "/" + *target.ReplaceKeyWith
	case target.ReplaceKeyPrefixWith != nil:
		// The value may be empty, which removes the prefix.
		var prefix string
		if rule.Condition != nil {
			prefix = deref(rule.Condition.KeyPrefixEquals)
		}

		to.Path = "/" + *target.ReplaceKeyPrefixWith + strings.TrimPrefix(key, prefix)
	default:
		// The path stays as it was.
	}

	code := http.StatusMovedPermanently
	n, err := strconv.Atoi(deref(target.HttpRedirectCode))
	if err == nil {
		code = n
	}

	redirect(w, code, to.String())
}

func redirect(w http.ResponseWriter, code int, location string) {
	h := w.Header()
	h.Set("Location", location)
	h.Set("Content-Type", contentTypeHTML)
	w.WriteHeader(code)
}

// writeObject sends an object as the website does: its Content-Type and ETag,
// and its body.
func writeObject(w http.ResponseWriter, status int, site *siteObject) {
	h := w.Header()
	h.Set("Content-Type", site.version.ContentType)
	h.Set("ETag", quoted(site.version.ETag))
	h.Set("Content-Length", strconv.FormatInt(site.version.Size, 10))
	w.WriteHeader(status)

	_, _ = io.Copy(w, site.body)
}

func writePage(w http.ResponseWriter, status int, page string) {
	w.Header().Set("Content-Type", contentTypeHTML)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, page)
}

// notFoundPage is the 404 page (_create_404_error_string) with its leading
// whitespace removed, as the handler's _flatten_html_response leaves it. A key
// the error document could not be read for adds the nested block.
func notFoundPage(code, message, resource, name, requestID, errorDocument string) string {
	var page strings.Builder

	page.WriteString("<html>\n<head><title>404 Not Found</title></head>\n<body>\n<h1>404 Not Found</h1>\n<ul>\n")
	page.WriteString("<li>Code: " + code + "</li>\n")
	page.WriteString("<li>Message: " + message + "</li>\n")
	page.WriteString("<li>" + resource + ": " + name + "</li>\n")
	page.WriteString("<li>RequestId: " + requestID + "</li>\n")
	page.WriteString("<li>HostId: " + websiteHostID + "</li>\n</ul>\n")

	if errorDocument != "" {
		page.WriteString("<h3>An Error Occurred While Attempting to Retrieve a Custom Error Document</h3>\n<ul>\n")
		page.WriteString("<li>Code: NoSuchKey</li>\n<li>Message: " + messageNoSuchKey + "</li>\n")
		page.WriteString("<li>Key: " + errorDocument + "</li>\n</ul>\n")
	}

	page.WriteString("<hr/>\n</body>\n</html>\n")

	return page.String()
}

// methodNotAllowedPage is the 405 page (_create_405_error_string).
func methodNotAllowedPage(method, requestID string) string {
	return "<html>\n<head><title>405 Method Not Allowed</title></head>\n<body>\n<h1>405 Method Not Allowed</h1>\n<ul>\n" +
		"<li>Code: MethodNotAllowed</li>\n" +
		"<li>Message: The specified method is not allowed against this resource.</li>\n" +
		"<li>Method: " + strings.ToUpper(method) + "</li>\n" +
		"<li>ResourceType: OBJECT</li>\n" +
		"<li>RequestId: " + requestID + "</li>\n" +
		"<li>HostId: " + websiteHostID + "</li>\n</ul>\n<hr/>\n</body>\n</html>\n"
}
