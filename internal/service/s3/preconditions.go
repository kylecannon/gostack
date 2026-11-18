package s3

import (
	"net/http"
	"strings"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	conditionIfMatch           = "If-Match"
	conditionIfNoneMatch       = "If-None-Match"
	conditionIfUnmodifiedSince = "If-Unmodified-Since"

	copySourcePrefix = "x-amz-copy-source-"

	preconditionMessage = "At least one of the pre-conditions you specified did not hold"
)

// readConditions are the conditional headers of a GET or HEAD.
type readConditions struct {
	ifMatch           *string
	ifNoneMatch       *string
	ifModifiedSince   *protocol.Time
	ifUnmodifiedSince *protocol.Time
}

// check answers 412 when If-Match or If-Unmodified-Since does not hold, and 304
// when If-None-Match matches or the object is not newer than If-Modified-Since,
// in the order validate_failed_precondition tests them. A date in the future is
// ignored, as S3 does.
func (c readConditions) check(v *Version) error {
	modified := v.LastModified.Truncate(time.Second)
	hasIfMatch := deref(c.ifMatch) != ""

	// An If-Match that holds overrides a failed If-Unmodified-Since, as the
	// GetObject and HeadObject documentation says: the answer is 200. LocalStack
	// answers 412 (utils.py:1017-1023); this follows AWS.
	switch {
	case hasIfMatch && !etagMatches(*c.ifMatch, v.ETag):
		return preconditionFailed(conditionIfMatch)
	case !hasIfMatch && c.ifUnmodifiedSince != nil && modified.After(c.ifUnmodifiedSince.Truncate(time.Second)):
		return preconditionFailed(conditionIfUnmodifiedSince)
	}

	noneMatch := deref(c.ifNoneMatch) != "" && etagMatches(*c.ifNoneMatch, v.ETag)

	if noneMatch || modifiedSinceHolds(c.ifModifiedSince, modified) {
		return &awserr.Error{
			Code: "NotModified", Message: "Not Modified", StatusCode: http.StatusNotModified, Fault: awserr.FaultReceiver,
		}
	}

	return nil
}

// modifiedSinceHolds reports whether a "modified since" date is one the object
// has not outgrown: not in the future, and not before the object's change.
func modifiedSinceHolds(since *protocol.Time, modified time.Time) bool {
	if since == nil {
		return false
	}

	date := since.Truncate(time.Second)

	return !modified.After(date) && date.Before(time.Now())
}

// copySourceCondition returns the name of the copy-source condition the source
// version fails, or "" when it passes (get_failed_precondition_copy_source).
func copySourceCondition(in *s3api.CopyObjectInput, v *Version) string {
	modified := v.LastModified.Truncate(time.Second)
	hasIfMatch := deref(in.CopySourceIfMatch) != ""

	// As for a read, a copy-source If-Match that holds overrides a failed
	// copy-source If-Unmodified-Since, per the CopyObject documentation.
	switch {
	case hasIfMatch && strings.Trim(*in.CopySourceIfMatch, `"`) != v.ETag:
		return copySourcePrefix + conditionIfMatch
	case !hasIfMatch && in.CopySourceIfUnmodifiedSince != nil &&
		modified.After(in.CopySourceIfUnmodifiedSince.Truncate(time.Second)):
		return copySourcePrefix + conditionIfUnmodifiedSince
	case deref(in.CopySourceIfNoneMatch) != "" && strings.Trim(*in.CopySourceIfNoneMatch, `"`) == v.ETag:
		return copySourcePrefix + conditionIfNoneMatch
	case modifiedSinceHolds(in.CopySourceIfModifiedSince, modified):
		return copySourcePrefix + "If-Modified-Since"
	}

	return ""
}

// etagMatches reports whether an If-Match or If-None-Match header names the
// ETag: any of its comma-separated entities, with or without quotes or a weak
// marker, or "*" for any object.
func etagMatches(header, etag string) bool {
	for entity := range strings.SplitSeq(header, ",") {
		entity = strings.TrimSpace(entity)
		entity = strings.TrimPrefix(entity, "W/")

		if entity == "*" || strings.Trim(entity, `"`) == etag {
			return true
		}
	}

	return false
}

// writeConditions are the conditional headers of a PutObject or CopyObject
// destination: S3 accepts If-None-Match: * and an If-Match ETag.
type writeConditions struct {
	ifMatch     string
	ifNoneMatch bool
}

func (c writeConditions) any() bool {
	return c.ifMatch != "" || c.ifNoneMatch
}

// parseWriteConditions refuses what S3 does not implement for a write: both
// headers together, an If-None-Match other than "*", and an If-Match of "*".
func parseWriteConditions(ifMatch, ifNoneMatch *string) (writeConditions, error) {
	match, noneMatch := deref(ifMatch), deref(ifNoneMatch)

	switch {
	case match != "" && noneMatch != "":
		return writeConditions{}, notImplementedHeader("If-Match,If-None-Match",
			"Multiple conditional request headers present in the request")
	case noneMatch != "" && noneMatch != "*":
		return writeConditions{}, notImplementedHeader(conditionIfNoneMatch,
			"We don't accept the provided value of If-None-Match header for this API")
	case match == "*":
		return writeConditions{}, notImplementedHeader(conditionIfMatch,
			"We don't accept the provided value of If-Match header for this API")
	}

	return writeConditions{ifMatch: match, ifNoneMatch: noneMatch != ""}, nil
}

// check holds a key's current object to the conditions: If-None-Match needs
// there to be none, If-Match needs one with that ETag. A delete marker counts
// as none.
func (c writeConditions) check(object *Object, key string) error {
	var current *Version

	if object != nil {
		current = object.current()
	}

	exists := current != nil && !current.DeleteMarker

	if c.ifNoneMatch && exists {
		return preconditionFailed(conditionIfNoneMatch)
	}

	if c.ifMatch == "" {
		return nil
	}

	if !exists {
		return noSuchKey(key)
	}

	if strings.Trim(c.ifMatch, `"`) != current.ETag {
		return preconditionFailed(conditionIfMatch)
	}

	return nil
}

func preconditionFailed(condition string) error {
	return (&s3api.PreconditionFailed{Condition: &condition}).Err(preconditionMessage)
}

func notImplementedHeader(header, additional string) error {
	return (&s3api.NotImplemented{Header: &header, AdditionalMessage: nonEmpty(additional)}).Err(
		"A header you provided implies functionality that is not implemented")
}
