package account

import (
	"regexp"
	"slices"
	"strings"
)

// DefaultRegion is the region of a call that names none, and the region an
// unknown one is rewritten to.
const DefaultRegion = "us-east-1"

const (
	scopeMarker = "Credential="
	// scopeFields is the number of "/" separated parts that must follow
	// the marker: key, date, region and the rest of the scope.
	scopeFields = 4
	sigV4Prefix = "AWS4-"
	sigV2Prefix = "AWS "
)

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
	// It differs from the one passed to Resolve only when the region was
	// rewritten, in which case its credential scope names the new region.
	Authorization string
}

// Resolve derives the account and region of a call from its Authorization
// header. An absent header, or one without a credential scope, gives the
// default account and us-east-1. parity is PARITY_AWS_ACCESS_KEY_ID.
func Resolve(authorization string, parity bool) Identity {
	accessKey := AccessKeyFromAuthorization(authorization)
	if accessKey == "" {
		accessKey = Default
	}

	id := Identity{
		AccountID:     FromAccessKey(accessKey, parity),
		Region:        RegionFromAuthorization(authorization),
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

// AccessKeyFromAuthorization returns the access key ID of a SigV4 or SigV2
// Authorization header, or "" when there is none.
func AccessKeyFromAuthorization(authorization string) string {
	switch {
	case strings.HasPrefix(authorization, sigV4Prefix):
		fields, ok := credential(authorization)
		if !ok {
			return ""
		}

		return fields[0]
	case strings.HasPrefix(authorization, sigV2Prefix):
		key, _, _ := strings.Cut(strings.TrimPrefix(authorization, sigV2Prefix), ":")

		return key
	default:
		return ""
	}
}

// RegionFromAuthorization returns the region in the credential scope of an
// Authorization header, or "" when there is none.
func RegionFromAuthorization(authorization string) string {
	fields, ok := credential(authorization)
	if !ok {
		return ""
	}

	return fields[2]
}

// RewriteRegion returns authorization with the region of every credential scope
// replaced by region.
func RewriteRegion(authorization, region string) string {
	return scopePattern.ReplaceAllStringFunc(authorization, func(scope string) string {
		parts := strings.SplitN(scope, "/", scopeFields)

		return parts[0] + "/" + parts[1] + "/" + region + "/"
	})
}

// credential splits the first credential scope after "Credential=" into its
// access key, date and region. The ok result is false when there is none.
func credential(authorization string) ([3]string, bool) {
	_, scope, found := strings.Cut(authorization, scopeMarker)
	if !found {
		return [3]string{}, false
	}

	parts := strings.SplitN(scope, "/", scopeFields)
	if len(parts) < scopeFields {
		return [3]string{}, false
	}

	if slices.Contains(parts[:scopeFields-1], "") {
		return [3]string{}, false
	}

	return [3]string{parts[0], parts[1], parts[2]}, true
}
