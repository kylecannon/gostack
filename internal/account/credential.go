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

// Scope is the credential scope of a SigV4 request, "<key>/<date>/<region>/<service>/<terminator>",
// as ParseScope reads it. Its fields may be empty.
type Scope struct {
	// AccessKey is the access key ID.
	AccessKey string
	// Date is the scope date, yyyymmdd.
	Date string
	// Region is the region, as sent.
	Region string
	// Service is the signing name.
	Service string
	// Terminator is the last field, aws4_request for SigV4.
	Terminator string
}

// ParseScope splits a credential scope into its five fields. It is false when
// the scope does not have exactly five, and it does not judge their values.
// Presigned-URL validation reads the scope of X-Amz-Credential through it, with
// the same five-field rule FromRequest applies to the signing name.
func ParseScope(scope string) (Scope, bool) {
	fields := strings.Split(scope, "/")
	if len(fields) != completeScopeFields {
		return Scope{}, false
	}

	return Scope{
		AccessKey: fields[0], Date: fields[1], Region: fields[2], Service: fields[3], Terminator: fields[4],
	}, true
}

// fromScope reads a credential scope, "<key>/<date>/<region>/<signing
// name>/aws4_request".
func fromScope(scope string) (Credential, bool) {
	var cred Credential

	fields := strings.Split(scope, "/")

	// The signing name is the fourth of exactly five fields, whatever the
	// fields before it hold.
	if len(fields) == completeScopeFields {
		cred.SigningName = fields[3]
	}

	// The key, date and region, then a fourth field, which a longer scope has
	// too.
	if len(fields) < completeScopeFields-1 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return cred, false
	}

	cred.AccessKey, cred.Region = fields[0], fields[2]

	return cred, true
}
