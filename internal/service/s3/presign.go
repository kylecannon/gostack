package s3

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SigV2 signs with HMAC-SHA1 by definition.
	"encoding/base64"
	"encoding/hex"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kylecannon/gostack/internal/account"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/sigv4"
)

const (
	// maxPresignExpires is the longest a SigV4 presigned URL is valid: a week.
	maxPresignExpires = 7 * 24 * 60 * 60

	amzDateLayout = "20060102T150405Z"
	serverTime    = "2006-01-02T15:04:05.000Z"

	// presignAccessKey and presignSecret are LocalStack's
	// DEFAULT_PRE_SIGNED_ACCESS_KEY_ID and _SECRET_ACCESS_KEY: the key a
	// request that names none is reported with, and the secret every presigned
	// request is checked with. internalSecret is the one of LocalStack's
	// internal access keys.
	presignAccessKey = "test"
	presignSecret    = "test"
	internalSecret   = "__internal_call__"

	paramAccessKeyV2 = "AWSAccessKeyId"
	paramAlgorithm   = "X-Amz-Algorithm"
	paramCredential  = "X-Amz-Credential" //nolint:gosec // G101: a query parameter name, not a credential.
	paramDate        = "X-Amz-Date"
	paramExpires     = "X-Amz-Expires"
	paramExpiresV2   = "Expires"
	paramSignatureV4 = "X-Amz-Signature"
	paramSecurity    = "x-amz-security-token"

	// hexGroup is the hex digits of the two bytes in a group of hexBytes.
	hexGroup = 4

	signatureMismatch = "The request signature we calculated does not match the signature you provided. " +
		"Check your key and signing method."
	requestExpired = "Request has expired"
)

var (
	signatureV2Params = []string{"Signature", paramExpiresV2, paramAccessKeyV2}
	signatureV4Params = []string{
		paramAlgorithm, paramCredential, paramDate, paramExpires, "X-Amz-SignedHeaders", paramSignatureV4,
	}

	// internalAccessKeys are INTERNAL_AWS_ACCESS_KEY_ID and
	// INTERNAL_RESOURCE_ACCOUNT, which sign with internalSecret.
	internalAccessKeys = []string{internalSecret, "949334387222"}

	// subresources are botocore's HmacV1Auth.QSAOfInterest in lower case: the
	// query parameters a SigV2 string to sign names after the path.
	subresources = map[string]bool{
		"accelerate": true, "acl": true, "cors": true, "defaultobjectacl": true, "location": true,
		"logging": true, "partnumber": true, "policy": true, "requestpayment": true, "torrent": true,
		"versioning": true, "versionid": true, "versions": true, "website": true, "uploads": true,
		"uploadid": true, "response-content-type": true, "response-content-language": true,
		"response-expires": true, "response-cache-control": true, "response-content-disposition": true,
		"response-content-encoding": true, "delete": true, "lifecycle": true, "tagging": true,
		"restore": true, "storageclass": true, "notification": true, "replication": true,
		"analytics": true, "metrics": true, "inventory": true, "select": true, "select-type": true,
		"object-lock": true,
	}

	// unsignableHeaders are botocore's SIGNED_HEADERS_BLACKLIST.
	unsignableHeaders = []string{"expect", "transfer-encoding", "user-agent", "x-amzn-trace-id"}

	hostIDMember = member{name: "HostId", value: hostID}
)

// serveAPI dispatches an S3 API call. A presigned URL is checked first, and the
// headers its query carries are moved into the request, so the generated
// decoder reads them (LocalStack's ParsePreSignedUrlRequest).
func (p *Provider) serveAPI(w http.ResponseWriter, rc *request.Context) {
	refused := p.checkPresigned(rc.Request)
	op, routed := s3api.Route(rc.Request)

	if refused != nil {
		rc.Operation = "GetObject"
		if routed {
			rc.Operation = op
		}

		refused.write(w, rc)

		return
	}

	if !routed {
		writeMethodNotAllowed(w, rc)

		return
	}

	rc.Operation = op

	if rc.Request.Method == http.MethodPost {
		w = &postWriter{ResponseWriter: w, rc: rc}
	}

	s3api.Serve(p, w, rc)
}

// writeMethodNotAllowed answers a request no S3 operation takes, such as a
// PATCH to a bucket or an object. LocalStack crashes into a 500 InternalError
// there ("Unable to find operation"), which
// gostack does not copy (docs/architecture.md, "Behavior the suite does not
// pin"). AWS's recorded 405s name the method and the resource type, as this
// does (test_s3.snapshot.json, the website 405 pages).
func writeMethodNotAllowed(w http.ResponseWriter, rc *request.Context) {
	r := rc.Request
	resource := ""

	if bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/"); bucket != "" {
		resource = resourceBucket

		if key != "" {
			resource = resourceObject
		}
	}

	writeErrorWithMethod(w, rc, *s3api.NewMethodNotAllowed(messageMethodNotAllowed), r.Method, resource)
}

// refusal is an error a presigned request gets before it is decoded, with the
// members LocalStack sends, in its order.
type refusal struct {
	err     *awserr.Error
	members []member
}

type member struct {
	name  string
	value string
}

func refuse(err *awserr.Error, members ...member) *refusal {
	return &refusal{err: err, members: members}
}

func (e *refusal) write(w http.ResponseWriter, rc *request.Context) {
	restxml.WriteS3Error(w, &restxml.Envelope{RequestID: rc.RequestID}, e.err, func(xw *restxml.Writer) {
		for _, m := range e.members {
			xw.String(m.name, m.value)
		}
	})
}

// checkPresigned is S3PreSignedURLRequestHandler. A request with a SigV2 or a
// SigV4 query parameter must carry the version's every parameter. Its query
// headers move into r either way; whether its signature matches and whether it
// has expired are checked only when Settings.ValidateSignatures is set.
func (p *Provider) checkPresigned(r *http.Request) *refusal {
	if r.URL.RawQuery == "" {
		return nil
	}

	args := parseQueryArgs(r.URL.RawQuery)

	switch {
	case args.hasAny(signatureV2Params):
		if !args.hasAll(signatureV2Params) {
			return refuse(s3api.NewAccessDenied(
				"Query-string authentication requires the Signature, Expires and AWSAccessKeyId parameters"),
				hostIDMember)
		}

		return p.checkSignatureV2(r, args)
	case args.hasAny(signatureV4Params):
		if !args.hasAll(signatureV4Params) {
			return refuse(s3api.NewAuthorizationQueryParametersError(
				"Query-string authentication version 4 requires the X-Amz-Algorithm, X-Amz-Credential, "+
					"X-Amz-Signature, X-Amz-Date, X-Amz-SignedHeaders, and X-Amz-Expires parameters."),
				hostIDMember)
		}

		return p.checkSignatureV4(r, args)
	default:
		return nil
	}
}

// checkSignatureV2 is validate_presigned_url_s3: expiry, then the HMAC-SHA1
// signature of the string botocore's HmacV1QueryAuth signs.
func (p *Provider) checkSignatureV2(r *http.Request, args queryArgs) *refusal {
	expires, err := strconv.ParseInt(strings.TrimSpace(args.get(paramExpiresV2)), 10, 64)
	if err != nil {
		return refuse(s3api.NewSignatureDoesNotMatch("Expires error?"))
	}

	if p.settings().ValidateSignatures {
		now := time.Now()
		if time.Unix(expires, 0).Before(now) {
			return refuse(s3api.NewAccessDenied(requestExpired),
				member{paramExpiresV2, isoTime(time.Unix(expires, 0))}, member{"ServerTime", isoTime(now)},
				hostIDMember)
		}

		accessKey := cmp.Or(args.get(paramAccessKeyV2), presignAccessKey)
		stringToSign := stringToSignV2(r.Method, signedHeadersV2(r, args), expires, resourceV2(r.URL.Path, args))
		provided := strings.ReplaceAll(args.get("Signature"), " ", "+")

		if !hmac.Equal([]byte(signV2(secretOf(accessKey), stringToSign)), []byte(provided)) {
			return refuse(s3api.NewSignatureDoesNotMatch(signatureMismatch),
				member{paramAccessKeyV2, accessKey}, hostIDMember, member{"SignatureProvided", provided},
				member{"StringToSign", stringToSign}, member{"StringToSignBytes", hexBytes(stringToSign)})
		}
	}

	var moved []member

	for _, name := range args.names {
		if !reservedV2(strings.ToLower(name)) {
			moved = append(moved, member{name, args.values[name]})
		}
	}

	addHeaders(r, moved)

	return nil
}

// checkSignatureV4 is validate_presigned_url_s3v4: the parameters are read,
// then come the headers present but not signed, the signature, which is tried
// again with other ports on the host, and expiry.
func (p *Provider) checkSignatureV4(r *http.Request, args queryArgs) *refusal {
	params, refused := readV4Params(args, account.QueryScope(r))
	if refused != nil {
		return refused
	}

	host, path := r.Host, r.URL.Path
	if sent, ok := sentVirtualHost(r); ok {
		if !validBucketName(sent.bucket) {
			return refuse(s3api.NewInvalidBucketName("The specified bucket is not valid."),
				member{"BucketName", sent.bucket})
		}

		host, path = sent.host, strings.TrimPrefix(path, "/"+sent.bucket)
	}

	req := newPresignedV4(r, args)
	addHeaders(r, req.moved)

	settings := p.settings()
	if !settings.ValidateSignatures {
		return nil
	}

	if params.expiresIn > maxPresignExpires {
		return refuse(s3api.NewAuthorizationQueryParametersError("X-Amz-Expires must be less than a week (in "+
			"seconds) that is, the given X-Amz-Expires must be less than 604800 seconds"), hostIDMember)
	}

	if len(req.unsigned) > 0 {
		return refuse(s3api.NewAccessDenied("There were headers present in the request which were not signed"),
			hostIDMember, member{"HeadersNotSigned", strings.Join(req.unsigned, ", ")})
	}

	refused = req.verify(r.Method, sigv4.EscapePath(path), host, settings.GatewayPort, params)
	if refused != nil {
		return refused
	}

	now := time.Now()
	if expiresAt := params.signedAt.Add(time.Duration(params.expiresIn) * time.Second); expiresAt.Before(now) {
		return refuse(s3api.NewAccessDenied(requestExpired),
			member{paramExpiresV2, isoTime(expiresAt)}, member{"ServerTime", isoTime(now)},
			member{paramExpires, strconv.FormatInt(params.expiresIn, 10)}, hostIDMember)
	}

	return nil
}

// paramsV4 are the parameters of a SigV4 presigned URL that the check reads.
type paramsV4 struct {
	accessKey string
	region    string
	amzDate   string
	signature string
	expiresIn int64
	signedAt  time.Time
}

// readV4Params reads the parameters, which are checked whether or not
// signatures are. credential is the X-Amz-Credential that account.FromRequest
// reads the account and region from, so a credential its decoder cannot read
// is malformed here too.
func readV4Params(args queryArgs, credential string) (paramsV4, *refusal) {
	scope, complete := account.ParseScope(credential)
	if !complete {
		return paramsV4{}, refuse(s3api.NewAuthorizationQueryParametersError("Error parsing the X-Amz-Credential "+
			`parameter; the Credential is mal-formed; expecting "<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request".`),
			hostIDMember)
	}

	expiresIn, err := strconv.ParseInt(strings.TrimSpace(args.get(paramExpires)), 10, 64)

	switch {
	case err != nil:
		return paramsV4{}, refuse(s3api.NewAuthorizationQueryParametersError("X-Amz-Expires should be a number"),
			hostIDMember)
	case expiresIn < 0:
		return paramsV4{}, refuse(s3api.NewAuthorizationQueryParametersError("X-Amz-Expires must be non-negative"),
			hostIDMember)
	default:
	}

	amzDate := args.get(paramDate)

	signedAt, err := time.Parse(amzDateLayout, amzDate)
	if err != nil {
		return paramsV4{}, refuse(s3api.NewAuthorizationQueryParametersError(
			`X-Amz-Date must be in the ISO8601 Long Format "yyyyMMdd'T'HHmmss'Z'"`), hostIDMember)
	}

	return paramsV4{
		accessKey: scope.AccessKey, region: scope.Region, amzDate: amzDate,
		signature: args.get(paramSignatureV4), expiresIn: expiresIn, signedAt: signedAt,
	}, nil
}

// verify checks the signature, with host and then with each of its
// permutations; the refusal shows what was signed with host.
func (req presignedV4) verify(method, path, host string, gatewayPort int, params paramsV4) *refusal {
	day := params.amzDate[:8]
	key := sigv4.SigningKey(secretOf(params.accessKey), day, params.region, serviceName)
	scope := sigv4.Scope(day, params.region, serviceName)

	signs := func(host string) (string, string, bool) {
		canonical := req.canonicalRequest(method, path, host)
		stringToSign := sigv4.StringToSign(params.amzDate, scope, canonical)

		return canonical, stringToSign, sigv4.Equal(sigv4.Signature(key, stringToSign), params.signature)
	}

	canonical, stringToSign, ok := signs(host)
	if ok || slices.ContainsFunc(hostPermutations(host, gatewayPort), func(candidate string) bool {
		_, _, matches := signs(candidate)

		return matches
	}) {
		return nil
	}

	return refuse(s3api.NewSignatureDoesNotMatch(signatureMismatch),
		member{paramAccessKeyV2, cmp.Or(params.accessKey, presignAccessKey)},
		member{"CanonicalRequest", canonical}, member{"CanonicalRequestBytes", hexBytes(canonical)},
		hostIDMember, member{"SignatureProvided", params.signature},
		member{"StringToSign", stringToSign}, member{"StringToSignBytes", hexBytes(stringToSign)})
}

// presignedV4 is what a SigV4 presigned request signed besides its host.
type presignedV4 struct {
	// query is the canonical query: every parameter but the signature.
	query string
	// headers are the signed headers present, by lower-case name, without
	// host.
	headers []sigv4.Header
	// payload is the payload hash the request signed.
	payload string
	// unsigned are the x-amz-* headers present that are not signed, sorted.
	unsigned []string
	// moved are the query parameters that are headers, which the request does
	// not have yet.
	moved []member
}

// newPresignedV4 is S3SigV4SignatureContext's reading of the request. A query
// parameter that is an x-amz-* header, If-Match or If-None-Match becomes a
// header; a signed one is also signed as one, and an x-amz-checksum-* one that
// is not signed is dropped, as the JS SDK adds it without knowing the value.
func newPresignedV4(r *http.Request, args queryArgs) presignedV4 {
	signed := strings.Split(strings.ToLower(args.get("X-Amz-SignedHeaders")), ";")

	present := make(map[string][]string, len(r.Header))
	for name, values := range r.Header {
		if lower := strings.ToLower(name); lower != "authorization" {
			present[lower] = append(present[lower], values...)
		}
	}

	var (
		req   presignedV4
		query = make([]sigv4.Pair, 0, len(args.names))
	)

	for _, name := range args.names {
		value := args.values[name]
		if name != paramSignatureV4 {
			query = append(query, sigv4.Pair{Key: name, Value: value})
		}

		lower := strings.ToLower(name)
		if slices.Contains(signatureV4Params, name) || !headerInQuery(lower) || present[lower] != nil {
			continue
		}

		switch {
		case slices.Contains(signed, lower):
			present[lower] = []string{value}
			req.moved = append(req.moved, member{lower, value})
		case !strings.HasPrefix(lower, "x-amz-checksum-"):
			req.moved = append(req.moved, member{lower, value})
		default:
		}
	}

	req.query = sigv4.CanonicalQuery(query)
	req.payload = sigv4.UnsignedPayload

	contentSHA256 := strings.ToLower(headerContentSHA256)

	for _, name := range slices.Sorted(maps.Keys(present)) {
		isSigned := slices.Contains(signed, name)

		switch {
		case strings.HasPrefix(name, "x-amz-") && !isSigned && name != contentSHA256:
			req.unsigned = append(req.unsigned, name)
		case !isSigned || name == "host" || slices.Contains(unsignableHeaders, name):
		case name == contentSHA256:
			req.payload = strings.Join(present[name], ",")

			fallthrough
		default:
			for _, value := range present[name] {
				req.headers = append(req.headers, sigv4.Header{Name: name, Value: value})
			}
		}
	}

	return req
}

// canonicalRequest is the canonical request of method on path, signed with
// host as its host header.
func (req presignedV4) canonicalRequest(method, path, host string) string {
	headers := append(slices.Clip(req.headers), sigv4.Header{Name: "host", Value: host})
	block, signed := sigv4.CanonicalHeaders(headers)

	return sigv4.CanonicalRequest(method, path, req.query, block, signed, req.payload)
}

// headerInQuery reports whether a query parameter is a header botocore or the
// JS SDK put in a presigned URL.
func headerInQuery(lower string) bool {
	return strings.HasPrefix(lower, "x-amz-") || lower == "if-match" || lower == "if-none-match"
}

// hostPermutations are the hosts a SigV4 signature is tried with besides the
// one sent: its port replaced by 80, 443, the gateway's and none, or each of
// them added to a host that has none (PORT_REPLACEMENT).
func hostPermutations(host string, gatewayPort int) []string {
	ports := []string{":80", ":443"}
	if gatewayPort > 0 {
		ports = append(ports, ":"+strconv.Itoa(gatewayPort))
	}

	ports = append(ports, "")

	name, port := host, ""
	if i := strings.LastIndexByte(host, ':'); i >= 0 && isDigits(host[i+1:]) {
		name, port = host[:i], host[i:]
	}

	hosts := make([]string, 0, len(ports))

	for _, candidate := range ports {
		if candidate != port {
			hosts = append(hosts, name+candidate)
		}
	}

	return hosts
}

// signedHeadersV2 are the headers botocore's HmacV1QueryAuth signed, by
// lower-case name: the query parameters that are not its own and the session
// token, and the request's x-amz-*, Content-Type, Content-MD5 and Date
// headers, which win.
func signedHeadersV2(r *http.Request, args queryArgs) map[string]string {
	headers := make(map[string]string, len(args.names))

	for _, name := range args.names {
		if lower := strings.ToLower(name); !reservedV2(lower) || lower == paramSecurity {
			headers[lower] = args.values[name]
		}
	}

	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || lower == "content-type" || lower == "date" || lower == "content-md5" {
			headers[lower] = values[len(values)-1]
		}
	}

	return headers
}

// stringToSignV2 is HmacV1Auth.canonical_string with the expiry in place of
// the date.
func stringToSignV2(method string, headers map[string]string, expires int64, resource string) string {
	var b strings.Builder

	b.WriteString(strings.ToUpper(method))
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(headers["content-md5"]))
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(headers["content-type"]))
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(expires, 10))
	b.WriteByte('\n')

	for _, name := range slices.Sorted(maps.Keys(headers)) {
		if strings.HasPrefix(name, "x-amz-") {
			b.WriteString(name)
			b.WriteByte(':')
			b.WriteString(strings.TrimSpace(headers[name]))
			b.WriteByte('\n')
		}
	}

	b.WriteString(resource)

	return b.String()
}

// resourceV2 is HmacV1Auth.canonical_resource: the encoded path and the
// subresources, sorted, a bare name when its value is empty.
func resourceV2(path string, args queryArgs) string {
	var named []string

	for _, name := range args.names {
		if subresources[strings.ToLower(name)] {
			named = append(named, name)
		}
	}

	slices.Sort(named)

	var b strings.Builder

	b.WriteString(sigv4.EscapePath(path))

	for i, name := range named {
		separator := byte('&')
		if i == 0 {
			separator = '?'
		}

		b.WriteByte(separator)
		b.WriteString(name)

		if value := args.values[name]; value != "" {
			b.WriteByte('=')
			b.WriteString(value)
		}
	}

	return b.String()
}

// reservedV2 reports whether a query parameter, in lower case, is one SigV2
// does not move into the headers: its own and the subresources.
func reservedV2(lower string) bool {
	switch lower {
	case fieldSignatureV2, "expires", fieldAccessKeyV2, paramSecurity:
		return true
	default:
		return subresources[lower]
	}
}

func signV2(secret, stringToSign string) string {
	h := hmac.New(sha1.New, []byte(secret))
	h.Write([]byte(stringToSign))

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// secretOf is the secret a presigned request of accessKey is checked with.
// LocalStack reads an IAM user's from moto; gostack has no IAM.
func secretOf(accessKey string) string {
	if slices.Contains(internalAccessKeys, accessKey) {
		return internalSecret
	}

	return presignSecret
}

// addHeaders adds to r each header it does not have yet. The header map is
// copied first, as r shares it with the gateway's request.
func addHeaders(r *http.Request, headers []member) {
	if len(headers) == 0 {
		return
	}

	r.Header = r.Header.Clone()

	for _, h := range headers {
		if len(r.Header.Values(h.name)) == 0 {
			r.Header.Add(h.name, h.value)
		}
	}
}

// hexBytes is Python's bytes.hex(" ", 2).upper(), which StringToSignBytes and
// CanonicalRequestBytes use: groups of two bytes counted from the end.
func hexBytes(s string) string {
	digits := strings.ToUpper(hex.EncodeToString([]byte(s)))
	if digits == "" {
		return ""
	}

	head := len(digits) % hexGroup
	if head == 0 {
		head = hexGroup
	}

	var b strings.Builder

	b.Grow(len(digits) + len(digits)/hexGroup)
	b.WriteString(digits[:head])

	for i := head; i < len(digits); i += hexGroup {
		b.WriteByte(' ')
		b.WriteString(digits[i : i+hexGroup])
	}

	return b.String()
}

func isoTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(serverTime)
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// queryArgs are a query string's parameters as werkzeug's request.args reads
// them: split on '&' alone, decoded, each name's first value, in the order the
// names first appear.
type queryArgs struct {
	names  []string
	values map[string]string
}

func parseQueryArgs(raw string) queryArgs {
	args := queryArgs{values: make(map[string]string)}

	for pair := range strings.SplitSeq(raw, "&") {
		if pair == "" {
			continue
		}

		name, value, _ := strings.Cut(pair, "=")
		name = unquotePlus(name)

		if _, seen := args.values[name]; seen {
			continue
		}

		args.names = append(args.names, name)
		args.values[name] = unquotePlus(value)
	}

	return args
}

func (a queryArgs) has(name string) bool {
	_, ok := a.values[name]

	return ok
}

func (a queryArgs) get(name string) string {
	return a.values[name]
}

func (a queryArgs) hasAny(names []string) bool {
	return slices.ContainsFunc(names, a.has)
}

func (a queryArgs) hasAll(names []string) bool {
	return !slices.ContainsFunc(names, func(name string) bool { return !a.has(name) })
}

// unquotePlus decodes '+' as a space and each valid %XX escape, and leaves an
// invalid escape as it is, as Python's unquote_plus does.
func unquotePlus(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}

	var b strings.Builder

	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s):
			decoded, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				b.WriteByte(c)

				continue
			}

			b.WriteByte(byte(decoded))

			i += 2
		default:
			b.WriteByte(c)
		}
	}

	return b.String()
}

// postWriter drops the body of a PostObject answer other than 201, which S3
// sends empty: the generated writer always writes a PostResponse.
type postWriter struct {
	http.ResponseWriter

	rc   *request.Context
	drop bool
}

func (w *postWriter) WriteHeader(code int) {
	w.drop = w.rc.Operation == "PostObject" && code != http.StatusCreated && code < http.StatusBadRequest
	w.ResponseWriter.WriteHeader(code)
}

func (w *postWriter) Write(p []byte) (int, error) {
	if w.drop {
		return len(p), nil
	}

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // io.Writer: the error of the Write it delegates to.
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *postWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
