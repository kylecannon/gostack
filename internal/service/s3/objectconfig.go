package s3

import (
	"context"
	"net/http"
	"slices"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

const resourceDeleteMarker = "DeleteMarker"

// versioned reports whether the bucket has ever had versioning set, which is
// when its objects report version ids. The caller holds b.mu.
func (b *Bucket) versioned() bool {
	return b.Versioning != nil && deref(b.Versioning.Status) != ""
}

// versionTarget finds the version a tagging or ACL operation addresses, as
// LocalStack's S3Bucket.get_object does: outside a versioned bucket only the
// null version exists; inside one a version id names a version and a delete
// marker answers 405 for any method that is not GET or HEAD. method is the
// HTTP method of the call. The caller holds b.mu.
func (b *Bucket) versionTarget(key, versionID, method string) (*Object, int, error) {
	object := b.Objects[key]

	if !b.versioned() {
		if versionID != "" && versionID != versionNull {
			return nil, 0, invalidArgument("Invalid version id specified", "versionId", &versionID)
		}

		if object == nil || len(object.Versions) == 0 {
			return nil, 0, noSuchKey(key)
		}

		return object, len(object.Versions) - 1, nil
	}

	if versionID != "" {
		if object == nil {
			return nil, 0, noSuchVersion(key, versionID)
		}

		index := slices.IndexFunc(object.Versions, func(v *Version) bool { return v.ID == versionID })
		if index < 0 {
			return nil, 0, noSuchVersion(key, versionID)
		}

		if object.Versions[index].DeleteMarker {
			return nil, 0, methodNotAllowed(method, versionID)
		}

		return object, index, nil
	}

	if object == nil || len(object.Versions) == 0 {
		return nil, 0, noSuchKey(key)
	}

	index := len(object.Versions) - 1
	if marker := object.Versions[index]; marker.DeleteMarker {
		if method != http.MethodGet && method != http.MethodHead {
			return nil, 0, methodNotAllowed(method, marker.ID)
		}

		return nil, 0, (&s3api.NoSuchKey{Key: &key, DeleteMarker: new(true), VersionId: &marker.ID}).Err(
			"The specified key does not exist.")
	}

	return object, index, nil
}

func noSuchVersion(key, versionID string) error {
	return (&s3api.NoSuchVersion{Key: &key, VersionId: &versionID}).Err("The specified version does not exist.")
}

func methodNotAllowed(method, versionID string) error {
	return (&s3api.MethodNotAllowed{
		Method: &method, ResourceType: new(resourceDeleteMarker), DeleteMarker: new(true),
		Allow: new(http.MethodDelete), VersionId: &versionID,
	}).Err("The specified method is not allowed against this resource.")
}

// reportedVersionID is the version id a tagging or ACL reply names: none for a
// bucket that never had versioning.
func (b *Bucket) reportedVersionID(v *Version) *string {
	if !b.versioned() {
		return nil
	}

	return nonEmpty(v.ID)
}

// replaceVersion swaps the version at index for a copy that change has edited.
// A Version never changes once it is committed, because readers use it after
// the bucket lock is gone. The caller holds b.mu for writing.
func replaceVersion(object *Object, index int, change func(*Version)) *Version {
	updated := *object.Versions[index]
	change(&updated)
	object.Versions[index] = &updated

	return &updated
}

// GetObjectTagging returns the tags of an object version.
func (p *Provider) GetObjectTagging(
	ctx context.Context, in *s3api.GetObjectTaggingInput,
) (*s3api.GetObjectTaggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	key := deref(in.Key)
	out := &s3api.GetObjectTaggingOutput{}

	err = b.readConfig(func() error {
		object, index, lookupErr := b.versionTarget(key, deref(in.VersionId), http.MethodGet)
		if lookupErr != nil {
			return b.taggingLookupError(lookupErr, key, deref(in.VersionId))
		}

		v := object.Versions[index]
		out.TagSet = append([]s3api.Tag{}, v.Tags...)
		out.VersionId = b.reportedVersionID(v)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// taggingLookupError words what GetObjectTagging answers when it finds no
// version, which S3 does differently from every other read: a delete marker is
// 405, and a missing key is named with the bucket in front of it.
func (b *Bucket) taggingLookupError(err error, key, versionID string) error {
	object := b.Objects[key]
	present := object != nil && len(object.Versions) > 0
	current := versionID == "" || (!b.versioned() && versionID == versionNull)

	switch {
	case present && b.versioned() && versionID == "" && object.Versions[len(object.Versions)-1].DeleteMarker:
		return methodNotAllowed(http.MethodGet, object.Versions[len(object.Versions)-1].ID)
	case !present && current:
		return noSuchKey(b.Name + "/" + key)
	default:
		return err
	}
}

// PutObjectTagging replaces the tags of an object version.
func (p *Provider) PutObjectTagging(
	ctx context.Context, in *s3api.PutObjectTaggingInput,
) (*s3api.PutObjectTaggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.PutObjectTaggingOutput{}

	err = b.writeConfig(func() error {
		object, index, lookupErr := b.versionTarget(deref(in.Key), deref(in.VersionId), http.MethodPut)
		if lookupErr != nil {
			return lookupErr
		}

		if in.Tagging == nil {
			return malformedXMLError()
		}

		validateErr := validateTags(in.Tagging.TagSet, tagsOfObject)
		if validateErr != nil {
			return validateErr
		}

		updated := replaceVersion(object, index, func(v *Version) { v.Tags = in.Tagging.TagSet })
		out.VersionId = b.reportedVersionID(updated)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteObjectTagging removes the tags of an object version.
func (p *Provider) DeleteObjectTagging(
	ctx context.Context, in *s3api.DeleteObjectTaggingInput,
) (*s3api.DeleteObjectTaggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.DeleteObjectTaggingOutput{}

	err = b.writeConfig(func() error {
		object, index, lookupErr := b.versionTarget(deref(in.Key), deref(in.VersionId), http.MethodDelete)
		if lookupErr != nil {
			return lookupErr
		}

		updated := replaceVersion(object, index, func(v *Version) { v.Tags = nil })
		out.VersionId = b.reportedVersionID(updated)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// GetObjectAcl returns the ACL of an object version, private until one is set.
//
//nolint:revive // var-naming: the generated Handler names the operation GetObjectAcl.
func (p *Provider) GetObjectAcl(ctx context.Context, in *s3api.GetObjectAclInput) (*s3api.GetObjectAclOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetObjectAclOutput{}

	err = b.readConfig(func() error {
		object, index, lookupErr := b.versionTarget(deref(in.Key), deref(in.VersionId), http.MethodGet)
		if lookupErr != nil {
			return lookupErr
		}

		acp := aclOrDefault(object.Versions[index].ACL)
		out.Owner, out.Grants = acp.Owner, acp.Grants

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutObjectAcl sets the ACL of an object version.
//
//nolint:revive // var-naming: the generated Handler names the operation PutObjectAcl.
func (p *Provider) PutObjectAcl(ctx context.Context, in *s3api.PutObjectAclInput) (*s3api.PutObjectAclOutput, error) {
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

	err = b.writeConfig(func() error {
		object, index, lookupErr := b.versionTarget(deref(in.Key), deref(in.VersionId), http.MethodPut)
		if lookupErr != nil {
			return lookupErr
		}

		acp, policyErr := request.policy()
		if policyErr != nil {
			return policyErr
		}

		if b.enforcesOwner() && request.refusedByOwnership(acp) {
			return aclNotSupported()
		}

		replaceVersion(object, index, func(v *Version) { v.ACL = acp })

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutObjectAclOutput{}, nil
}

// objectConfig is what a request that writes an object says about its ACL and
// its tags: the x-amz-acl and x-amz-grant-* headers and x-amz-tagging.
type objectConfig struct {
	acl  *s3api.AccessControlPolicy
	tags []s3api.Tag
}

// newObjectConfig reads and checks the ACL and tag headers of a PutObject,
// CopyObject or CreateMultipartUpload before anything is written.
func newObjectConfig(canned *string, headers grantHeaders, tagging *string) (objectConfig, error) {
	tags, err := parseTaggingHeader(deref(tagging))
	if err != nil {
		return objectConfig{}, err
	}

	acl, err := newResourceACL(canned, headers)
	if err != nil {
		return objectConfig{}, err
	}

	return objectConfig{acl: acl, tags: tags}, nil
}

// putObjectConfig is the objectConfig of a PutObject.
func putObjectConfig(in *s3api.PutObjectInput) (objectConfig, error) {
	return newObjectConfig(in.ACL, grantHeaders{
		fullControl: in.GrantFullControl, read: in.GrantRead, readACP: in.GrantReadACP, writeACP: in.GrantWriteACP,
	}, in.Tagging)
}
