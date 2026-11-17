package gateway

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/cors"
)

const (
	headerOrigin            = "Origin"
	headerReferer           = "Referer"
	headerAllowOrigin       = "Access-Control-Allow-Origin"
	headerAllowMethods      = "Access-Control-Allow-Methods"
	headerAllowHeaders      = "Access-Control-Allow-Headers"
	headerExposeHeaders     = "Access-Control-Expose-Headers"
	headerRequestPrivate    = "Access-Control-Request-Private-Network"
	headerAllowPrivate      = "Access-Control-Allow-Private-Network"
	corsLocalhost           = "localhost"
	corsLocalhostHostname   = "localhost.localstack.cloud"
	corsAllowedMethods      = "HEAD,GET,PUT,POST,DELETE,OPTIONS,PATCH"
	dynamicOriginDomainPart = 2
	dynamicOriginPortPart   = 3
)

// corsAllowedHeaders are the request headers that browsers may send, from
// CORS_ALLOWED_HEADERS in LocalStack's aws/handlers/cors.py.
var corsAllowedHeaders = []string{
	"authorization",
	"cache-control",
	"content-length",
	"content-md5",
	"content-type",
	"etag",
	"location",
	"x-amz-acl",
	"x-amz-content-sha256",
	"x-amz-date",
	"x-amz-request-id",
	"x-amz-security-token",
	"x-amz-tagging",
	"x-amz-target",
	"x-amz-user-agent",
	"x-amz-version-id",
	"x-amzn-requestid",
	"x-localstack-target",
	// for AWS SDK v3
	"amz-sdk-invocation-id",
	"amz-sdk-request",
	// for lambda
	"x-amz-log-type",
}

// corsExposeHeaders are the response headers that browsers may read, from
// CORS_EXPOSE_HEADERS.
var corsExposeHeaders = []string{
	"etag",
	"x-amz-version-id",
	// for lambda
	"x-amz-log-result",
	"x-amz-executed-version",
	"x-amz-function-error",
}

// dynamicOrigins are the origins of S3 websites and CloudFront distributions
// served from an internal domain, whatever their bucket or distribution name.
var dynamicOrigins = []*regexp.Regexp{
	regexp.MustCompile(`^(.*)\.s3-website\.(.[^:]*)(:[0-9]{2,5})?`),
	regexp.MustCompile(`^(.*)\.cloudfront\.(.[^:]*)(:[0-9]{2,5})?`),
}

// corsRules are LocalStack's CORS rules, built once from the settings the
// gateway starts with, like the module-level constants of cors.py.
type corsRules struct {
	origins      []string
	domains      map[string]struct{}
	ports        map[string]struct{}
	allowHeaders string
	expose       string
}

func newCORSRules(s Settings) *corsRules {
	rules := &corsRules{
		domains: map[string]struct{}{corsLocalhost: {}, corsLocalhostHostname: {}},
		ports:   make(map[string]struct{}),
		origins: []string{
			// the web app and localhost domains
			"https://app.localstack.cloud",
			"http://app.localstack.cloud", //nolint:revive // LocalStack lists this plain http origin too.
			"https://localhost",
			"https://localhost.localstack.cloud",
			// requests from Electron apps, such as DynamoDB NoSQL Workbench
			"file://",
		},
	}

	if s.LocalstackHost != "" {
		rules.domains[s.LocalstackHost] = struct{}{}
	}

	for _, address := range s.Listen {
		_, port, err := net.SplitHostPort(address)
		if err == nil {
			rules.ports[port] = struct{}{}
		}
	}

	for _, scheme := range []string{"http", "https"} {
		for port := range rules.ports {
			rules.origins = append(rules.origins,
				scheme+"://"+corsLocalhost+":"+port,
				scheme+"://"+corsLocalhostHostname+":"+port)
		}
	}

	for origin := range strings.SplitSeq(s.ExtraCORSAllowedOrigins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			rules.origins = append(rules.origins, origin)
		}
	}

	allowHeaders := corsAllowedHeaders
	if extra := strings.TrimSpace(s.ExtraCORSAllowedHeaders); extra != "" {
		allowHeaders = append(allowHeaders[:len(allowHeaders):len(allowHeaders)], strings.Split(extra, ",")...)
	}

	expose := corsExposeHeaders
	if extra := strings.TrimSpace(s.ExtraCORSExposeHeaders); extra != "" {
		expose = append(expose[:len(expose):len(expose)], strings.Split(extra, ",")...)
	}

	rules.allowHeaders = strings.Join(allowHeaders, ",")
	rules.expose = strings.Join(expose, ",")

	return rules
}

// requestAllowed is CorsEnforcer.is_cors_origin_allowed: the Origin decides, or
// else the origin part of Referer. A request with neither is allowed, because
// command line clients send neither.
func (c *corsRules) requestAllowed(header http.Header) bool {
	if origin := header.Get(headerOrigin); origin != "" {
		return c.originAllowed(origin)
	}

	if referer := header.Get(headerReferer); referer != "" {
		parsed, err := url.Parse(referer)
		if err != nil {
			return false
		}

		return c.originAllowed(parsed.Scheme + "://" + parsed.Host)
	}

	return true
}

func (c *corsRules) originAllowed(origin string) bool {
	for _, allowed := range c.origins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}

	for _, pattern := range dynamicOrigins {
		match := pattern.FindStringSubmatch(origin)
		if match == nil {
			continue
		}

		if _, ok := c.domains[match[dynamicOriginDomainPart]]; !ok {
			continue
		}

		if port := match[dynamicOriginPortPart]; port == "" || c.hasPort(port[1:]) {
			return true
		}
	}

	return false
}

func (c *corsRules) hasPort(port string) bool {
	// Compare as numbers, so "04566" is port 4566.
	number, err := strconv.Atoi(port)
	if err != nil {
		return false
	}

	_, ok := c.ports[strconv.Itoa(number)]

	return ok
}

// setHeaders adds the headers LocalStack's CorsResponseEnricher sends to a
// request that has an Origin. go-chi/cors has set the Allow-Origin and
// Allow-Credentials already.
func (c *corsRules) setHeaders(header http.Header, r *http.Request) {
	// go-chi/cors answers a preflight with the one method and the headers
	// asked for. LocalStack lists everything it allows, on every request.
	header.Set(headerAllowMethods, corsAllowedMethods)
	header.Set(headerAllowHeaders, c.allowHeaders)
	header.Set(headerExposeHeaders, c.expose)

	if r.Header.Get(headerRequestPrivate) == valueTrue {
		header.Set(headerAllowPrivate, valueTrue)
	}

	// The response depends on the Origin, so say it exactly once.
	header.Set("Vary", headerOrigin)
}

// corsMiddleware is root step 5, LocalStack's CorsEnforcer and
// CorsResponseEnricher in one.
//
// A request from an origin that is not allowed gets 403 and nothing else, not
// even the identification headers: LocalStack terminates its chain there. A
// request that has an Origin is passed through go-chi/cors, which decides the
// Allow-Origin and Allow-Credentials headers, with LocalStack's origin rules as
// AllowOriginFunc; under DISABLE_CORS_CHECKS every origin gets "*" and no
// credentials. An OPTIONS request is answered with 204 unless
// DISABLE_PREFLIGHT_PROCESSING is set.
func (g *Gateway) corsMiddleware(next http.Handler) http.Handler {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := g.settings()

		if _, hasOrigin := r.Header[headerOrigin]; hasOrigin && !s.DisableCORSHeaders {
			g.cors.setHeaders(w.Header(), r)
		}

		if r.Method == http.MethodOptions && !s.DisablePreflightProcessing {
			g.identify(w.Header())
			w.WriteHeader(http.StatusNoContent)

			return
		}

		next.ServeHTTP(w, r)
	})

	enforced := g.corsEnforced.Handler(final)
	open := g.corsOpen.Handler(final)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := g.settings()

		if !s.DisableCORSChecks && !g.cors.requestAllowed(r.Header) {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		_, hasOrigin := r.Header[headerOrigin]

		switch {
		case !hasOrigin || s.DisableCORSHeaders:
			final.ServeHTTP(w, r)
		case s.DisableCORSChecks:
			open.ServeHTTP(w, r)
		default:
			enforced.ServeHTTP(w, r)
		}
	})
}

// newCORSHandlers builds the two go-chi/cors handlers: one that allows the
// origins in rules and one that allows all.
func newCORSHandlers(rules *corsRules) (enforced, open *cors.Cors) {
	methods := strings.Split(corsAllowedMethods, ",")

	enforced = cors.New(cors.Options{
		AllowOriginFunc: func(_ *http.Request, origin string) bool {
			return rules.originAllowed(origin)
		},
		AllowedMethods:     methods,
		AllowedHeaders:     []string{"*"},
		ExposedHeaders:     corsExposeHeaders,
		AllowCredentials:   true,
		OptionsPassthrough: true,
	})

	// A "*" origin cannot carry credentials, because the browser would refuse
	// the response.
	open = cors.New(cors.Options{
		AllowedOrigins:     []string{"*"},
		AllowedMethods:     methods,
		AllowedHeaders:     []string{"*"},
		ExposedHeaders:     corsExposeHeaders,
		AllowCredentials:   false,
		OptionsPassthrough: true,
	})

	return enforced, open
}
