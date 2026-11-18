package s3

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
)

const (
	headerOrigin         = "Origin"
	headerRequestMethod  = "Access-Control-Request-Method"
	headerRequestHeaders = "Access-Control-Request-Headers"
	headerAllowOrigin    = "Access-Control-Allow-Origin"
	headerAllowCreds     = "Access-Control-Allow-Credentials"
	headerAllowMethods   = "Access-Control-Allow-Methods"
	headerAllowHeaders   = "Access-Control-Allow-Headers"
	headerExposeHeaders  = "Access-Control-Expose-Headers"
	headerMaxAge         = "Access-Control-Max-Age"

	corsWildcard = "*"
	corsVary     = "Origin, Access-Control-Request-Headers, Access-Control-Request-Method"

	maxCORSRules = 100

	messageCORSNoBucket = "CORSResponse: Bucket not found"
	messageCORSDisabled = "CORSResponse: CORS is not enabled for this bucket."
	messageCORSDenied   = "CORSResponse: This CORS request is not allowed. This is usually because the evalution of " +
		"Origin, request method / Access-Control-Request-Method or Access-Control-Request-Headers are not " +
		"whitelisted by the resource's CORS spec."
	messageCORSNoOrigin = "Insufficient information. Origin request header needed."
)

// corsMethods are the HTTP methods a CORS rule may allow.
var corsMethods = []string{http.MethodGet, http.MethodPut, http.MethodHead, http.MethodPost, http.MethodDelete}

// PutBucketCors stores the CORS rules of the bucket, as LocalStack's
// put_bucket_cors does: at most 100 rules, each with AllowedMethods and
// AllowedOrigins, and methods from GET, PUT, HEAD, POST and DELETE.
func (p *Provider) PutBucketCors(
	ctx context.Context, in *s3api.PutBucketCorsInput,
) (*s3api.PutBucketCorsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	err = validateCORS(in.CORSConfiguration)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.gone {
		return nil, noSuchBucket(b.Name)
	}

	b.CORS = in.CORSConfiguration

	return &s3api.PutBucketCorsOutput{}, nil
}

// GetBucketCors returns the CORS rules of the bucket.
func (p *Provider) GetBucketCors(
	ctx context.Context, in *s3api.GetBucketCorsInput,
) (*s3api.GetBucketCorsOutput, error) {
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

	if b.CORS == nil {
		return nil, (&s3api.NoSuchCORSConfiguration{BucketName: &b.Name}).Err("The CORS configuration does not exist")
	}

	return &s3api.GetBucketCorsOutput{CORSRules: b.CORS.CORSRules}, nil
}

// DeleteBucketCors removes the CORS rules of the bucket. A bucket without any
// is not an error.
func (p *Provider) DeleteBucketCors(
	ctx context.Context, in *s3api.DeleteBucketCorsInput,
) (*s3api.DeleteBucketCorsOutput, error) {
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

	b.CORS = nil

	return &s3api.DeleteBucketCorsOutput{}, nil
}

// validateCORS is validate_cors_configuration (validation.py:408). A rule that
// lacks AllowedMethods or AllowedOrigins is malformed; so is an empty list of
// rules, and so is a body with no configuration at all.
func validateCORS(config *s3api.CORSConfiguration) error {
	if config == nil || len(config.CORSRules) == 0 || len(config.CORSRules) > maxCORSRules {
		return malformedConfig()
	}

	for _, rule := range config.CORSRules {
		if len(rule.AllowedMethods) == 0 || len(rule.AllowedOrigins) == 0 {
			return malformedConfig()
		}

		for _, method := range rule.AllowedMethods {
			if !slices.Contains(corsMethods, method) {
				return invalidConfigRequest("Found unsupported HTTP method in CORS config. Unsupported method is " + method)
			}
		}
	}

	return nil
}

// bucketCORS returns the CORS rules of the named bucket in any account, whether
// the bucket exists, and its name when it does. Requests that carry CORS
// headers are not signed, so the bucket is found by name alone.
func (p *Provider) bucketCORS(name string) (rules []s3api.CORSRule, exists bool) {
	owner, ok := p.names.owner(name)
	if !ok {
		return nil, false
	}

	b := p.state(owner).get(name)
	if b == nil {
		return nil, false
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.gone {
		return nil, false
	}

	if b.CORS == nil {
		return nil, true
	}

	return b.CORS.CORSRules, true
}

// corsBucketName is the bucket a request addresses, for CORS: the first
// segment of the path, which preprocess has made the bucket for a virtual
// host, or the bucket of a website host.
func corsBucketName(r *http.Request) string {
	if bucket, _, ok := strings.Cut(r.Host, ".s3-website."); ok && isWebsiteHost(r.Host) {
		return bucket
	}

	first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	return first
}

// serveCORS is LocalStack's S3CorsHandler (services/s3/cors.py:134-290) for a
// request S3 owns, run before the request is parsed. It reports whether it has
// answered the request, which only a preflight (OPTIONS) is. For any other
// request it puts the CORS headers on w, and the handler adds the rest.
func (p *Provider) serveCORS(w http.ResponseWriter, rc *request.Context) bool {
	r := rc.Request
	if rc.CORS == nil || service.OwnerOf(r) != serviceName {
		return false
	}

	preflight := r.Method == http.MethodOptions

	origin := r.Header.Get(headerOrigin)
	if origin == "" {
		if preflight {
			writeCORSError(w, rc, awserr.Error{
				Code: "BadRequest", Message: messageCORSNoOrigin, StatusCode: http.StatusBadRequest,
			}, "", "")

			return true
		}

		// Without an Origin the request is not a cross-origin request.
		return false
	}

	rules, bucketExists := p.bucketCORS(corsBucketName(r))

	if len(rules) == 0 {
		message := messageCORSNoBucket
		if bucketExists {
			message = messageCORSDisabled
		}

		return defaultCORS(w, rc, message)
	}

	rule := matchCORSRule(rules, r)
	if rule == nil {
		if preflight {
			writeCORSError(w, rc, forbidden(messageCORSDenied), r.Header.Get(headerRequestMethod), "OBJECT")

			return true
		}

		return false
	}

	setCORSHeaders(w.Header(), r, rule, origin)

	if preflight {
		endPreflight(w, rc)

		return true
	}

	return false
}

// defaultCORS is what S3 does for a bucket that has no rules or does not exist:
// the gateway's own CORS policy, which allows its own origins.
func defaultCORS(w http.ResponseWriter, rc *request.Context, refusal string) bool {
	r := rc.Request
	preflight := r.Method == http.MethodOptions

	if rc.CORS.OriginAllowed(r.Header) {
		rc.CORS.AddHeaders(w.Header(), r)

		if preflight {
			endPreflight(w, rc)

			return true
		}

		return false
	}

	if !preflight {
		return false
	}

	method := r.Header.Get(headerRequestMethod)
	if method == "" {
		method = http.MethodOptions
	}

	writeCORSError(w, rc, forbidden(refusal), method, "BUCKET")

	return true
}

func forbidden(message string) awserr.Error {
	return awserr.Error{Code: "AccessForbidden", Message: message, StatusCode: http.StatusForbidden}
}

// writeCORSError answers a preflight with an S3 error that carries the HostId
// and, when they are not empty, the Method and ResourceType members.
func writeCORSError(w http.ResponseWriter, rc *request.Context, e awserr.Error, method, resourceType string) {
	restxml.WriteS3Error(w, &restxml.Envelope{RequestID: rc.RequestID}, &e, func(xw *restxml.Writer) {
		xw.String("HostId", hostID)

		if method != "" {
			xw.String("Method", method)
		}

		if resourceType != "" {
			xw.String("ResourceType", resourceType)
		}
	})
}

// endPreflight answers a preflight whose headers are set: an empty 200 with the
// request id and an x-amz-id-2 built from it, as stop_options_chain does.
func endPreflight(w http.ResponseWriter, rc *request.Context) {
	header := w.Header()
	header["x-amz-request-id"] = []string{rc.RequestID}
	header["x-amz-id-2"] = []string{"MzRISOwyjmnup" + rc.RequestID + "7/JypPGXLh0OVFGcJaaO3KW/hRAqKOpIEEp"}
	header.Del("Content-Type")

	w.WriteHeader(http.StatusOK)
}

// matchCORSRule returns the first rule the request satisfies (match_rules): the
// method, or the Access-Control-Request-Method of a preflight, is allowed; the
// Origin matches an AllowedOrigin, where "*" stands for one or more characters;
// and every Access-Control-Request-Headers entry is in AllowedHeaders.
func matchCORSRule(rules []s3api.CORSRule, r *http.Request) *s3api.CORSRule {
	method := r.Header.Get(headerRequestMethod)
	if method == "" {
		method = r.Method
	}

	origin := r.Header.Get(headerOrigin)
	requested := r.Header.Get(headerRequestHeaders)

	for i := range rules {
		rule := &rules[i]

		if !slices.Contains(rule.AllowedMethods, method) {
			continue
		}

		if !slices.Contains(rule.AllowedOrigins, corsWildcard) && !originMatches(rule.AllowedOrigins, origin) {
			continue
		}

		if requested != "" && !headersAllowed(rule.AllowedHeaders, requested) {
			continue
		}

		return rule
	}

	return nil
}

// originMatches reports whether origin matches one of allowed, as a pattern in
// which every "*" is ".+" and everything else is literal.
func originMatches(allowed []string, origin string) bool {
	for _, pattern := range allowed {
		expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".+") + "$"

		matched, err := regexp.MatchString(expr, origin)
		if err == nil && matched {
			return true
		}
	}

	return false
}

// headersAllowed reports whether every comma separated name in requested is an
// AllowedHeader, compared in lower case, or AllowedHeaders holds "*".
func headersAllowed(allowed []string, requested string) bool {
	if len(allowed) == 0 {
		return false
	}

	if slices.Contains(allowed, corsWildcard) {
		return true
	}

	lower := make([]string, len(allowed))
	for i, name := range allowed {
		lower[i] = strings.ToLower(name)
	}

	for name := range strings.SplitSeq(strings.ToLower(requested), ",") {
		if !slices.Contains(lower, strings.TrimSpace(name)) {
			return false
		}
	}

	return true
}

// setCORSHeaders puts the headers of a matched rule on header. As LocalStack
// does, Allow-Origin is the request's Origin unless the rule allows any origin.
func setCORSHeaders(header http.Header, r *http.Request, rule *s3api.CORSRule, origin string) {
	wildcard := slices.Contains(rule.AllowedOrigins, corsWildcard)

	if wildcard {
		header.Set(headerAllowOrigin, corsWildcard)
	} else {
		header.Set(headerAllowOrigin, origin)
		header.Set(headerAllowCreds, "true")
	}

	header.Set("Vary", corsVary)
	header.Set(headerAllowMethods, strings.Join(rule.AllowedMethods, ", "))

	if requested := r.Header.Get(headerRequestHeaders); requested != "" {
		names := strings.Split(requested, ",")
		for i, name := range names {
			names[i] = strings.ToLower(strings.TrimSpace(name))
		}

		header.Set(headerAllowHeaders, strings.Join(names, ", "))
	}

	if len(rule.ExposeHeaders) > 0 {
		header.Set(headerExposeHeaders, strings.Join(rule.ExposeHeaders, ", "))
	}

	if rule.MaxAgeSeconds != nil && *rule.MaxAgeSeconds != 0 {
		header.Set(headerMaxAge, strconv.Itoa(int(*rule.MaxAgeSeconds)))
	}
}

// defaultCORSWriter applies the gateway's CORS policy to the operations
// LocalStack's s3_cors_request_handler covers, ListBuckets and CreateBucket,
// when S3 owns the request: an origin the gateway does not allow gets 403, and
// the response gets the gateway's CORS headers.
type defaultCORSWriter struct {
	http.ResponseWriter

	rc    *request.Context
	wrote bool
}

func (w *defaultCORSWriter) WriteHeader(code int) {
	if !w.wrote && code >= http.StatusOK {
		w.wrote = true

		if op := w.rc.Operation; op == "ListBuckets" || op == "CreateBucket" {
			if !w.rc.CORS.ChecksDisabled() && !w.rc.CORS.OriginAllowed(w.rc.Request.Header) {
				code = http.StatusForbidden
			}

			w.rc.CORS.AddHeaders(w.Header(), w.rc.Request)
		}
	}

	w.ResponseWriter.WriteHeader(code)
}

func (w *defaultCORSWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // io.Writer: the error of the Write it delegates to.
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *defaultCORSWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// malformedConfig is the MalformedXML LocalStack raises for a configuration that
// parsed but is not valid.
func malformedConfig() *awserr.Error {
	return &awserr.Error{
		Code:       "MalformedXML",
		Message:    "The XML you provided was not well-formed or did not validate against our published schema",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// invalidConfigRequest is an InvalidRequest about a configuration.
func invalidConfigRequest(message string) *awserr.Error {
	return s3api.NewInvalidRequest(message)
}
