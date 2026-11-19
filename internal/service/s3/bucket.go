package s3

import (
	"context"
	"encoding/base64"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kylecannon/gostack/internal/account"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	regionUSEast1 = "us-east-1"
	regionEUWest1 = "eu-west-1"
	// locationEU is the LocationConstraint alias of eu-west-1.
	locationEU = "EU"

	// ownerID is the canonical user id of every account, as LocalStack's
	// get_owner_for_account_id hard-codes it.
	ownerID = "75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a"

	sseAES256 = "AES256"

	minBucketName = 3
	maxBucketName = 63
)

var (
	// locationConstraints is the BucketLocationConstraint enum of the spec,
	// LocalStack's BUCKET_LOCATION_CONSTRAINTS. us-east-1 is not in it.
	locationConstraints = []string{
		"af-south-1", "ap-east-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-south-1",
		"ap-south-2", "ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
		"ap-southeast-5", "ca-central-1", "cn-north-1", "cn-northwest-1", locationEU, "eu-central-1",
		"eu-central-2", "eu-north-1", "eu-south-1", "eu-south-2", regionEUWest1, "eu-west-2", "eu-west-3",
		"il-central-1", "me-central-1", "me-south-1", "sa-east-1", "us-east-2", "us-gov-east-1",
		"us-gov-west-1", "us-west-1", "us-west-2",
	}

	// bucketLabels is the part of LocalStack's BUCKET_NAME_REGEX (utils.py:93)
	// that RE2 can express; validBucketName checks the length and the IP form
	// its lookaheads hold.
	bucketLabels = regexp.MustCompile(
		`^(([a-z0-9]|[a-z0-9][a-z0-9\-]*[a-z0-9])\.)*([a-z0-9]|[a-z0-9][a-z0-9\-]*[a-z0-9])$`)
	ipLike = regexp.MustCompile(`^(\d+\.)+\d+$`)

	// objectOwnerships are the values x-amz-object-ownership takes.
	objectOwnerships = []string{"BucketOwnerPreferred", "ObjectWriter", "BucketOwnerEnforced"}
)

// CreateBucket creates a bucket in the region of its LocationConstraint, as
// LocalStack's create_bucket does. A second CreateBucket of a name the caller
// owns in us-east-1 succeeds again; anywhere else it fails.
func (p *Provider) CreateBucket(ctx context.Context, in *s3api.CreateBucketInput) (*s3api.CreateBucketOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.Bucket)
	if !validBucketName(name) {
		return nil, (&s3api.InvalidBucketName{BucketName: &name}).Err("The specified bucket is not valid.")
	}

	var constraint string
	if in.CreateBucketConfiguration != nil {
		constraint = deref(in.CreateBucketConfiguration.LocationConstraint)
	}

	err = validateLocationConstraint(rc.Region, constraint)
	if err != nil {
		return nil, err
	}

	if ownership := in.ObjectOwnership; ownership != nil && !slices.Contains(objectOwnerships, *ownership) {
		return nil, invalidArgument("Invalid x-amz-object-ownership header: "+*ownership,
			"x-amz-object-ownership", nil)
	}

	tags, acl, err := newBucketConfig(in)
	if err != nil {
		return nil, err
	}

	region := bucketRegion(constraint)

	holder, claimed := p.names.claim(name, rc.AccountID)
	if !claimed {
		return p.existingBucket(rc.AccountID, holder, name, region, tags)
	}

	b := newBucket(name, rc.AccountID, region, constraint, in.ObjectOwnership)
	b.Tags, b.ACL = tags, acl

	if in.ObjectLockEnabledForBucket != nil && *in.ObjectLockEnabledForBucket {
		b.enableObjectLock()
	}

	s := p.state(rc.AccountID)
	s.mu.Lock()
	s.buckets[name] = b
	s.mu.Unlock()

	location := "/" + name
	if region != regionUSEast1 {
		location = p.bucketURL(name)
	}

	return &s3api.CreateBucketOutput{Location: &location, BucketArn: new(bucketARN(name, region))}, nil
}

// HeadBucket answers whether the bucket exists, with its region and ARN.
func (p *Provider) HeadBucket(ctx context.Context, in *s3api.HeadBucketInput) (*s3api.HeadBucketOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	// A DeleteBucket that ran after the lookup has retired the bucket.
	err = b.readConfig(func() error { return nil })
	if err != nil {
		return nil, err
	}

	return &s3api.HeadBucketOutput{BucketRegion: &b.Region, BucketArn: new(bucketARN(b.Name, b.Region))}, nil
}

// GetBucketLocation returns the bucket's LocationConstraint, empty for
// us-east-1. The body is the LocationConstraint element itself, which the
// patched spec makes the payload (get_bucket_location_xml).
func (p *Provider) GetBucketLocation(
	ctx context.Context, in *s3api.GetBucketLocationInput,
) (*s3api.GetBucketLocationOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	err = b.readConfig(func() error { return nil })
	if err != nil {
		return nil, err
	}

	body := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`
	if b.LocationConstraint == "" {
		body += "/>"
	} else {
		body += ">" + b.LocationConstraint + "</LocationConstraint>"
	}

	return &s3api.GetBucketLocationOutput{LocationConstraint: &body}, nil
}

// DeleteBucket deletes an empty bucket.
func (p *Provider) DeleteBucket(ctx context.Context, in *s3api.DeleteBucketInput) (*s3api.DeleteBucketOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	name := deref(in.Bucket)

	b, err := p.bucket(rc.AccountID, name)
	if err != nil {
		return nil, err
	}

	s := p.state(b.Account)
	s.mu.Lock()
	b.mu.Lock()

	if b.gone || len(b.Objects) > 0 {
		gone, versioning := b.gone, b.Versioning

		b.mu.Unlock()
		s.mu.Unlock()

		if gone {
			return nil, noSuchBucket(name)
		}

		return nil, bucketNotEmpty(name, versioning)
	}

	delete(s.buckets, name)
	b.gone = true
	b.mu.Unlock()
	s.mu.Unlock()

	p.names.release(name, b.Account)
	p.retire(b)

	return &s3api.DeleteBucketOutput{}, nil
}

// ListBuckets lists the caller's buckets in name order, filtered by prefix and
// region and paged by MaxBuckets, as LocalStack's list_buckets does. The
// continuation token is the base64 of the first name of the next page.
func (p *Provider) ListBuckets(ctx context.Context, in *s3api.ListBucketsInput) (*s3api.ListBucketsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	region := deref(in.BucketRegion)
	if region != "" && !account.KnownRegion(region) {
		return nil, invalidArgument("Argument value "+region+" is not a valid AWS Region", "bucket-region", nil)
	}

	var after string

	if token := deref(in.ContinuationToken); token != "" {
		decoded, decodeErr := base64.URLEncoding.DecodeString(token)
		if decodeErr != nil {
			return nil, invalidArgument("The continuation token provided is incorrect", "continuation-token", nil)
		}

		after = string(decoded)
	}

	prefix := deref(in.Prefix)
	limit := 0

	if in.MaxBuckets != nil {
		limit = int(*in.MaxBuckets)
	}

	out := &s3api.ListBucketsOutput{Owner: &s3api.Owner{ID: new(ownerID)}, Prefix: in.Prefix, Buckets: []s3api.Bucket{}}

	for _, b := range p.state(rc.AccountID).sorted() {
		if b.Name < after || !strings.HasPrefix(b.Name, prefix) || (region != "" && b.Region != region) {
			continue
		}

		if limit > 0 && len(out.Buckets) >= limit {
			out.ContinuationToken = new(base64.URLEncoding.EncodeToString([]byte(b.Name)))

			break
		}

		out.Buckets = append(out.Buckets, s3api.Bucket{
			Name:         new(b.Name),
			CreationDate: &protocol.Time{Time: b.CreationDate.Truncate(time.Second)},
			BucketRegion: new(b.Region),
			BucketArn:    new(bucketARN(b.Name, b.Region)),
		})
	}

	return out, nil
}

// existingBucket answers a CreateBucket of a name that is taken.
func (p *Provider) existingBucket(
	caller, holder, name, region string, tags []s3api.Tag,
) (*s3api.CreateBucketOutput, error) {
	if holder != caller {
		return nil, s3api.NewBucketAlreadyExists(
			"The requested bucket name is not available. The bucket namespace is shared by all users of the " +
				"system. Please select a different name and try again.")
	}

	existing := p.state(caller).get(name)
	if region != regionUSEast1 || len(tags) > 0 || existing == nil {
		return nil, (&s3api.BucketAlreadyOwnedByYou{BucketName: &name}).Err(
			"Your previous request to create the named bucket succeeded and you already own it.")
	}

	return &s3api.CreateBucketOutput{
		Location:  new("/" + name),
		BucketArn: new(bucketARN(name, existing.Region)),
	}, nil
}

// bucketURL is get_full_default_bucket_location: the virtual host on
// LOCALSTACK_HOST, or the path on it when the host is not LocalStack's own
// domain, which may not resolve subdomains.
func (p *Provider) bucketURL(name string) string {
	settings := p.settings()
	if settings.hostname() != "localhost.localstack.cloud" {
		return "http://" + settings.LocalstackHost + "/" + name + "/"
	}

	return "http://" + name + ".s3." + settings.LocalstackHost + "/"
}

func newBucket(name, owner, region, constraint string, ownership *string) *Bucket {
	objectOwnership := "BucketOwnerEnforced"
	if ownership != nil {
		objectOwnership = *ownership
	}

	return &Bucket{
		Name:               name,
		Account:            owner,
		Region:             region,
		LocationConstraint: constraint,
		CreationDate:       time.Now().UTC(),
		Objects:            make(map[string]*Object),
		Uploads:            make(map[string]*MultipartUpload),
		// The defaults every new bucket gets: SSE-S3 encryption, a public
		// access block that blocks everything, and ACLs disabled.
		Encryption: &s3api.ServerSideEncryptionConfiguration{Rules: []s3api.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &s3api.ServerSideEncryptionByDefault{SSEAlgorithm: new(sseAES256)},
			BucketKeyEnabled:                   new(false),
		}}},
		PublicAccessBlock: &s3api.PublicAccessBlockConfiguration{
			BlockPublicAcls: new(true), BlockPublicPolicy: new(true),
			IgnorePublicAcls: new(true), RestrictPublicBuckets: new(true),
		},
		OwnershipControls: &s3api.OwnershipControls{Rules: []s3api.OwnershipControlsRule{{
			ObjectOwnership: &objectOwnership,
		}}},
	}
}

// bucketNotEmpty words the error as S3 does for a bucket that has had
// versioning, whose versions must go too.
func bucketNotEmpty(name string, versioning *s3api.VersioningConfiguration) error {
	message := "The bucket you tried to delete is not empty"
	if versioning != nil && deref(versioning.Status) != "" {
		message += ". You must delete all versions in the bucket."
	}

	return (&s3api.BucketNotEmpty{BucketName: &name}).Err(message)
}

// validateLocationConstraint is LocalStack's validate_location_constraint: in
// us-east-1 the constraint must be one of the enum, which us-east-1 is not; in
// eu-west-1 it must be EU or eu-west-1; anywhere else it must be the region,
// and an absent one is wrong outside us-east-1.
func validateLocationConstraint(region, constraint string) error {
	switch {
	case constraint == "" && region != regionUSEast1:
		return illegalLocationConstraint("unspecified")
	case constraint == "":
		return nil
	case region == regionUSEast1:
		if !slices.Contains(locationConstraints, constraint) {
			return (&s3api.InvalidLocationConstraint{LocationConstraint: &constraint}).Err(
				"The specified location-constraint is not valid")
		}

		return nil
	case region == regionEUWest1:
		if constraint != locationEU && constraint != regionEUWest1 {
			return illegalLocationConstraint(constraint)
		}

		return nil
	case constraint != region:
		return illegalLocationConstraint(constraint)
	default:
		return nil
	}
}

// illegalLocationConstraint is an error the spec does not model.
func illegalLocationConstraint(constraint string) *awserr.Error {
	return &awserr.Error{
		Code: "IllegalLocationConstraintException",
		Message: "The " + constraint +
			" location constraint is incompatible for the region specific endpoint this request was sent to.",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// bucketRegion is the region a LocationConstraint puts a bucket in.
func bucketRegion(constraint string) string {
	switch constraint {
	case "":
		return regionUSEast1
	case locationEU:
		return regionEUWest1
	default:
		return constraint
	}
}

// validBucketName is LocalStack's is_bucket_name_valid.
func validBucketName(name string) bool {
	return len(name) >= minBucketName && len(name) <= maxBucketName && !ipLike.MatchString(name) &&
		bucketLabels.MatchString(name)
}

// bucketARN is the bucket's ARN in the partition of its region.
func bucketARN(name, region string) string {
	return "arn:" + partitionOf(region) + ":s3:::" + name
}
