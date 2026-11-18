package s3

import (
	"context"
	"crypto/md5" //nolint:gosec // S3's ETag and Content-MD5 are MD5 by definition.
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
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
)

// PutObject stores the body under the key, streaming it into a blob while it
// computes the ETag (MD5) and the checksum, so no body is held whole. A
// Content-MD5 or checksum the body does not match leaves the key as it was.
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
	if len(key) > maxKeyBytes {
		return nil, (&s3api.KeyTooLongError{
			MaxSizeAllowed: new(strconv.Itoa(maxKeyBytes)), Size: new(strconv.Itoa(len(key))),
		}).Err("Your key is too long")
	}

	contentMD5 := deref(in.ContentMD5)
	if contentMD5 != "" && !validContentMD5(contentMD5) {
		return nil, (&s3api.InvalidDigest{Content_MD5: &contentMD5}).Err("The Content-MD5 you specified was invalid.")
	}

	algorithm, wantChecksum, err := requestChecksum(in)
	if err != nil {
		return nil, err
	}

	body, algorithm, err := streamingBody(rc.Request.Header, in.Body, algorithm)
	if err != nil {
		return nil, err
	}

	config, err := putObjectConfig(in)
	if err != nil {
		return nil, err
	}

	in.ContentEncoding = withoutAWSChunked(in.ContentEncoding)
	v := newVersion(in, b, algorithm)
	v.ACL, v.Tags = config.acl, config.tags

	err = p.writeBody(v, body, wantChecksum, contentMD5)
	if err != nil {
		return nil, err
	}

	err = p.commitVersion(b, key, v)
	if err != nil {
		return nil, err
	}

	out := &s3api.PutObjectOutput{
		ETag: new(quoted(v.ETag)), ServerSideEncryption: nonEmpty(v.ServerSideEncryption),
		Expiration: b.expiration(key, v),
	}
	c := checksumsOf(v)
	out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = c.sha1, c.sha256, c.kind

	return out, nil
}

// GetObject streams the current version of the object. The body is opened
// before the call returns, so a PutObject that replaces the object meanwhile
// does not cut it short.
func (p *Provider) GetObject(ctx context.Context, in *s3api.GetObjectInput) (*s3api.GetObjectOutput, error) {
	var body io.ReadCloser

	v, err := p.readable(ctx, in.Bucket, in.Key, in.VersionId, func(v *Version) error {
		opened, openErr := p.blobs.Open(v.BlobID)
		if openErr != nil {
			return fmt.Errorf("open object: %w", openErr)
		}

		body = opened

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &s3api.GetObjectOutput{
		Body:                    body,
		AcceptRanges:            new(acceptRangesBytes),
		ContentLength:           new(v.Size),
		ETag:                    new(quoted(v.ETag)),
		LastModified:            &protocol.Time{Time: v.LastModified},
		Metadata:                v.Metadata,
		ContentType:             nonEmpty(v.ContentType),
		CacheControl:            nonEmpty(v.CacheControl),
		ContentDisposition:      nonEmpty(v.ContentDisposition),
		ContentEncoding:         nonEmpty(v.ContentEncoding),
		ContentLanguage:         nonEmpty(v.ContentLanguage),
		Expires:                 nonEmpty(v.Expires),
		ServerSideEncryption:    nonEmpty(v.ServerSideEncryption),
		StorageClass:            storageClassOf(v),
		WebsiteRedirectLocation: nonEmpty(v.WebsiteRedirectLocation),
		TagCount:                tagCountOf(v),
		Expiration:              p.expirationOf(ctx, in.Bucket, deref(in.Key), in.VersionId, v),
	}

	if deref(in.ChecksumMode) == checksumModeEnabled {
		c := checksumsOf(v)
		out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
		out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = c.sha1, c.sha256, c.kind
	}

	return out, nil
}

// HeadObject answers what GetObject would, without the body.
func (p *Provider) HeadObject(ctx context.Context, in *s3api.HeadObjectInput) (*s3api.HeadObjectOutput, error) {
	v, err := p.readable(ctx, in.Bucket, in.Key, in.VersionId, func(*Version) error { return nil })
	if err != nil {
		return nil, err
	}

	out := &s3api.HeadObjectOutput{
		AcceptRanges:            new(acceptRangesBytes),
		ContentLength:           new(v.Size),
		ETag:                    new(quoted(v.ETag)),
		LastModified:            &protocol.Time{Time: v.LastModified},
		Metadata:                v.Metadata,
		ContentType:             nonEmpty(v.ContentType),
		CacheControl:            nonEmpty(v.CacheControl),
		ContentDisposition:      nonEmpty(v.ContentDisposition),
		ContentEncoding:         nonEmpty(v.ContentEncoding),
		ContentLanguage:         nonEmpty(v.ContentLanguage),
		Expires:                 nonEmpty(v.Expires),
		ServerSideEncryption:    nonEmpty(v.ServerSideEncryption),
		StorageClass:            storageClassOf(v),
		WebsiteRedirectLocation: nonEmpty(v.WebsiteRedirectLocation),
		TagCount:                tagCountOf(v),
		Expiration:              p.expirationOf(ctx, in.Bucket, deref(in.Key), in.VersionId, v),
	}

	if deref(in.ChecksumMode) == checksumModeEnabled {
		c := checksumsOf(v)
		out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
		out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = c.sha1, c.sha256, c.kind
	}

	return out, nil
}

// DeleteObject removes the object and its body. Deleting a key that does not
// exist succeeds.
func (p *Provider) DeleteObject(ctx context.Context, in *s3api.DeleteObjectInput) (*s3api.DeleteObjectOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	if id := deref(in.VersionId); id != "" && id != versionNull {
		return nil, invalidArgument("Invalid version id specified", "versionId", &id)
	}

	b.mu.Lock()

	object, found := b.Objects[deref(in.Key)]
	delete(b.Objects, deref(in.Key))

	b.mu.Unlock()

	if found {
		for _, v := range object.Versions {
			if v.BlobID != "" {
				_ = p.blobs.Remove(v.BlobID)
			}
		}
	}

	return &s3api.DeleteObjectOutput{}, nil
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

			return fmt.Errorf("read object body: %w", err)
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

// commitVersion makes v the object's current version. Outside a versioned
// bucket it replaces the one there was, whose body goes.
func (p *Provider) commitVersion(b *Bucket, key string, v *Version) error {
	b.mu.Lock()

	if b.gone {
		b.mu.Unlock()
		_ = p.blobs.Remove(v.BlobID)

		return noSuchBucket(b.Name)
	}

	var replaced []*Version

	if object, ok := b.Objects[key]; ok {
		replaced = object.Versions
	}

	b.Objects[key] = &Object{Versions: []*Version{v}}
	b.mu.Unlock()

	for _, old := range replaced {
		if old.BlobID != "" {
			_ = p.blobs.Remove(old.BlobID)
		}
	}

	return nil
}

// readable returns the current version of an object for a GET or HEAD, as
// S3Bucket.get_object does outside a versioned bucket. It calls use with the
// version while the bucket's lock is held, so a GET opens the body before a
// write that replaces the version can remove it. A Version never changes once
// committed, so its fields are safe to read after the lock is gone.
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

	if id := deref(versionID); id != "" && id != versionNull {
		return nil, invalidArgument("Invalid version id specified", "versionId", &id)
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	object, ok := b.Objects[deref(key)]
	if !ok || object.current() == nil || object.current().DeleteMarker {
		return nil, noSuchKey(deref(key))
	}

	v := object.current()

	err = use(v)
	if err != nil {
		return nil, err
	}

	return v, nil
}

// newVersion returns the version a PutObject makes, before its body is written.
func newVersion(in *s3api.PutObjectInput, b *Bucket, algorithm string) *Version {
	v := &Version{
		ID:                 versionNull,
		Metadata:           in.Metadata,
		ContentType:        deref(in.ContentType),
		CacheControl:       deref(in.CacheControl),
		ContentDisposition: deref(in.ContentDisposition),
		ContentEncoding:    deref(in.ContentEncoding),
		ContentLanguage:    deref(in.ContentLanguage),
		Expires:            deref(in.Expires),
		StorageClass:       deref(in.StorageClass),

		WebsiteRedirectLocation: deref(in.WebsiteRedirectLocation),

		ChecksumAlgorithm: algorithm,
		ChecksumType:      checksumFullObject,
		// The bucket's default encryption applies when the request names none.
		ServerSideEncryption: deref(in.ServerSideEncryption),
	}

	if v.ContentType == "" {
		v.ContentType = defaultContentType
	}

	if v.StorageClass == "" {
		v.StorageClass = storageClassDefault
	}

	if v.ChecksumAlgorithm == "" {
		v.ChecksumAlgorithm = algorithmCRC64NVME
	}

	if v.ServerSideEncryption == "" {
		v.ServerSideEncryption = defaultEncryption(b)
	}

	return v
}

// defaultEncryption is the SSE algorithm of the bucket's default encryption.
func defaultEncryption(b *Bucket) string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.Encryption == nil || len(b.Encryption.Rules) == 0 {
		return ""
	}

	if rule := b.Encryption.Rules[0].ApplyServerSideEncryptionByDefault; rule != nil {
		return deref(rule.SSEAlgorithm)
	}

	return ""
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
