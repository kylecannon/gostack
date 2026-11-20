package s3

import (
	"context"
	"crypto/md5" //nolint:gosec // S3's ETag is MD5 by definition.
	"encoding/hex"
	"fmt"
	"io"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	directiveCopy    = "COPY"
	directiveReplace = "REPLACE"
)

// copyPlan is a CopyObject resolved against the buckets it names.
type copyPlan struct {
	cond            writeConditions
	dest, srcBucket *Bucket
	destKey         string
	src             copySourceName
}

// planCopy validates the request's headers, finds both buckets and checks the
// KMS key the request names (checkKMSKey), so that nothing is read or written
// for a request that cannot succeed.
func (p *Provider) planCopy(ctx context.Context, rc *request.Context, in *s3api.CopyObjectInput) (copyPlan, error) {
	plan := copyPlan{destKey: deref(in.Key)}

	var err error

	plan.cond, err = parseWriteConditions(in.IfMatch, in.IfNoneMatch)
	if err != nil {
		return plan, err
	}

	err = checkKey(plan.destKey)
	if err != nil {
		return plan, err
	}

	plan.dest, err = p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return plan, err
	}

	plan.src, err = parseCopySource(deref(in.CopySource))
	if err != nil {
		return plan, err
	}

	plan.srcBucket, err = p.bucket(rc.AccountID, plan.src.bucket)
	if err != nil {
		return plan, err
	}

	return plan, p.checkKMSKey(ctx, deref(in.SSEKMSKeyId), plan.dest)
}

// openSource reads the source, checks it and opens its body in one hold of the
// source bucket's lock. The destination is not locked meanwhile: nothing takes
// a bucket's lock while holding another's.
func (p *Provider) openSource(plan copyPlan, in *s3api.CopyObjectInput) (*Version, io.ReadSeekCloser, error) {
	var body io.ReadSeekCloser

	selfCopy := plan.srcBucket == plan.dest && plan.src.key == plan.destKey

	source, err := readableIn(plan.srcBucket, plan.src.key, &plan.src.versionID, func(source *Version) error {
		checkErr := checkSource(in, source)
		if checkErr != nil {
			return checkErr
		}

		// A self-copy's destination is this bucket, whose lock is held.
		if selfCopy && !changesObject(in) && !copyAllowedInPlace(plan.dest, source) {
			return s3api.NewInvalidRequest("This copy request is illegal because it is trying to copy an object to " +
				"itself without changing the object's metadata, storage class, website redirect location or " +
				"encryption attributes.")
		}

		var openErr error

		body, openErr = p.blobs.Open(source.BlobID)
		if openErr != nil {
			return fmt.Errorf("open source object: %w", openErr)
		}

		return nil
	})

	return source, body, err
}

// CopyObject writes a new object from an existing one, streaming the body
// between two blobs. The source's conditions, the metadata and tagging
// directives, and the destination's write conditions are checked as S3 does;
// copying an object onto itself is refused unless something about it changes.
func (p *Provider) CopyObject(ctx context.Context, in *s3api.CopyObjectInput) (*s3api.CopyObjectOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	plan, err := p.planCopy(ctx, rc, in)
	if err != nil {
		return nil, err
	}

	v, err := p.newCopiedVersion(ctx, in, plan.dest)
	if err != nil {
		return nil, err
	}

	// The copy keeps no lock from its source: it takes the one the request names,
	// or the bucket's default retention.
	v.Lock, err = plan.dest.lockFor(in.ObjectLockMode, in.ObjectLockRetainUntilDate, in.ObjectLockLegalHoldStatus)
	if err != nil {
		return nil, err
	}

	source, body, err := p.openSource(plan, in)
	if err != nil {
		return nil, copySourceError(err)
	}

	defer func() { _ = body.Close() }()

	inherit(v, in, source)

	err = p.copyBody(v, body)
	if err != nil {
		return nil, err
	}

	err = p.commitVersion(plan.dest, plan.destKey, v, plan.cond.checker(plan.destKey))
	if err != nil {
		return nil, err
	}

	p.notify(ctx, plan.dest, plan.destKey, v)

	result := &s3api.CopyObjectResult{ETag: new(quoted(v.ETag)), LastModified: &protocol.Time{Time: v.LastModified.Time}}
	c := checksumsOf(v)
	result.ChecksumCRC32, result.ChecksumCRC32C, result.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	result.ChecksumSHA1, result.ChecksumSHA256, result.ChecksumType = c.sha1, c.sha256, c.kind

	out := &s3api.CopyObjectOutput{
		CopyObjectResult:    result,
		Expiration:          plan.dest.expiration(plan.destKey, v),
		VersionId:           plan.dest.versionHeader(v),
		CopySourceVersionId: sourceVersionHeader(source),
		SSEKMSEncryptionContext: kmsContext(objectEncryption{algorithm: v.ServerSideEncryption},
			in.SSEKMSEncryptionContext),
	}
	out.ServerSideEncryption, out.SSEKMSKeyId, out.BucketKeyEnabled = versionEncryption(v)
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = copyEncryption(in).customer.echo()

	return out, nil
}

// checkSource holds the source version to what a copy needs of it: not
// archived, and its conditions met.
func checkSource(in *s3api.CopyObjectInput, source *Version) error {
	err := checkReadableSource(source)
	if err != nil {
		return err
	}

	condition := copySourceCondition(in, source)
	if condition != "" {
		return preconditionFailed(condition)
	}

	// The source's key, then the target's own parameters.
	err = checkCustomerRead(source.SSECustomerKeyMD5, customerKeyOf(
		in.CopySourceSSECustomerAlgorithm, in.CopySourceSSECustomerKey, in.CopySourceSSECustomerKeyMD5), accessDenied)
	if err != nil {
		return err
	}

	target := copyEncryption(in)

	return validateCustomerKey(target.customer, target.algorithm)
}

// checkReadableSource is checkReadable for the source of a copy.
func checkReadableSource(source *Version) error {
	if !archived(source) {
		return nil
	}

	return (&s3api.InvalidObjectState{StorageClass: &source.StorageClass}).Err(
		"Operation is not valid for the source object's storage class")
}

// changesObject reports whether a copy onto the same key changes the object:
// its metadata, storage class, website redirect location or encryption. A
// bucket with encryption configured allows it too (copyAllowedInPlace).
func changesObject(in *s3api.CopyObjectInput) bool {
	return deref(in.StorageClass) != "" || deref(in.ServerSideEncryption) != "" ||
		deref(in.SSECustomerKeyMD5) != "" || deref(in.MetadataDirective) == directiveReplace ||
		deref(in.WebsiteRedirectLocation) != ""
}

// newCopiedVersion returns the version a CopyObject makes with what the request
// sets, before it takes what it inherits from the source. Directives other than
// COPY and REPLACE are refused. An aws:kms copy that names no key gets the
// account's AWS managed key.
func (p *Provider) newCopiedVersion(ctx context.Context, in *s3api.CopyObjectInput, dest *Bucket) (*Version, error) {
	directive := deref(in.MetadataDirective)
	if directive != "" && directive != directiveCopy && directive != directiveReplace {
		return nil, invalidArgument("Unknown metadata directive.", "x-amz-metadata-directive", &directive)
	}

	taggingDirective := deref(in.TaggingDirective)
	if taggingDirective != "" && taggingDirective != directiveCopy && taggingDirective != directiveReplace {
		return nil, invalidArgument("Unknown tagging directive.", "x-amz-tagging-directive", &taggingDirective)
	}

	err := checkStorageClass(in.StorageClass)
	if err != nil {
		return nil, err
	}

	err = checkChecksumAlgorithm(deref(in.ChecksumAlgorithm))
	if err != nil {
		return nil, err
	}

	err = checkWebsiteRedirect(deref(in.WebsiteRedirectLocation))
	if err != nil {
		return nil, err
	}

	v := &Version{
		ID:                      versionNull,
		StorageClass:            deref(in.StorageClass),
		WebsiteRedirectLocation: deref(in.WebsiteRedirectLocation),
	}

	// The target takes the encryption the request names or the bucket's
	// default, never the source's. The request's SSE-C parameters are checked
	// with the source's (checkSource), once the source has been found.
	enc, err := p.encryptionFor(ctx, copyEncryption(in), dest)
	if err != nil {
		return nil, err
	}

	v.setEncryption(enc)

	if in.Tagging != nil {
		v.Tags, err = parseTaggingHeader(*in.Tagging)
		if err != nil {
			return nil, err
		}
	}

	// A copy takes the ACL the request names, not the source's.
	v.ACL, err = newResourceACL(in.ACL, grantHeaders{
		fullControl: in.GrantFullControl, read: in.GrantRead, readACP: in.GrantReadACP, writeACP: in.GrantWriteACP,
	})
	if err != nil {
		return nil, err
	}

	if v.StorageClass == "" {
		v.StorageClass = storageClassDefault
	}

	return v, nil
}

// inherit gives v the metadata, tags and checksum algorithm a copy takes from
// source or from the request: REPLACE takes the request's metadata and system
// headers, anything else the source's.
func inherit(v *Version, in *s3api.CopyObjectInput, source *Version) {
	if deref(in.MetadataDirective) == directiveReplace {
		v.Metadata = decodeUserMetadata(in.Metadata)
		v.ContentType = deref(in.ContentType)
		v.CacheControl = deref(in.CacheControl)
		v.ContentDisposition = deref(in.ContentDisposition)
		v.ContentEncoding = deref(in.ContentEncoding)
		v.ContentLanguage = deref(in.ContentLanguage)
		v.Expires = deref(in.Expires)

		if v.ContentType == "" {
			v.ContentType = defaultContentType
		}
	} else {
		v.Metadata = source.Metadata
		v.ContentType = source.ContentType
		v.CacheControl = source.CacheControl
		v.ContentDisposition = source.ContentDisposition
		v.ContentEncoding = source.ContentEncoding
		v.ContentLanguage = source.ContentLanguage
		v.Expires = source.Expires
	}

	if deref(in.TaggingDirective) != directiveReplace {
		v.Tags = source.Tags
	}

	v.ChecksumAlgorithm = source.ChecksumAlgorithm
	if algorithm := deref(in.ChecksumAlgorithm); algorithm != "" {
		v.ChecksumAlgorithm = algorithm
	}
}

// copyBody streams the source's body into a new blob of v. The copy is a whole
// object: its ETag is the MD5 of the bytes, not the source's multipart ETag, and
// its checksum is a full-object one of its own algorithm, both computed on the
// way, as LocalStack's storage backend does.
func (p *Provider) copyBody(v *Version, body io.Reader) error {
	err := checkMetadataSize(v.Metadata)
	if err != nil {
		return err
	}

	writer, err := p.blobs.Create()
	if err != nil {
		return fmt.Errorf("store object: %w", err)
	}

	etag := md5.New() //nolint:gosec // S3's ETag is MD5 by definition.
	sum := newChecksum(v.ChecksumAlgorithm)

	_, err = io.Copy(io.MultiWriter(writer, etag, sum), body)
	if err != nil {
		writer.Abort()

		return fmt.Errorf("copy object body: %w", err)
	}

	v.BlobID, v.Size, err = writer.Commit()
	if err != nil {
		return fmt.Errorf("store object: %w", err)
	}

	v.ETag = hex.EncodeToString(etag.Sum(nil))
	v.ChecksumValue, v.ChecksumType = encodeChecksum(sum), checksumFullObject
	v.LastModified = protocol.SnapshotTime{Time: time.Now().UTC()}

	return nil
}
