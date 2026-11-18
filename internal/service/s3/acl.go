package s3

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// The group URIs are identifiers S3 defines, which are never fetched.
const (
	//nolint:revive // an identifier, not a link.
	groupAllUsers = "http://acs.amazonaws.com/groups/global/AllUsers"
	//nolint:revive // an identifier, not a link.
	groupAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
	//nolint:revive // an identifier, not a link.
	groupLogDelivery = "http://acs.amazonaws.com/groups/s3/LogDelivery"

	granteeCanonicalUser = "CanonicalUser"
	granteeGroup         = "Group"
	granteeEmail         = "AmazonCustomerByEmail"

	permissionFullControl = "FULL_CONTROL"
	permissionRead        = "READ"
	permissionReadACP     = "READ_ACP"
	permissionWrite       = "WRITE"
	permissionWriteACP    = "WRITE_ACP"

	cannedPrivate           = "private"
	cannedPublicRead        = "public-read"
	cannedPublicReadWrite   = "public-read-write"
	cannedAuthenticatedRead = "authenticated-read"
	cannedLogDeliveryWrite  = "log-delivery-write"
	cannedBucketOwnerFull   = "bucket-owner-full-control"
	canonicalIDLength       = 64
)

var (
	// predefinedGroups are the groups a grant may name by URI.
	predefinedGroups = []string{groupAllUsers, groupAuthenticatedUsers, groupLogDelivery}

	// validPermissions are the permissions a grant takes.
	validPermissions = []string{
		permissionFullControl, permissionRead, permissionReadACP, permissionWrite, permissionWriteACP,
	}

	// cannedACLs are the BucketCannedACL and ObjectCannedACL values of the spec,
	// log-delivery-write included.
	cannedACLs = []string{
		cannedPrivate, cannedPublicRead, cannedPublicReadWrite, cannedAuthenticatedRead, cannedLogDeliveryWrite,
		"bucket-owner-read", cannedBucketOwnerFull, "aws-exec-read",
	}
)

// grantHeaders are the x-amz-grant-* headers of a request.
type grantHeaders struct {
	fullControl, read, readACP, write, writeACP *string
}

// headerGrant is one grant header that the request carries.
type headerGrant struct {
	permission string
	header     string
	value      string
}

// present returns the grant headers the request carries, in the order
// LocalStack reads them.
func (g grantHeaders) present() []headerGrant {
	var grants []headerGrant

	for _, entry := range []headerGrant{
		{permissionFullControl, "x-amz-grant-full-control", deref(g.fullControl)},
		{permissionRead, "x-amz-grant-read", deref(g.read)},
		{permissionReadACP, "x-amz-grant-read-acp", deref(g.readACP)},
		{permissionWrite, "x-amz-grant-write", deref(g.write)},
		{permissionWriteACP, "x-amz-grant-write-acp", deref(g.writeACP)},
	} {
		if entry.value != "" {
			grants = append(grants, entry)
		}
	}

	return grants
}

// bucketOwner is the owner of every bucket and object here.
func bucketOwner() *s3api.Owner {
	return &s3api.Owner{ID: new(ownerID)}
}

// cannedPolicy is the access control policy of a canned ACL.
func cannedPolicy(canned string) *s3api.AccessControlPolicy {
	owner := bucketOwner()
	grants := []s3api.Grant{{
		Grantee:    &s3api.Grantee{ID: owner.ID, Type: new(granteeCanonicalUser)},
		Permission: new(permissionFullControl),
	}}

	group := func(uri, permission string) s3api.Grant {
		return s3api.Grant{Grantee: &s3api.Grantee{URI: &uri, Type: new(granteeGroup)}, Permission: &permission}
	}

	switch canned {
	case cannedPublicRead:
		grants = append(grants, group(groupAllUsers, permissionRead))
	case cannedPublicReadWrite:
		grants = append(grants, group(groupAllUsers, permissionRead), group(groupAllUsers, permissionWrite))
	case cannedAuthenticatedRead:
		grants = append(grants, group(groupAuthenticatedUsers, permissionRead))
	case cannedLogDeliveryWrite:
		grants = append(grants, group(groupLogDelivery, permissionReadACP), group(groupLogDelivery, permissionWrite))
	default:
		// private and the owner-oriented canned ACLs grant nobody else.
	}

	return &s3api.AccessControlPolicy{Owner: owner, Grants: grants}
}

// aclOrDefault returns acp, or the private policy for a resource that never set one.
func aclOrDefault(acp *s3api.AccessControlPolicy) *s3api.AccessControlPolicy {
	if acp == nil {
		return cannedPolicy(cannedPrivate)
	}

	return acp
}

func validateCanned(canned string) error {
	if canned != "" && !slices.Contains(cannedACLs, canned) {
		return invalidArgument("", "x-amz-acl", &canned)
	}

	return nil
}

// validCanonicalID reports whether id is a 64 digit hex number that is not zero.
func validCanonicalID(id string) bool {
	if len(id) != canonicalIDLength {
		return false
	}

	nonZero := false

	for _, c := range id {
		switch {
		case c >= '1' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			nonZero = true
		case c == '0':
		default:
			return false
		}
	}

	return nonZero
}

// parseGrantHeader reads one x-amz-grant-* header: grantees as uri="...",
// id="..." or emailAddress="...", separated by commas.
func parseGrantHeader(entry headerGrant) ([]s3api.Grant, error) {
	var grants []s3api.Grant

	for raw := range strings.SplitSeq(entry.value, ",") {
		raw = strings.TrimSpace(raw)

		kind, value, found := strings.Cut(raw, "=")
		if !found {
			return nil, invalidArgument("Argument format not recognized", entry.header, &raw)
		}

		value = strings.Trim(value, `"`)

		var grantee *s3api.Grantee

		switch kind {
		case "uri":
			if !slices.Contains(predefinedGroups, value) {
				return nil, invalidArgument("Invalid group uri", "uri", &value)
			}

			grantee = &s3api.Grantee{Type: new(granteeGroup), URI: &value}
		case "id":
			if !validCanonicalID(value) {
				return nil, invalidArgument("Invalid id", "id", &value)
			}

			grantee = &s3api.Grantee{Type: new(granteeCanonicalUser), ID: &value, DisplayName: new("webfile")}
		case "emailAddress":
			grantee = &s3api.Grantee{Type: new(granteeEmail), EmailAddress: &value}
		default:
			return nil, invalidArgument("Argument format not recognized", entry.header, &raw)
		}

		grants = append(grants, s3api.Grant{Grantee: grantee, Permission: &entry.permission})
	}

	return grants, nil
}

// grantsFromHeaders reads every grant header the request carries.
func grantsFromHeaders(headers grantHeaders) ([]s3api.Grant, error) {
	var grants []s3api.Grant

	for _, entry := range headers.present() {
		parsed, err := parseGrantHeader(entry)
		if err != nil {
			return nil, err
		}

		grants = append(grants, parsed...)
	}

	return grants, nil
}

// validateACP is LocalStack's validate_acl_acp: an access control policy in a
// request body needs its Owner, and every grant a permission and a grantee that
// S3 knows.
func validateACP(acp *s3api.AccessControlPolicy) error {
	if acp == nil || acp.Owner == nil {
		return malformedACL()
	}

	if ownerID := deref(acp.Owner.ID); !validCanonicalID(ownerID) {
		return invalidArgument("Invalid id", "CanonicalUser/ID", &ownerID)
	}

	for _, grant := range acp.Grants {
		if !slices.Contains(validPermissions, deref(grant.Permission)) || grant.Grantee == nil {
			return malformedACL()
		}

		switch deref(grant.Grantee.Type) {
		case granteeGroup:
			if uri := deref(grant.Grantee.URI); !slices.Contains(predefinedGroups, uri) {
				return invalidArgument("Invalid group uri", "Group/URI", &uri)
			}
		case granteeCanonicalUser:
			if id := deref(grant.Grantee.ID); !validCanonicalID(id) {
				return invalidArgument("Invalid id", "CanonicalUser/ID", &id)
			}
		case granteeEmail:
		default:
			return malformedACL()
		}
	}

	return nil
}

func malformedACL() error {
	return configError("MalformedACLError", malformedXMLError().Message, http.StatusBadRequest)
}

// aclRequest is what a PutBucketAcl or PutObjectAcl carries.
type aclRequest struct {
	canned  string
	headers grantHeaders
	body    *s3api.AccessControlPolicy
}

// policy is LocalStack's get_access_control_policy_from_acl_request: one of a
// canned ACL, grant headers or a body, and no more than one.
func (r aclRequest) policy() (*s3api.AccessControlPolicy, error) {
	grantHeaderGiven := len(r.headers.present()) > 0

	switch {
	case r.canned == "" && !grantHeaderGiven && r.body == nil:
		name := "x-amz-acl"

		return nil, (&s3api.MissingSecurityHeader{MissingHeaderName: &name}).Err("Your request was missing a required header")
	case r.canned != "" && grantHeaderGiven:
		return nil, s3api.NewInvalidRequest("Specifying both Canned ACLs and Header Grants is not allowed")
	case (r.canned != "" || grantHeaderGiven) && r.body != nil:
		return nil, configError("UnexpectedContent", "This request does not support content", http.StatusBadRequest)
	}

	if r.canned != "" {
		err := validateCanned(r.canned)
		if err != nil {
			return nil, err
		}

		return cannedPolicy(r.canned), nil
	}

	if grantHeaderGiven {
		grants, err := grantsFromHeaders(r.headers)
		if err != nil {
			return nil, err
		}

		return &s3api.AccessControlPolicy{Owner: bucketOwner(), Grants: grants}, nil
	}

	err := validateACP(r.body)
	if err != nil {
		return nil, err
	}

	return r.body, nil
}

// refusedByOwnership reports whether a bucket with BucketOwnerEnforced refuses
// acp: it takes only the bucket-owner-full-control ACL or its equivalent, one
// FULL_CONTROL grant to the owner.
func (r aclRequest) refusedByOwnership(acp *s3api.AccessControlPolicy) bool {
	if r.canned != "" {
		return r.canned != cannedBucketOwnerFull
	}

	return len(acp.Grants) != 1 || deref(acp.Grants[0].Permission) != permissionFullControl ||
		acp.Grants[0].Grantee == nil || deref(acp.Grants[0].Grantee.ID) != ownerID
}

func aclNotSupported() error {
	return configError("AccessControlListNotSupported", "The bucket does not allow ACLs", http.StatusBadRequest)
}

// newResourceACL is LocalStack's get_access_control_policy_for_new_resource_request:
// the ACL of a bucket or object that CreateBucket, PutObject, CopyObject or
// CreateMultipartUpload makes, private when the request names none. It does not
// refuse ACLs for BucketOwnerEnforced: LocalStack's own tests send them to
// such buckets.
func newResourceACL(canned *string, headers grantHeaders) (*s3api.AccessControlPolicy, error) {
	grantHeaderGiven := len(headers.present()) > 0

	switch {
	case deref(canned) == "" && !grantHeaderGiven:
		return cannedPolicy(cannedPrivate), nil
	case deref(canned) != "" && grantHeaderGiven:
		return nil, s3api.NewInvalidRequest("Specifying both Canned ACLs and Header Grants is not allowed")
	case deref(canned) != "":
		err := validateCanned(*canned)
		if err != nil {
			return nil, err
		}

		return cannedPolicy(*canned), nil
	}

	grants, err := grantsFromHeaders(headers)
	if err != nil {
		return nil, err
	}

	return &s3api.AccessControlPolicy{Owner: bucketOwner(), Grants: grants}, nil
}

// newBucketConfig reads and checks the tags and the ACL a CreateBucket carries,
// before the bucket takes its name.
func newBucketConfig(in *s3api.CreateBucketInput) ([]s3api.Tag, *s3api.AccessControlPolicy, error) {
	var tags []s3api.Tag
	if in.CreateBucketConfiguration != nil {
		tags = in.CreateBucketConfiguration.Tags
	}

	err := validateTags(tags, tagsOfNewBucket)
	if err != nil {
		return nil, nil, err
	}

	acl, err := newResourceACL(in.ACL, grantHeaders{
		fullControl: in.GrantFullControl, read: in.GrantRead, readACP: in.GrantReadACP,
		write: in.GrantWrite, writeACP: in.GrantWriteACP,
	})
	if err != nil {
		return nil, nil, err
	}

	return tags, acl, nil
}

// PutBucketAcl sets the bucket's ACL.
//
//nolint:revive // var-naming: the generated Handler names the operation PutBucketAcl.
func (p *Provider) PutBucketAcl(ctx context.Context, in *s3api.PutBucketAclInput) (*s3api.PutBucketAclOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	request := aclRequest{
		canned: deref(in.ACL),
		headers: grantHeaders{
			fullControl: in.GrantFullControl, read: in.GrantRead, readACP: in.GrantReadACP,
			write: in.GrantWrite, writeACP: in.GrantWriteACP,
		},
		body: in.AccessControlPolicy,
	}

	acp, err := request.policy()
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		if b.enforcesOwner() && request.refusedByOwnership(acp) {
			return aclNotSupported()
		}

		b.ACL = acp

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketAclOutput{}, nil
}

// GetBucketAcl returns the bucket's ACL, private until one is set.
//
//nolint:revive // var-naming: the generated Handler names the operation GetBucketAcl.
func (p *Provider) GetBucketAcl(ctx context.Context, in *s3api.GetBucketAclInput) (*s3api.GetBucketAclOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketAclOutput{}

	err = b.readConfig(func() error {
		acp := aclOrDefault(b.ACL)
		out.Owner, out.Grants = acp.Owner, acp.Grants

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// itoa names a count in an error message.
func itoa(n int) string {
	return strconv.Itoa(n)
}
