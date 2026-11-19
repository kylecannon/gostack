package account

import (
	"net/http"
	"regexp"
	"strings"
)

// DefaultRegion is the region of a call that names none, and the region an
// unknown one is rewritten to.
const DefaultRegion = "us-east-1"

// scopeFields is the number of "/" separated parts of a scopePattern match:
// "Credential=<key>", date, region and the empty rest.
const scopeFields = 4

// regions are the regions of the aws partition that have an S3 endpoint, which
// LocalStack takes as the regions available everywhere
// (Session().get_available_regions("s3", "aws")). aws-global is not one of them:
// the call is rewritten to us-east-1, as for any other unknown region.
//
// This list is a snapshot from botocore's endpoints data. It has not been
// diffed against botocore 1.42.59, which the repository does not vendor, so a
// region added after the snapshot would be rewritten to us-east-1.
var regions = map[string]struct{}{
	"af-south-1": {}, "ap-east-1": {}, "ap-east-2": {}, "ap-northeast-1": {},
	"ap-northeast-2": {}, "ap-northeast-3": {}, "ap-south-1": {}, "ap-south-2": {},
	"ap-southeast-1": {}, "ap-southeast-2": {}, "ap-southeast-3": {}, "ap-southeast-4": {},
	"ap-southeast-5": {}, "ap-southeast-6": {}, "ap-southeast-7": {},
	"ca-central-1": {}, "ca-west-1": {}, "eu-central-1": {}, "eu-central-2": {},
	"eu-north-1": {}, "eu-south-1": {}, "eu-south-2": {}, "eu-west-1": {},
	"eu-west-2": {}, "eu-west-3": {}, "il-central-1": {}, "me-central-1": {},
	"me-south-1": {}, "mx-central-1": {}, "sa-east-1": {}, "us-east-1": {},
	"us-east-2": {}, "us-west-1": {}, "us-west-2": {},
}

var scopePattern = regexp.MustCompile(`Credential=([^/]+)/([^/]+)/([^/]+)/`)

// Identity is the account and region a call acts on.
type Identity struct {
	// AccountID is the 12-digit account.
	AccountID string
	// Region is the region, already rewritten to us-east-1 when the caller's
	// was unknown.
	Region string
	// Authorization is the Authorization header the call carries from here on.
	// It differs from the request's only when the region was rewritten, in
	// which case its credential scope names the new region.
	Authorization string
}

// Resolve derives the account and region of a call from the credential
// FromRequest reads. A call without one gives the default account and
// us-east-1, and so does a SigV2 call for the region. An unknown region is
// rewritten in the Authorization header only: the query of a presigned URL is
// signed as it stands. parity is PARITY_AWS_ACCESS_KEY_ID.
func Resolve(r *http.Request, parity bool) Identity {
	cred, _ := FromRequest(r)
	authorization := r.Header.Get("Authorization")

	id := Identity{
		AccountID:     FromAccessKey(cred.AccessKey, parity),
		Region:        cred.Region,
		Authorization: authorization,
	}

	switch {
	case id.Region == "":
		id.Region = DefaultRegion
	case !KnownRegion(id.Region):
		id.Region = DefaultRegion
		id.Authorization = RewriteRegion(authorization, DefaultRegion)
	default:
		// A known region needs no rewrite.
	}

	return id
}

// KnownRegion reports whether region is one the gateway accepts as is.
func KnownRegion(region string) bool {
	_, ok := regions[region]

	return ok
}

// RewriteRegion returns authorization with the region of every credential scope
// replaced by region.
func RewriteRegion(authorization, region string) string {
	return scopePattern.ReplaceAllStringFunc(authorization, func(scope string) string {
		parts := strings.SplitN(scope, "/", scopeFields)

		return parts[0] + "/" + parts[1] + "/" + region + "/"
	})
}
