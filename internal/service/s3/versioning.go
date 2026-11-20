package s3

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	versioningSuspended = "Suspended"
	mfaDeleteDisabled   = "Disabled"

	// versionIDSequenceBytes is the leading, ever-increasing part of a version
	// id, and versionIDRandomBytes the random part after it: together 24 bytes,
	// 32 characters of base64 with no padding, as generate_safe_version_id makes
	// them.
	versionIDSequenceBytes = 6
	versionIDRandomBytes   = 18
)

var (
	// errNoSuchVersion and errObjectLocked are what deleteFrom answers for a
	// version that is not there and one that Object Lock holds. DeleteObject
	// and DeleteObjects word them differently.
	errNoSuchVersion = errors.New("no such version")
	errObjectLocked  = errors.New("object protected by object lock")

	// versionSequence is the counter the leading part of a version id comes
	// from. It starts at the time in milliseconds, as LocalStack's does, so ids
	// made after a restart still sort after the ones made before it.
	versionSequence = newVersionSequence()
)

func newVersionSequence() *atomic.Uint64 {
	sequence := new(atomic.Uint64)
	sequence.Store(uint64(time.Now().UnixMilli()))

	return sequence
}

// generateVersionID makes the id of a version in a bucket whose versioning is
// Enabled. The ids sort by the order they were made in, which ListObjectVersions
// relies on to resume after a version that has since been deleted.
func generateVersionID() string {
	var raw [versionIDSequenceBytes + versionIDRandomBytes]byte

	var sequence [8]byte

	binary.BigEndian.PutUint64(sequence[:], versionSequence.Add(1))
	copy(raw[:versionIDSequenceBytes], sequence[8-versionIDSequenceBytes:])

	// crypto/rand.Read never fails: a failure to read the system's randomness
	// stops the process.
	_, _ = rand.Read(raw[versionIDSequenceBytes:])

	return versionIDEncoding.EncodeToString(raw[:])
}

// PutBucketVersioning turns versioning on or suspends it. A bucket that has
// had versioning never goes back to having none, and one with Object Lock
// cannot be suspended. MFADelete is stored and reported; nothing checks a
// token.
func (p *Provider) PutBucketVersioning(
	ctx context.Context, in *s3api.PutBucketVersioningInput,
) (*s3api.PutBucketVersioningOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error { return b.setVersioning(in.VersioningConfiguration) })
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketVersioningOutput{}, nil
}

// setVersioning is PutBucketVersioning's change to the bucket. The caller
// holds b.mu for writing.
func (b *Bucket) setVersioning(config *s3api.VersioningConfiguration) error {
	var status, mfaDelete string

	if config != nil {
		status, mfaDelete = deref(config.Status), deref(config.MFADelete)
	}

	switch {
	case status == "":
		return configError("IllegalVersioningConfigurationException",
			"The Versioning element must be specified", http.StatusBadRequest)
	case status != versioningEnabled && status != versioningSuspended,
		mfaDelete != "" && mfaDelete != versioningEnabled && mfaDelete != mfaDeleteDisabled:
		return malformedXMLError()
	case status == versioningSuspended && b.lockEnabled():
		return invalidBucketState(
			"An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.")
	}

	if mfaDelete == "" && b.Versioning != nil {
		mfaDelete = deref(b.Versioning.MFADelete)
	}

	b.Versioning = &s3api.VersioningConfiguration{Status: &status, MFADelete: nonEmpty(mfaDelete)}

	return nil
}

// GetBucketVersioning reports the versioning status, which is absent for a
// bucket that never had any.
func (p *Provider) GetBucketVersioning(
	ctx context.Context, in *s3api.GetBucketVersioningInput,
) (*s3api.GetBucketVersioningOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketVersioningOutput{}

	err = b.readConfig(func() error {
		if b.Versioning != nil {
			out.Status, out.MFADelete = b.Versioning.Status, b.Versioning.MFADelete
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// newVersionID is the id the next version of the bucket gets: a fresh one
// while versioning is Enabled, versionNull while it is Suspended or never was
// on. The caller holds b.mu.
func (b *Bucket) newVersionID() string {
	if b.Versioning != nil && deref(b.Versioning.Status) == versioningEnabled {
		return generateVersionID()
	}

	return versionNull
}

// addVersion makes v the current version of key and gives it its id. A null
// version, which is all a bucket without versioning has, replaces the one the
// key already has, wherever it lies in the chain; any other version is added on
// top. It returns the versions that were replaced, whose bodies the caller
// removes once it has let go of b.mu. The caller holds b.mu for writing.
func (b *Bucket) addVersion(key string, v *Version) []*Version {
	object := b.Objects[key]
	if object == nil {
		object = &Object{}
		b.putObject(key, object)
	}

	v.ID = b.newVersionID()

	var replaced []*Version

	if v.ID == versionNull {
		object.Versions = slices.DeleteFunc(object.Versions, func(old *Version) bool {
			if old.ID != versionNull {
				return false
			}

			replaced = append(replaced, old)

			return true
		})
	}

	object.Versions = append(object.Versions, v)

	return replaced
}

// versionHeader is the x-amz-version-id a reply names for v: none for a bucket
// that never had versioning, versionNull for a version made while it was
// suspended or before it was on.
func (b *Bucket) versionHeader(v *Version) *string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.reportedVersionID(v)
}

// sourceVersionHeader is the x-amz-copy-source-version-id of a copy: the id of
// the version it read, when that has one.
func sourceVersionHeader(source *Version) *string {
	if source.ID == versionNull {
		return nil
	}

	return &source.ID
}

// copySourceError is the answer to a copy whose source cannot be read. Naming
// a delete marker by its version id is an InvalidRequest to a copy, where it is
// a method that is not allowed to a GET.
func copySourceError(err error) error {
	var aws *awserr.Error
	if errors.As(err, &aws) && aws.Code == "MethodNotAllowed" {
		return s3api.NewInvalidRequest(
			"The source of a copy request may not specifically refer to a delete marker by version id.")
	}

	return err
}

// versionHeadersOf is the version and Object Lock headers a GET or HEAD of v
// carries. A bucket that has gone has none.
func (p *Provider) versionHeadersOf(ctx context.Context, bucket *string, v *Version) objectVersionHeaders {
	headers := v.Lock.headers()

	rc, err := callOf(ctx)
	if err != nil {
		return headers
	}

	b, err := p.bucket(rc.AccountID, deref(bucket))
	if err != nil {
		return headers
	}

	headers.versionID = b.versionHeader(v)

	return headers
}

// deletion is what removing a key, or one version of it, did.
type deletion struct {
	// versionID is the version that was removed, or the delete marker that was
	// added; "" for a bucket that never had versioning.
	versionID string
	// marker says versionID names a delete marker.
	marker bool
	// added says the delete marker was made by this delete, as a delete of a key
	// with no version id does in a bucket with versioning.
	added bool
	// absent says the bucket has versioning, the delete named a version, and
	// the key has no versions at all.
	absent bool
	// changed says the delete took something away or added a marker. A
	// DeleteObject that changed nothing raises no event (SQ-113).
	changed bool
}

// deleteFrom removes the key, or the version of it that versionID names, from
// the bucket. Without a version id a bucket with versioning gets a delete
// marker instead; one without it loses the key. A version that Object Lock holds
// stays, unless bypass lifts a GOVERNANCE hold. It answers errNoSuchVersion for
// a version the key does not have and errObjectLocked for one that is held.
func (p *Provider) deleteFrom(b *Bucket, key, versionID string, bypass bool) (deletion, error) {
	p.transfer.RLock()
	defer p.transfer.RUnlock()

	b.mu.Lock()

	if b.gone {
		b.mu.Unlock()

		return deletion{}, noSuchBucket(b.Name)
	}

	result, removed, err := b.deleteLocked(key, versionID, bypass, time.Now())
	b.mu.Unlock()

	result.changed = err == nil && (len(removed) > 0 || result.added)

	for _, v := range removed {
		if v.BlobID != "" {
			_ = p.blobs.Remove(v.BlobID)
		}
	}

	return result, err
}

// deleteLocked is deleteFrom under the bucket's lock. It returns the versions
// that left the bucket, whose bodies the caller removes.
func (b *Bucket) deleteLocked(key, versionID string, bypass bool, now time.Time) (deletion, []*Version, error) {
	if !b.versioned() {
		if versionID != "" && versionID != versionNull {
			return deletion{}, nil, errNoSuchVersion
		}

		object := b.Objects[key]
		b.dropObject(key)

		if object == nil {
			return deletion{}, nil, nil
		}

		return deletion{}, object.Versions, nil
	}

	if versionID == "" {
		marker := &Version{DeleteMarker: true, LastModified: protocol.SnapshotTime{Time: now.UTC()}}
		replaced := b.addVersion(key, marker)

		return deletion{versionID: marker.ID, marker: true, added: true}, replaced, nil
	}

	object := b.Objects[key]
	if object == nil {
		return deletion{absent: true}, nil, nil
	}

	index := slices.IndexFunc(object.Versions, func(v *Version) bool { return v.ID == versionID })
	if index < 0 {
		return deletion{}, nil, errNoSuchVersion
	}

	v := object.Versions[index]

	held := v.Lock.active(now, bypass)
	if held {
		return deletion{}, nil, errObjectLocked
	}

	object.Versions = slices.Delete(object.Versions, index, index+1)
	if len(object.Versions) == 0 {
		b.dropObject(key)
	}

	return deletion{versionID: versionID, marker: v.DeleteMarker}, []*Version{v}, nil
}
