package s3

import (
	"bytes"
	"context"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/kylecannon/gostack/internal/service"
)

// sniffLimit bounds how much of a POST body preprocess reads for the two legacy
// rules that look into the body. A multipart form names its fields before the
// file, and a DeleteObjects body opens with <Delete> and its first <Key>.
const sniffLimit = 64 << 10

// bodyKey carries what preprocess found at the start of a POST body to
// fallback, as a sniffed.
type bodyKey struct{}

// sniffed is what the start of a POST body showed.
type sniffed struct {
	// fields holds the names of the multipart form's fields that are not files,
	// which werkzeug's request.values includes.
	fields url.Values
	// deleteObjects is true when the body holds <Delete and <Key>.
	deleteObjects bool
}

// virtualHostPattern is LocalStack's S3_VIRTUAL_HOSTNAME_REGEX (utils.py:101),
// <bucket>.s3[.<region>].<anything>. Its unescaped dots match any character, as
// they do there.
var virtualHostPattern = regexp.MustCompile(`^(.*).s3.((?:us-gov|us|ap|ca|cn|eu|sa)-[a-z]+-\d)?.*`)

// presignParameters are the query parameters that make a request a presigned
// S3 call to legacy_s3_rules (service_router.py:263-276).
var presignParameters = []string{
	"AWSAccessKeyId", "Signature", "X-Amz-Algorithm", "X-Amz-Credential",
	"X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature",
}

// claimHost reports whether a request to host is S3's before any other
// detection, as LocalStack's S3 pre-router (services/s3/cors.py:61-92) and its
// data-plane rule for websites (service_router.py:216) decide it: S3's own host
// s3.<LOCALSTACK_HOST>, a virtual host <bucket>.s3[.<region>].<host>, and an S3
// website host.
func (p *Provider) claimHost(host string) bool {
	if rest, ok := strings.CutPrefix(host, "s3."); ok && strings.HasPrefix(rest, p.settings().hostname()) {
		return true
	}

	if isWebsiteHost(host) {
		return true
	}

	return strings.Contains(host, ".s3") && virtualHostPattern.MatchString(host)
}

// fallback is LocalStack's legacy_s3_rules (service_router.py:232-297), which
// take a request for S3 when no other detection step settles on one service.
// The rules run in LocalStack's order.
func fallback(r *http.Request) bool {
	stripped := strings.Trim(r.URL.Path, "/")
	values := requestValues(r)

	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && stripped != "":
		// :244, GET or HEAD of /<bucket>/<key...>.
		return true
	case stripped != "" && !strings.Contains(stripped, "/") &&
		(r.Method == http.MethodPut || (r.Method == http.MethodPost && values.Has("key"))):
		// :250 and :253, PUT of /<bucket>, and a form POST to it.
		return true
	case strings.Contains(r.UserAgent(), "aws-cli/"):
		// :258, the CLI's --no-sign-request.
		return true
	case slices.ContainsFunc(presignParameters, values.Has):
		// :263, a presigned URL.
		return true
	case r.Method == http.MethodPost && values.Has("delete") && sniffedBody(r).deleteObjects:
		// :279, DeleteObjects.
		return true
	case strings.Count(stripped, "/") >= 1 && r.Method == http.MethodPut:
		// :285, PUT of /<bucket>/<key...>.
		return true
	case strings.HasPrefix(r.Header.Get("Authorization"), "AWS "):
		// :291, SigV2.
		return true
	default:
		// :295, a virtual host, last because it is greedy.
		return virtualHostBucket(r.Host) != ""
	}
}

// isBucketPath reports whether the first segment of path names a bucket of
// any account.
func (p *Provider) isBucketPath(path string) bool {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if first == "" {
		return false
	}

	_, exists := p.names.owner(first)

	return exists
}

func isWebsiteHost(host string) bool {
	return strings.Contains(host, ".s3-website.")
}

// virtualHostBucket returns the bucket of a virtual-host request, or "" for a
// path-style one: uses_host_addressing (utils.py:538).
func virtualHostBucket(host string) string {
	if !strings.Contains(host, ".s3.") {
		return ""
	}

	match := virtualHostPattern.FindStringSubmatch(host)
	if match == nil {
		return ""
	}

	return match[1]
}

// preprocess owns the requests LocalStack's S3 pre-router recognizes
// (cors.py:61-92): a host S3 claims, and a path whose first segment is an
// existing bucket. Owned, they reach S3 before the signing name is read, with
// their body as sent and no root CORS. A virtual-host request is rewritten to
// path style first, so the generated router and readers see /<bucket>/<key>,
// as LocalStack's VirtualHostRewriter does (parser.py:1420-1495). Any other
// POST has the start of its body read for the legacy rules that look into it.
// The gateway's own paths are left alone.
func (p *Provider) preprocess(r *http.Request) *http.Request {
	if isGatewayPath(r.URL.Path) {
		return nil
	}

	if bucket := virtualHostBucket(r.Host); bucket != "" {
		return toPathStyle(service.Own(r, serviceName), bucket)
	}

	switch {
	case p.claimHost(r.Host) || p.isBucketPath(r.URL.Path):
		return service.Own(r, serviceName)
	case r.Method == http.MethodPost:
		return sniff(r)
	default:
		return nil
	}
}

// toPathStyle moves the bucket from the host to the start of the path.
// It changes r, which must be a copy of the gateway's request, as service.Own
// returns one.
func toPathStyle(r *http.Request, bucket string) *http.Request {
	rewritten := *r.URL
	rewritten.Path = "/" + bucket + rewritten.Path

	if rewritten.RawPath != "" {
		rewritten.RawPath = "/" + bucket + rewritten.RawPath
	}

	r.URL = &rewritten
	r.Host = strings.TrimPrefix(r.Host, bucket+".")

	return r
}

// sniff reads up to sniffLimit bytes of a multipart form or a DeleteObjects
// body and puts them back in front of the rest, for fallback's :253 and :279
// rules, which werkzeug answers by reading the whole body. Any other POST is
// left alone.
func sniff(r *http.Request) *http.Request {
	contentType := r.Header.Get("Content-Type")
	form := strings.HasPrefix(contentType, "multipart/form-data")
	deletion := strings.Contains(r.URL.RawQuery, "delete") && r.URL.Query().Has("delete")

	if (!form && !deletion) || r.Body == nil || r.Body == http.NoBody {
		return nil
	}

	head := make([]byte, sniffLimit)
	n, _ := io.ReadFull(r.Body, head)
	head = head[:n]

	r.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(head), r.Body), Closer: r.Body}

	found := sniffed{
		deleteObjects: deletion && bytes.Contains(head, []byte("<Delete")) && bytes.Contains(head, []byte("<Key>")),
	}

	if form {
		found.fields = formFields(contentType, head)
	}

	return r.WithContext(context.WithValue(r.Context(), bodyKey{}, found))
}

// replayBody reads what sniff read and then the rest, and closes the original.
type replayBody struct {
	io.Reader
	io.Closer
}

// formFields returns the names of the fields that are not files in the part of
// a multipart form that head holds.
func formFields(contentType string, head []byte) url.Values {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil
	}

	fields := url.Values{}
	reader := multipart.NewReader(bytes.NewReader(head), params["boundary"])

	for {
		part, partErr := reader.NextPart()
		if partErr != nil {
			return fields
		}

		if name := part.FormName(); name != "" && part.FileName() == "" {
			fields.Set(name, "")
		}
	}
}

func sniffedBody(r *http.Request) sniffed {
	found, _ := r.Context().Value(bodyKey{}).(sniffed)

	return found
}

// isGatewayPath reports whether path is one of the gateway's own, which no
// service claims.
func isGatewayPath(path string) bool {
	return strings.HasPrefix(path, "/_localstack") || strings.HasPrefix(path, "/_gostack") ||
		strings.HasPrefix(path, "/_aws")
}

// requestValues is werkzeug's request.values: the query string, the form body
// when the gateway has parsed it, and the multipart fields sniff found.
func requestValues(r *http.Request) url.Values {
	values := r.Form
	if values == nil {
		values = r.URL.Query()
	}

	fields := sniffedBody(r).fields
	if len(fields) == 0 {
		return values
	}

	merged := make(url.Values, len(values)+len(fields))
	maps.Copy(merged, values)

	for name, value := range fields {
		merged[name] = append(merged[name], value...)
	}

	return merged
}
