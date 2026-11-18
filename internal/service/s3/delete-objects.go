package s3

import (
	"context"
	"errors"
	"net/http"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

// maxDeleteObjects is how many keys one DeleteObjects takes.
const maxDeleteObjects = 1000

// DeleteObjects removes up to 1000 keys, or versions of keys, in one call. A
// bucket with versioning gets a delete marker for a key named without a version.
// It answers each key it removed, or none in quiet mode, and each key it could
// not remove as an error.
func (p *Provider) DeleteObjects(
	ctx context.Context, in *s3api.DeleteObjectsInput,
) (*s3api.DeleteObjectsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	err = checkContentMD5(rc)
	if err != nil {
		return nil, err
	}

	if in.BypassGovernanceRetention != nil && !objectLockOn(b) {
		return nil, invalidArgument(
			"x-amz-bypass-governance-retention is only applicable to Object Lock enabled buckets.",
			"x-amz-bypass-governance-retention", nil)
	}

	if in.Delete == nil || len(in.Delete.Objects) == 0 || len(in.Delete.Objects) > maxDeleteObjects {
		return nil, &awserr.Error{
			Code:       "MalformedXML",
			Message:    "The XML you provided was not well-formed or did not validate against our published schema",
			StatusCode: http.StatusBadRequest,
			Fault:      awserr.FaultReceiver,
		}
	}

	quiet := in.Delete.Quiet != nil && *in.Delete.Quiet
	bypass := in.BypassGovernanceRetention != nil && *in.BypassGovernanceRetention
	out := &s3api.DeleteObjectsOutput{}

	for _, object := range in.Delete.Objects {
		deleted, failed, err := p.deleteEntry(b, object, bypass)
		if err != nil {
			return nil, err
		}

		switch {
		case failed != nil:
			out.Errors = append(out.Errors, *failed)
		case !quiet:
			out.Deleted = append(out.Deleted, deleted)
		default:
		}
	}

	return out, nil
}

// deleteEntry deletes one key of a DeleteObjects. It answers what was deleted,
// or the error entry of a version that is not there or that Object Lock holds;
// the third result is the error that fails the whole call.
func (p *Provider) deleteEntry(
	b *Bucket, object s3api.ObjectIdentifier, bypass bool,
) (s3api.DeletedObject, *s3api.Error, error) {
	key, id := deref(object.Key), deref(object.VersionId)

	removed, err := p.deleteFrom(b, key, id, bypass)

	switch {
	case errors.Is(err, errObjectLocked):
		return s3api.DeletedObject{}, &s3api.Error{
			Code: new("AccessDenied"), Key: &key, Message: new(lockedObjectMessage), VersionId: &id,
		}, nil
	case errors.Is(err, errNoSuchVersion) || removed.absent:
		return s3api.DeletedObject{}, &s3api.Error{
			Code: new("NoSuchVersion"), Key: &key, Message: new("The specified version does not exist."),
			VersionId: &id,
		}, nil
	case err != nil:
		return s3api.DeletedObject{}, nil, err
	}

	deleted := s3api.DeletedObject{Key: &key}

	switch {
	case removed.added:
		deleted.DeleteMarker, deleted.DeleteMarkerVersionId = new(true), &removed.versionID
	case removed.versionID != "":
		deleted.VersionId = &removed.versionID

		if removed.marker {
			deleted.DeleteMarker, deleted.DeleteMarkerVersionId = new(true), &removed.versionID
		}
	default:
	}

	return deleted, nil, nil
}

// checkContentMD5 refuses a Content-MD5 that is not the base64 of 16 bytes. AWS
// also refuses a DeleteObjects that carries neither Content-MD5 nor a checksum
// header; gostack accepts it, because LocalStack does and
// test_s3_delete_objects_trailing_slash, which LocalStack passes, posts one
// with neither. The digest is not compared with the body: the generated decoder
// has consumed it by now.
func checkContentMD5(rc *request.Context) error {
	md5Sum := rc.Request.Header.Get("Content-MD5")
	if md5Sum != "" && !validContentMD5(md5Sum) {
		return invalidDigest(md5Sum)
	}

	return nil
}

func invalidDigest(value string) error {
	return (&s3api.InvalidDigest{Content_MD5: &value}).Err("The Content-MD5 you specified was invalid.")
}
