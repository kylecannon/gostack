package s3

import (
	"context"
	"crypto/md5" //nolint:gosec // S3's ETag and Content-MD5 are MD5 by definition.
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	// maxKeyBytes is the longest object key, in bytes of UTF-8.
	maxKeyBytes = 1024

	defaultContentType  = "binary/octet-stream"
	storageClassDefault = "STANDARD"
	acceptRangesBytes   = "bytes"
	checksumModeEnabled = "ENABLED"

	// invalidVersionMessage is what a version id that cannot name a version of
	// the bucket's objects is answered with.
	invalidVersionMessage = "Invalid version id specified"
)

// PutObject stores the body under the key, streaming it into a blob while it
// computes the ETag (MD5) and the checksum, so no body is held whole. A
// Content-MD5 or checksum the body does not match leaves the key as it was, and
// so does a conditional write whose condition fails: that is decided once more
// under the bucket's lock, so of two concurrent If-None-Match writes one wins.
func (p *Provider) PutObject(ctx context.Context, in *s3api.PutObjectInput) (*s3api.PutObjectOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	key := deref(in.Key)

	req, err := checkPut(in)
	if err != nil {
		return nil, err
	}

	v, body, err := newPutVersion(rc.Request.Header, in, b, req.algorithm)
	if err != nil {
		return nil, err
	}

	v.Lock, err = b.lockFor(in.ObjectLockMode, in.ObjectLockRetainUntilDate, in.ObjectLockLegalHoldStatus)
	if err != nil {
		return nil, err
	}

	check := req.cond.checker(key)

	err = precheck(b, key, check)
	if err != nil {
		return nil, err
	}

	err = p.writeBody(v, body, req.wantChecksum, req.contentMD5)
	if err != nil {
		return nil, err
	}

	err = p.commitVersion(b, key, v, check)
	if err != nil {
		return nil, err
	}

	p.notify(ctx, b, key, v)

	return putOutput(in, b, key, v), nil
}

// putOutput is PutObject's answer for the version it stored: its ETag,
// expiration, encryption, version id and checksum.
func putOutput(in *s3api.PutObjectInput, b *Bucket, key string, v *Version) *s3api.PutObjectOutput {
	out := &s3api.PutObjectOutput{
		ETag: new(quoted(v.ETag)), Expiration: b.expiration(key, v),
		SSEKMSEncryptionContext: kmsContext(objectEncryption{algorithm: v.ServerSideEncryption},
			in.SSEKMSEncryptionContext),
	}
	out.ServerSideEncryption, out.SSEKMSKeyId, out.BucketKeyEnabled = versionEncryption(v)
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = putEncryption(in).customer.echo()

	// Only a version made while versioning is Enabled has an id to report; a
	// suspended bucket's null version does not.
	if v.ID != versionNull {
		out.VersionId = &v.ID
	}

	c := checksumsOf(v)
	out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = c.sha1, c.sha256, c.kind

	return out
}

// newPutVersion returns the version a PutObject makes and the payload to write
// into it. An aws-chunked body is decoded as it is read, and may name its
// checksum algorithm in a trailer when the request has none; the encoding is
// not stored.
func newPutVersion(
	h http.Header, in *s3api.PutObjectInput, b *Bucket, algorithm string,
) (*Version, io.Reader, error) {
	body, algorithm, err := streamingBody(h, in.Body, algorithm)
	if err != nil {
		return nil, nil, err
	}

	config, err := putObjectConfig(in)
	if err != nil {
		return nil, nil, err
	}

	in.ContentEncoding = withoutAWSChunked(in.ContentEncoding)

	v, err := newVersion(in, b)
	if err != nil {
		return nil, nil, err
	}

	v.ACL, v.Tags = config.acl, config.tags

	if algorithm != "" {
		v.ChecksumAlgorithm = algorithm
	}

	return v, body, nil
}

// putRequest is what a PutObject asks for that is checked before anything is
// written: its conditions, Content-MD5 and checksum.
type putRequest struct {
	cond                    writeConditions
	contentMD5              string
	algorithm, wantChecksum string
}

// checkPut validates the request's own fields: storage class, key, conditional
// headers, Content-MD5 and the single checksum header it may carry.
func checkPut(in *s3api.PutObjectInput) (putRequest, error) {
	var req putRequest

	err := checkStorageClass(in.StorageClass)
	if err != nil {
		return req, err
	}

	err = checkKey(deref(in.Key))
	if err != nil {
		return req, err
	}

	req.cond, err = parseWriteConditions(in.IfMatch, in.IfNoneMatch)
	if err != nil {
		return req, err
	}

	req.contentMD5 = deref(in.ContentMD5)
	if req.contentMD5 != "" && !validContentMD5(req.contentMD5) {
		return req, invalidDigest(req.contentMD5)
	}

	req.algorithm, req.wantChecksum, err = requestChecksum(in)

	return req, err
}

// objectFields are the response members GetObject and HeadObject share.
type objectFields struct {
	contentLength *int64
	contentRange  *string
	statusCode    *int32

	etag         *string
	lastModified *protocol.Time
	metadata     map[string]string

	contentType, cacheControl, contentDisposition, contentEncoding, contentLanguage, expires *string
	serverSideEncryption, storageClass, websiteRedirect                                      *string
	sseKMSKeyID                                                                              *string
	bucketKeyEnabled                                                                         *bool

	tagCount, partsCount *int32
	checksum             checksumFields
}

// fieldsOf describes the version as a GET or HEAD answers it. A range makes it
// a 206 with the range's length; a part number adds the object's PartsCount. A
// checksum goes out for ChecksumMode ENABLED: for a range only when it covers
// the whole object, since the checksum is of the whole, except that one part of
// a COMPOSITE object answers its own.
func fieldsOf(v *Version, rng *byteRange, part *int32, checksumMode *string) objectFields {
	f := objectFields{
		contentLength:      new(v.Size),
		etag:               new(quoted(v.ETag)),
		lastModified:       &protocol.Time{Time: v.LastModified},
		metadata:           encodeUserMetadata(v.Metadata),
		contentType:        nonEmpty(v.ContentType),
		cacheControl:       nonEmpty(v.CacheControl),
		contentDisposition: nonEmpty(v.ContentDisposition),
		contentEncoding:    nonEmpty(v.ContentEncoding),
		contentLanguage:    nonEmpty(v.ContentLanguage),
		expires:            nonEmpty(v.Expires),
		storageClass:       storageClassOf(v),
		websiteRedirect:    nonEmpty(v.WebsiteRedirectLocation),
		tagCount:           tagCountOf(v),
	}

	f.serverSideEncryption, f.sseKMSKeyID, f.bucketKeyEnabled = versionEncryption(v)

	if rng != nil {
		f.contentLength = new(rng.length())
		f.contentRange = &rng.contentRange
		f.statusCode = new(int32(http.StatusPartialContent))
	}

	if part != nil && len(v.Parts) > 0 {
		f.partsCount = new(int32(len(v.Parts))) //nolint:gosec // a multipart object has at most 10000 parts.
	}

	if !checksumRequested(checksumMode) {
		return f
	}

	switch {
	case part != nil && len(v.Parts) > 0 && v.ChecksumType == checksumComposite:
		// partNumberRange has found this part, by its number.
		found, _ := v.partRange(*part)
		f.checksum = partChecksumFields(v, found)
	case rng == nil || rng.length() == v.Size:
		f.checksum = checksumsOf(v)
	default:
	}

	return f
}

// partChecksumFields is the checksum a GET of one part of a COMPOSITE object
// answers: the part's own, with the type of the object's.
func partChecksumFields(v *Version, part ObjectPart) checksumFields {
	whole := *v
	whole.ChecksumValue = part.Checksum

	fields := checksumsOf(&whole)
	if part.Checksum != "" {
		return fields
	}

	return checksumFields{kind: fields.kind}
}

// openedBody is the body of a version opened for a GET, positioned at its range.
func (p *Provider) openedBody(v *Version, rng *byteRange) (io.ReadCloser, error) {
	body, err := p.blobs.Open(v.BlobID)
	if err != nil {
		return nil, fmt.Errorf("open object: %w", err)
	}

	if rng == nil {
		return body, nil
	}

	ranged, err := openRange(body, rng)
	if err != nil {
		return nil, fmt.Errorf("open object: %w", err)
	}

	return ranged, nil
}

// GetObject streams the current version of the object, or the range of it that
// was asked for. The body is opened before the call returns, so a PutObject
// that replaces the object meanwhile does not cut it short.
func (p *Provider) GetObject(ctx context.Context, in *s3api.GetObjectInput) (*s3api.GetObjectOutput, error) {
	var (
		body io.ReadCloser
		rng  *byteRange
	)

	v, err := p.readable(ctx, in.Bucket, in.Key, in.VersionId, func(v *Version) error {
		var err error

		rng, body, err = p.prepareGet(in, v)

		return err
	})
	if err != nil {
		return nil, err
	}

	out := getOutput(fieldsOf(v, rng, in.PartNumber, in.ChecksumMode), body, in)
	out.Expiration = p.expirationOf(ctx, in.Bucket, deref(in.Key), in.VersionId, v)
	out.Restore = nonEmpty(v.Restore)
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = customerKeyOf(
		in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5).echo()

	h := p.versionHeadersOf(ctx, in.Bucket, v)
	out.VersionId, out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus =
		h.versionID, h.lockMode, h.retainUntil, h.legalHold

	return out, nil
}

// prepareGet holds a GET to what it asks of the version, in the order S3 does:
// the storage class, the conditions, the range and the header overrides, and
// then opens the body.
func (p *Provider) prepareGet(in *s3api.GetObjectInput, v *Version) (*byteRange, io.ReadCloser, error) {
	err := checkReadable(v)
	if err != nil {
		return nil, nil, err
	}

	err = checkCustomerRead(v.SSECustomerKeyMD5,
		customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5), wrongCustomerKey)
	if err != nil {
		return nil, nil, err
	}

	err = readConditions{
		ifMatch: in.IfMatch, ifNoneMatch: in.IfNoneMatch,
		ifModifiedSince: in.IfModifiedSince, ifUnmodifiedSince: in.IfUnmodifiedSince,
	}.check(v)
	if err != nil {
		return nil, nil, err
	}

	rng, err := requestedRange(v, in.Range, in.PartNumber)
	if err != nil {
		return nil, nil, err
	}

	err = checkOverrides(in)
	if err != nil {
		return nil, nil, err
	}

	body, err := p.openedBody(v, rng)

	return rng, body, err
}

// getOutput builds the answer to a GET from the object's fields, with the
// response-* overrides the request asked for.
func getOutput(f objectFields, body io.ReadCloser, in *s3api.GetObjectInput) *s3api.GetObjectOutput {
	out := &s3api.GetObjectOutput{
		Body:                    body,
		AcceptRanges:            new(acceptRangesBytes),
		ContentLength:           f.contentLength,
		ContentRange:            f.contentRange,
		StatusCode:              f.statusCode,
		ETag:                    f.etag,
		LastModified:            f.lastModified,
		Metadata:                f.metadata,
		ContentType:             f.contentType,
		CacheControl:            f.cacheControl,
		ContentDisposition:      f.contentDisposition,
		ContentEncoding:         f.contentEncoding,
		ContentLanguage:         f.contentLanguage,
		Expires:                 f.expires,
		ServerSideEncryption:    f.serverSideEncryption,
		SSEKMSKeyId:             f.sseKMSKeyID,
		BucketKeyEnabled:        f.bucketKeyEnabled,
		StorageClass:            f.storageClass,
		WebsiteRedirectLocation: f.websiteRedirect,
		TagCount:                f.tagCount,
		PartsCount:              f.partsCount,
	}

	out.ChecksumCRC32, out.ChecksumCRC32C = f.checksum.crc32, f.checksum.crc32c
	out.ChecksumCRC64NVME = f.checksum.crc64nvme
	out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = f.checksum.sha1, f.checksum.sha256, f.checksum.kind

	applyOverrides(out, in)

	return out
}

// HeadObject answers what GetObject would, without the body.
func (p *Provider) HeadObject(ctx context.Context, in *s3api.HeadObjectInput) (*s3api.HeadObjectOutput, error) {
	var rng *byteRange

	v, err := p.readable(ctx, in.Bucket, in.Key, in.VersionId, func(v *Version) error {
		err := readConditions{
			ifMatch: in.IfMatch, ifNoneMatch: in.IfNoneMatch,
			ifModifiedSince: in.IfModifiedSince, ifUnmodifiedSince: in.IfUnmodifiedSince,
		}.check(v)
		if err != nil {
			return err
		}

		err = checkCustomerRead(v.SSECustomerKeyMD5,
			customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5), wrongCustomerKey)
		if err != nil {
			return err
		}

		rng, err = requestedRange(v, in.Range, in.PartNumber)

		return err
	})
	if err != nil {
		return nil, err
	}

	f := fieldsOf(v, rng, in.PartNumber, in.ChecksumMode)

	out := headOutput(f, v)
	out.Expiration = p.expirationOf(ctx, in.Bucket, deref(in.Key), in.VersionId, v)

	out.ChecksumCRC32, out.ChecksumCRC32C = f.checksum.crc32, f.checksum.crc32c
	out.ChecksumCRC64NVME = f.checksum.crc64nvme
	out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = f.checksum.sha1, f.checksum.sha256, f.checksum.kind
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = customerKeyOf(
		in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5).echo()

	h := p.versionHeadersOf(ctx, in.Bucket, v)
	out.VersionId, out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus =
		h.versionID, h.lockMode, h.retainUntil, h.legalHold

	return out, nil
}

// checkReadable refuses a GET of an object in an archive storage class, which
// stays out of reach until it is restored.
func checkReadable(v *Version) error {
	if !archived(v) {
		return nil
	}

	return (&s3api.InvalidObjectState{StorageClass: &v.StorageClass}).Err(
		"The operation is not valid for the object's storage class")
}

// checkOverrides refuses a response-* parameter the header cannot carry.
func checkOverrides(in *s3api.GetObjectInput) error {
	for _, o := range []struct {
		name  string
		value *string
	}{
		{"response-content-type", in.ResponseContentType},
		{"response-content-language", in.ResponseContentLanguage},
		{"response-cache-control", in.ResponseCacheControl},
		{"response-content-disposition", in.ResponseContentDisposition},
		{"response-content-encoding", in.ResponseContentEncoding},
	} {
		if o.value == nil {
			continue
		}

		err := checkLatin1(o.name, *o.value)
		if err != nil {
			return err
		}
	}

	return nil
}

// applyOverrides sets the response headers a GET asked to override
// (ALLOWED_HEADER_OVERRIDES).
func applyOverrides(out *s3api.GetObjectOutput, in *s3api.GetObjectInput) {
	override := func(dst **string, value *string) {
		if value != nil && *value != "" {
			*dst = value
		}
	}

	override(&out.ContentType, in.ResponseContentType)
	override(&out.ContentLanguage, in.ResponseContentLanguage)
	override(&out.CacheControl, in.ResponseCacheControl)
	override(&out.ContentDisposition, in.ResponseContentDisposition)
	override(&out.ContentEncoding, in.ResponseContentEncoding)

	if in.ResponseExpires != nil {
		out.Expires = new(in.ResponseExpires.UTC().Format(http.TimeFormat))
	}
}

// DeleteObject removes the object and its body. Deleting a key that does not
// exist succeeds. S3 takes If-Match and its two companions only for directory
// buckets, which gostack has none of, so they answer 501, as LocalStack does.
func (p *Provider) DeleteObject(ctx context.Context, in *s3api.DeleteObjectInput) (*s3api.DeleteObjectOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	if in.BypassGovernanceRetention != nil && !objectLockOn(b) {
		return nil, invalidArgument(
			"x-amz-bypass-governance-retention is only applicable to Object Lock enabled buckets.",
			"x-amz-bypass-governance-retention", nil)
	}

	switch {
	case deref(in.IfMatch) != "":
		return nil, notImplementedHeader(conditionIfMatch, "")
	case in.IfMatchSize != nil:
		return nil, notImplementedHeader("x-amz-if-match-size", "")
	case in.IfMatchLastModifiedTime != nil:
		return nil, notImplementedHeader("x-amz-if-match-last-modified-time", "")
	}

	bypass := in.BypassGovernanceRetention != nil && *in.BypassGovernanceRetention

	removed, err := p.deleteFrom(b, deref(in.Key), deref(in.VersionId), bypass)

	switch {
	case errors.Is(err, errNoSuchVersion):
		id := deref(in.VersionId)

		return nil, invalidArgument(invalidVersionMessage, "versionId", &id)
	case errors.Is(err, errObjectLocked):
		return nil, deniedByLock()
	case err != nil:
		return nil, err
	}

	if removed.changed {
		p.notifyDeletion(ctx, b, deref(in.Key), removed)
	}

	out := &s3api.DeleteObjectOutput{VersionId: nonEmpty(removed.versionID)}
	if removed.marker {
		out.DeleteMarker = new(true)
	}

	return out, nil
}

// objectLockOn reports whether the bucket has Object Lock turned on.
func objectLockOn(b *Bucket) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.ObjectLock != nil && deref(b.ObjectLock.ObjectLockEnabled) == "Enabled"
}

// writeBody streams body into a new blob of v, computing its ETag and
// checksum, and checks them against what the client sent. On any failure the
// blob is dropped.
func (p *Provider) writeBody(v *Version, body io.Reader, wantChecksum, contentMD5 string) error {
	if wantChecksum != "" && !validChecksum(v.ChecksumAlgorithm, wantChecksum) {
		return invalidChecksumValue(v.ChecksumAlgorithm)
	}

	writer, err := p.blobs.Create()
	if err != nil {
		return fmt.Errorf("store object: %w", err)
	}

	etag := md5.New() //nolint:gosec // see the import.
	checksum := newChecksum(v.ChecksumAlgorithm)

	if body != nil {
		_, err = io.Copy(io.MultiWriter(writer, etag, checksum), body)
		if err != nil {
			writer.Abort()

			return bodyReadError(err)
		}
	}

	if wantChecksum == "" {
		wantChecksum, err = trailerChecksum(body, v.ChecksumAlgorithm)
		if err != nil {
			writer.Abort()

			return err
		}
	}

	v.ETag = hex.EncodeToString(etag.Sum(nil))
	v.ChecksumValue = encodeChecksum(checksum)

	err = checkDigests(v, wantChecksum, contentMD5, base64.StdEncoding.EncodeToString(etag.Sum(nil)))
	if err != nil {
		writer.Abort()

		return err
	}

	v.BlobID, v.Size, err = writer.Commit()
	if err != nil {
		return fmt.Errorf("store object: %w", err)
	}

	v.LastModified = time.Now().UTC()

	return nil
}

// commitVersion makes v the object's current version and gives it its id
// (addVersion). A bucket without versioning, or with it suspended, replaces the
// null version there was, whose body goes; one with it enabled keeps every
// version. A write with a
// condition passes it as check: it is called with the key's object under the
// bucket's lock, and a non-nil answer leaves the object as it was and drops the
// body of v. A nil check is none.
func (p *Provider) commitVersion(b *Bucket, key string, v *Version, check ...func(*Object) error) error {
	return p.commitVersionThen(b, key, v, nil, check...)
}

// commitVersionThen is commitVersion with a step that runs once v is the
// object's current version, under the same hold of the bucket's lock: after
// every check has passed, never before, so the checks only check and a
// refused write leaves everything but its own body as it was. then returns the
// blob ids of what it dropped, which go with the replaced versions' bodies. A
// nil then is none.
func (p *Provider) commitVersionThen(
	b *Bucket, key string, v *Version, then func() []string, check ...func(*Object) error,
) error {
	p.transfer.RLock()
	defer p.transfer.RUnlock()

	b.mu.Lock()

	if b.gone {
		b.mu.Unlock()
		_ = p.blobs.Remove(v.BlobID)

		return noSuchBucket(b.Name)
	}

	object := b.Objects[key]

	for _, condition := range check {
		if condition == nil {
			continue
		}

		err := condition(object)
		if err != nil {
			b.mu.Unlock()
			_ = p.blobs.Remove(v.BlobID)

			return err
		}
	}

	replaced := b.addVersion(key, v)

	var dropped []string
	if then != nil {
		dropped = then()
	}

	b.mu.Unlock()

	for _, old := range replaced {
		if old.BlobID != "" {
			_ = p.blobs.Remove(old.BlobID)
		}
	}

	for _, id := range dropped {
		_ = p.blobs.Remove(id)
	}

	return nil
}

// precheck runs a write condition against the key before its body is read, so
// a write that cannot succeed does not read the body.
func precheck(b *Bucket, key string, check func(*Object) error) error {
	if check == nil {
		return nil
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	return check(b.Objects[key])
}

// checker returns the condition as a check for commitVersion, or nil when
// there is none.
func (c writeConditions) checker(key string) func(*Object) error {
	if !c.any() {
		return nil
	}

	return func(object *Object) error { return c.check(object, key) }
}

// lookupVersion returns the version of a key that a GET, HEAD or copy source
// names, as S3Bucket.get_object does: the current version, or the one a version
// id names. A current delete marker makes a missing key, and a delete marker
// named by its id is a method that is not allowed. The caller holds b.mu.
func lookupVersion(b *Bucket, key string, versionID *string) (*Version, error) {
	object, index, err := b.versionTarget(key, deref(versionID), http.MethodGet)
	if err != nil {
		return nil, err
	}

	return object.Versions[index], nil
}

// readable returns the current version of an object for a GET or HEAD. It
// calls use with the version while the bucket's lock is held, so a GET opens
// the body before a write that replaces the version can remove it. A Version
// never changes once committed, so its fields are safe to read after the lock
// is gone.
func (p *Provider) readable(
	ctx context.Context, bucket, key, versionID *string, use func(*Version) error,
) (*Version, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(bucket))
	if err != nil {
		return nil, err
	}

	return readableIn(b, deref(key), versionID, use)
}

// readableIn is readable for a bucket that was found already.
func readableIn(b *Bucket, key string, versionID *string, use func(*Version) error) (*Version, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	v, err := lookupVersion(b, key, versionID)
	if err != nil {
		return nil, err
	}

	err = use(v)
	if err != nil {
		return nil, err
	}

	return v, nil
}

// checkKey refuses a key over S3's limit of 1024 bytes.
func checkKey(key string) error {
	if len(key) <= maxKeyBytes {
		return nil
	}

	return (&s3api.KeyTooLongError{
		MaxSizeAllowed: new(strconv.Itoa(maxKeyBytes)), Size: new(strconv.Itoa(len(key))),
	}).Err("Your key is too long")
}

// newVersion returns the version a PutObject makes, before its body is written.
func newVersion(in *s3api.PutObjectInput, b *Bucket) (*Version, error) {
	v := &Version{
		ID:                      versionNull,
		Metadata:                decodeUserMetadata(in.Metadata),
		ContentType:             deref(in.ContentType),
		CacheControl:            deref(in.CacheControl),
		ContentDisposition:      deref(in.ContentDisposition),
		ContentEncoding:         deref(in.ContentEncoding),
		ContentLanguage:         deref(in.ContentLanguage),
		Expires:                 deref(in.Expires),
		StorageClass:            deref(in.StorageClass),
		ChecksumAlgorithm:       algorithmCRC64NVME,
		ChecksumType:            checksumFullObject,
		WebsiteRedirectLocation: deref(in.WebsiteRedirectLocation),
	}

	err := checkMetadataSize(v.Metadata)
	if err != nil {
		return nil, err
	}

	// The bucket's default encryption applies where the request names none.
	request := putEncryption(in)

	err = validateCustomerKey(request.customer, request.algorithm)
	if err != nil {
		return nil, err
	}

	v.setEncryption(resolveEncryption(request, b))

	err = checkWebsiteRedirect(v.WebsiteRedirectLocation)
	if err != nil {
		return nil, err
	}

	if v.ContentType == "" {
		v.ContentType = defaultContentType
	}

	if v.StorageClass == "" {
		v.StorageClass = storageClassDefault
	}

	return v, nil
}

// checkDigests compares the checksum and Content-MD5 the client sent with the
// ones the body has. calculatedMD5 is the base64 of the body's MD5.
func checkDigests(v *Version, wantChecksum, contentMD5, calculatedMD5 string) error {
	if wantChecksum != "" && wantChecksum != v.ChecksumValue {
		return s3api.NewBadDigest(fmt.Sprintf("The %s you specified did not match the calculated checksum.",
			v.ChecksumAlgorithm))
	}

	if contentMD5 != "" && calculatedMD5 != contentMD5 {
		expected := etagOfContentMD5(contentMD5)

		return (&s3api.BadDigest{ExpectedDigest: &expected, CalculatedDigest: &calculatedMD5}).Err(
			"The Content-MD5 you specified did not match what we received.")
	}

	return nil
}

// validContentMD5 reports whether a Content-MD5 is the base64 of 16 bytes.
func validContentMD5(value string) bool {
	raw, err := base64.StdEncoding.DecodeString(value)

	return err == nil && len(raw) == md5.Size
}

// etagOfContentMD5 is the hex form of a valid Content-MD5.
func etagOfContentMD5(value string) string {
	raw, _ := base64.StdEncoding.DecodeString(value)

	return hex.EncodeToString(raw)
}

func storageClassOf(v *Version) *string {
	if v.StorageClass == storageClassDefault {
		return nil
	}

	return nonEmpty(v.StorageClass)
}

func quoted(etag string) string {
	return `"` + etag + `"`
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// headOutput is HeadObject's answer before the checksum, SSE-C echo, expiration
// and version headers, which the caller adds.
func headOutput(f objectFields, v *Version) *s3api.HeadObjectOutput {
	return &s3api.HeadObjectOutput{
		AcceptRanges:            new(acceptRangesBytes),
		ContentLength:           f.contentLength,
		ContentRange:            f.contentRange,
		StatusCode:              f.statusCode,
		ETag:                    f.etag,
		LastModified:            f.lastModified,
		Metadata:                f.metadata,
		ContentType:             f.contentType,
		CacheControl:            f.cacheControl,
		ContentDisposition:      f.contentDisposition,
		ContentEncoding:         f.contentEncoding,
		ContentLanguage:         f.contentLanguage,
		Expires:                 f.expires,
		ServerSideEncryption:    f.serverSideEncryption,
		SSEKMSKeyId:             f.sseKMSKeyID,
		BucketKeyEnabled:        f.bucketKeyEnabled,
		StorageClass:            f.storageClass,
		WebsiteRedirectLocation: f.websiteRedirect,
		TagCount:                f.tagCount,
		PartsCount:              f.partsCount,
		Restore:                 nonEmpty(v.Restore),
	}
}
