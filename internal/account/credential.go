package account

import (
	"net/http"
	"strings"
)

const (
	// sigV4Prefix starts the scheme of a SigV4 Authorization header, matched
	// without regard to case: AWS4-HMAC-SHA256, or AWS4-ECDSA-P256-SHA256 for
	// SigV4a.
	sigV4Prefix = "AWS4-"
	// sigV2Prefix starts a SigV2 Authorization header, "AWS <key>:<signature>".
	sigV2Prefix = "AWS "
	// credentialParam names the credential scope in a SigV4 Authorization
	// header.
	credentialParam = "Credential"
	// queryCredential is the credential scope of a presigned SigV4 URL.
	queryCredential = "X-Amz-Credential" //nolint:gosec // G101: the name of a query parameter, not a credential.
	// queryAccessKey is the access key ID of a presigned SigV2 URL.
	queryAccessKey = "AWSAccessKeyId"
)

// Credential is what a request says about the key it is signed with.
type Credential struct {
	// AccessKey is the access key ID.
	AccessKey string
	// SigningName is the service name of the credential scope. It is set only
	// when the scope has all five fields, and it is empty for SigV2, which has
	// no scope.
	SigningName string
	// Region is the region of the credential scope, as sent: Resolve rewrites
	// one it does not know. It is empty for SigV2.
	Region string
}

// FromRequest returns the credential r is signed with. It is the one parser of
// credentials: Resolve reads the account and region from it, and route
// detection reads the signing name from FromAuthorization, its header half.
//
// A request with an Authorization header is read from the header alone: a SigV4
// header ("AWS4-HMAC-SHA256 Credential=<scope>, ...", the scheme in any case) or
// a SigV2 one ("AWS <key>:<signature>"). A request without one is read from the
// query string of a presigned URL: X-Amz-Credential for SigV4, else
// AWSAccessKeyId for SigV2. That is how LocalStack's S3 reads a presigned URL,
// which takes its account from the query only when there is no header.
//
// A scope is "<key>/<date>/<region>/<signing name>/aws4_request". It gives the
// key and region when its first three fields are not empty and a fourth
// follows, as LocalStack's account and region regexes read it; it gives the
// signing name whenever it has exactly five fields, as LocalStack's service
// router reads it.
//
// The result is false when there is no credential, or the one that decides is
// malformed: a scope short of four fields or with an empty key, date or region,
// or a SigV2 key that is empty. A malformed five-field scope still gives its
// signing name.
func FromRequest(r *http.Request) (Credential, bool) {
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		return parseAuthorization(authorization)
	}

	if r.URL.RawQuery == "" {
		return Credential{}, false
	}

	query := r.URL.Query()

	if query.Has(queryCredential) {
		return fromScope(query.Get(queryCredential))
	}

	if query.Has(queryAccessKey) {
		key := query.Get(queryAccessKey)

		return Credential{AccessKey: key}, key != ""
	}

	return Credential{}, false
}

// FromAuthorization returns the credential of r's Authorization header, as
// FromRequest reads it, and ignores the query string of a presigned URL. Route
// detection takes the signing name from it alone, as LocalStack's service
// router reads only the header (service_router.py:64-77): a presigned URL is
// routed by its path and host.
func FromAuthorization(r *http.Request) (Credential, bool) {
	return parseAuthorization(r.Header.Get("Authorization"))
}

// parseAuthorization reads a SigV4 or SigV2 Authorization header.
func parseAuthorization(authorization string) (Credential, bool) {
	if key, ok := strings.CutPrefix(authorization, sigV2Prefix); ok {
		key, _, _ = strings.Cut(key, ":")

		return Credential{AccessKey: key}, key != ""
	}

	scheme, params, _ := strings.Cut(strings.TrimSpace(authorization), " ")
	if len(scheme) < len(sigV4Prefix) || !strings.EqualFold(scheme[:len(sigV4Prefix)], sigV4Prefix) {
		return Credential{}, false
	}

	for param := range strings.SplitSeq(params, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if found && name == credentialParam {
			return fromScope(strings.Trim(value, `"`))
		}
	}

	return Credential{}, false
}

// completeScopeFields counts the fields of a complete credential scope.
const completeScopeFields = 5

// Scope holds the first four fields of a SigV4 credential scope,
// "<key>/<date>/<region>/<service>/<terminator>", as ParseScope reads them. A
// field may be empty. The terminator is counted and not kept: nothing reads it.
type Scope struct {
	// AccessKey is the access key ID.
	AccessKey string
	// Date is the scope date, yyyymmdd.
	Date string
	// Region is the region, as sent.
	Region string
	// Service is the signing name.
	Service string
}

// maxScopeFields is the count splitScope reports for a scope with more than
// completeScopeFields fields.
const maxScopeFields = completeScopeFields + 1

// splitScope is the one reader of a credential scope. It returns the fields it
// found, up to four, and how many the scope has: up to five, or maxScopeFields
// for a longer one. It does not allocate, as FromRequest runs it on every
// request.
func splitScope(scope string) (Scope, int) {
	var (
		s          Scope
		terminator string
	)

	slots := [...]*string{&s.AccessKey, &s.Date, &s.Region, &s.Service, &terminator}
	rest := scope

	for n, slot := range slots {
		field, tail, more := strings.Cut(rest, "/")
		*slot = field

		if !more {
			return s, n + 1
		}

		rest = tail
	}

	return s, maxScopeFields
}

// ParseScope splits a credential scope. It is true when the scope has exactly
// five fields, and it does not judge their values. A scope that is not complete
// still gives the leading fields it has. Presigned-URL validation and the
// POST form's credential read X-Amz-Credential through it; FromRequest reads
// the same scope through the same split.
func ParseScope(scope string) (Scope, bool) {
	s, fields := splitScope(scope)

	return s, fields == completeScopeFields
}

// QueryScope returns the X-Amz-Credential of r's query string, decoded as
// FromRequest decodes it, "" when there is none. A caller that validates a
// presigned URL reads the credential from here, so it sees the one FromRequest
// takes the account and region from.
func QueryScope(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}

	return r.URL.Query().Get(queryCredential)
}

// fromScope reads a credential scope, "<key>/<date>/<region>/<signing
// name>/aws4_request".
func fromScope(scope string) (Credential, bool) {
	s, fields := splitScope(scope)

	var cred Credential

	// The signing name is the fourth field of a scope with exactly five,
	// whatever the fields before it hold.
	if fields == completeScopeFields {
		cred.SigningName = s.Service
	}

	if s.AccessKey == "" || s.Date == "" || s.Region == "" || fields < completeScopeFields-1 {
		return cred, false
	}

	cred.AccessKey, cred.Region = s.AccessKey, s.Region

	return cred, true
}
