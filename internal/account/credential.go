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
// credentials: route detection reads the signing name from it, and Resolve the
// account and region.
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
// signing name only when it has exactly five fields, as LocalStack's service
// router reads it.
//
// The result is false when there is no credential, or the one that decides is
// malformed: a scope short of four fields or with an empty key, date or region,
// or a SigV2 key that is empty.
func FromRequest(r *http.Request) (Credential, bool) {
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		return fromAuthorization(authorization)
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

// fromAuthorization reads a SigV4 or SigV2 Authorization header.
func fromAuthorization(authorization string) (Credential, bool) {
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

// fromScope reads a credential scope, "<key>/<date>/<region>/<signing
// name>/aws4_request".
func fromScope(scope string) (Credential, bool) {
	key, rest, _ := strings.Cut(scope, "/")
	date, rest, _ := strings.Cut(rest, "/")
	region, rest, found := strings.Cut(rest, "/")

	if key == "" || date == "" || region == "" || !found {
		return Credential{}, false
	}

	cred := Credential{AccessKey: key, Region: region}

	// The signing name and the terminator, and nothing after them.
	if signingName, terminator, ok := strings.Cut(rest, "/"); ok && !strings.Contains(terminator, "/") {
		cred.SigningName = signingName
	}

	return cred, true
}
