package s3

import (
	"context"
	"net/http"
	"slices"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

const (
	// restoreExpiryFormat is how S3 writes the expiry-date of a restore.
	restoreExpiryFormat = "Mon, 02 Jan 2006 15:04:05 GMT"
)

// archived reports whether v is in an archive storage class and has not been
// restored, which makes its body unreadable.
func archived(v *Version) bool {
	return isArchived(v.StorageClass) && v.Restore == ""
}

// invalidObjectState is the error of an operation that needs the body of an
// archived object.
func invalidObjectState(v *Version) error {
	return (&s3api.InvalidObjectState{StorageClass: &v.StorageClass}).Err(
		"The operation is not valid for the object's storage class")
}

// RestoreObject makes an archived object readable for Days days, at once, as
// LocalStack does: it answers 202 the first time and 200 for a restore that
// extends one. A restore request without Days is a SELECT restore, which is
// accepted and does nothing.
func (p *Provider) RestoreObject(
	ctx context.Context, in *s3api.RestoreObjectInput,
) (*s3api.RestoreObjectOutput, error) {
	v, err := p.readable(ctx, in.Bucket, in.Key, in.VersionId, func(*Version) error { return nil })
	if err != nil {
		return nil, err
	}

	if !isArchived(v.StorageClass) {
		return nil, invalidObjectState(v)
	}

	var days int32
	if in.RestoreRequest != nil && in.RestoreRequest.Days != nil {
		days = *in.RestoreRequest.Days
	}

	if days == 0 {
		return &s3api.RestoreObjectOutput{}, nil
	}

	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	expiry := time.Now().UTC().Add(time.Duration(days) * hoursPerDay * time.Hour)
	status := int32(http.StatusAccepted)

	if v.Restore != "" {
		status = http.StatusOK
	}

	restored := *v
	restored.Restore = `ongoing-request="false", expiry-date="` + expiry.Format(restoreExpiryFormat) + `"`

	b.mu.Lock()
	defer b.mu.Unlock()

	object, ok := b.Objects[deref(in.Key)]
	if !ok || b.gone {
		return nil, noSuchKey(deref(in.Key))
	}

	i := slices.Index(object.Versions, v)
	if i < 0 {
		return nil, noSuchKey(deref(in.Key))
	}

	object.Versions[i] = &restored

	return &s3api.RestoreObjectOutput{StatusCode: &status}, nil
}
