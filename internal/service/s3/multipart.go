package s3

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	checksumComposite = "COMPOSITE"

	// minPartSize is the smallest part but the last (S3_UPLOAD_PART_MIN_SIZE).
	minPartSize   = 5 << 20
	maxPartNumber = 10000
	// uploadIDBytes gives the 128-character id of token_urlsafe(96).
	uploadIDBytes = 96
	// initiatorName is the DisplayName AWS still sends for the initiator of
	// an upload, as LocalStack hard-codes it.
	initiatorName = "webfile"
)

// MultipartUpload is an upload in progress. Its parts are blobs until it
// completes into one.
type MultipartUpload struct {
	ID        string
	Key       string
	Initiated time.Time

	Metadata           map[string]string
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	Expires            string

	StorageClass string
	// The server-side encryption the object completes with, as Version holds it.
	ServerSideEncryption string
	SSEKMSKeyID          string
	BucketKeyEnabled     bool
	SSECustomerKeyMD5    string
	ChecksumAlgorithm    string
	ChecksumType         string

	// KeyExisted records that the key held an object when the upload began,
	// which an If-None-Match completion treats as a conflict.
	KeyExisted bool

	// Parts maps a part number to its latest upload.
	Parts map[int32]*Part
}

// Part is one uploaded part of a multipart upload.
type Part struct {
	Number        int32
	BlobID        string
	Size          int64
	ETag          string
	LastModified  time.Time
	ChecksumValue string
	// ChecksumAlgorithm is the algorithm of ChecksumValue, "" when the part
	// was sent without one.
	ChecksumAlgorithm string
}

// ObjectPart is where one part of a completed multipart object lies in its
// body, as LocalStack's InternalObjectPart records it. GetObject and HeadObject
// with partNumber, and GetObjectAttributes, read it. Checksum is the part's
// checksum in a COMPOSITE object, "" otherwise.
type ObjectPart struct {
	Number   int32
	Offset   int64
	Size     int64
	ETag     string
	Checksum string
}

// partRange returns where part number lies in v's body. An object that was
// not uploaded in parts has one part, its whole body.
func (v *Version) partRange(number int32) (ObjectPart, bool) {
	if len(v.Parts) == 0 {
		return ObjectPart{Number: 1, Size: v.Size, ETag: v.ETag}, number == 1
	}

	if number < 1 || int(number) > len(v.Parts) {
		return ObjectPart{}, false
	}

	return v.Parts[number-1], true
}

// CreateMultipartUpload starts an upload. The object's headers are taken now
// and applied when it completes.
func (p *Provider) CreateMultipartUpload(
	ctx context.Context, in *s3api.CreateMultipartUploadInput,
) (*s3api.CreateMultipartUploadOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	algorithm, kind, err := uploadChecksumType(deref(in.ChecksumAlgorithm), deref(in.ChecksumType))
	if err != nil {
		return nil, err
	}

	request := uploadEncryptionRequest(in)

	err = validateCustomerKey(request.customer, request.algorithm)
	if err != nil {
		return nil, err
	}

	upload := newUpload(in, algorithm, kind)
	enc := resolveEncryption(request, b)
	upload.setEncryption(enc)

	b.mu.Lock()

	if b.gone {
		b.mu.Unlock()

		return nil, noSuchBucket(b.Name)
	}

	upload.KeyExisted = hasCurrentObject(b, upload.Key)
	b.Uploads[upload.ID] = upload
	b.mu.Unlock()

	out := &s3api.CreateMultipartUploadOutput{
		Bucket:                  in.Bucket,
		Key:                     in.Key,
		UploadId:                &upload.ID,
		ChecksumAlgorithm:       nonEmpty(algorithm),
		ChecksumType:            nonEmpty(kind),
		SSEKMSEncryptionContext: kmsContext(enc, in.SSEKMSEncryptionContext),
	}
	out.ServerSideEncryption, out.SSEKMSKeyId, out.BucketKeyEnabled = uploadEncryption(upload)
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = request.customer.echo()

	return out, nil
}

// newUpload is the upload a CreateMultipartUpload starts, with the defaults
// PutObject gives an object.
func newUpload(in *s3api.CreateMultipartUploadInput, algorithm, kind string) *MultipartUpload {
	id := make([]byte, uploadIDBytes)
	_, _ = rand.Read(id) // crypto/rand.Read never fails.

	upload := &MultipartUpload{
		ID:                 base64.RawURLEncoding.EncodeToString(id),
		Key:                deref(in.Key),
		Initiated:          time.Now().UTC(),
		Metadata:           in.Metadata,
		ContentType:        deref(in.ContentType),
		CacheControl:       deref(in.CacheControl),
		ContentDisposition: deref(in.ContentDisposition),
		ContentEncoding:    deref(in.ContentEncoding),
		ContentLanguage:    deref(in.ContentLanguage),
		Expires:            deref(in.Expires),
		StorageClass:       deref(in.StorageClass),
		ChecksumAlgorithm:  algorithm,
		ChecksumType:       kind,
		Parts:              make(map[int32]*Part),
	}

	if upload.ContentType == "" {
		upload.ContentType = defaultContentType
	}

	if upload.StorageClass == "" {
		upload.StorageClass = storageClassDefault
	}

	return upload
}

// uploadChecksumType checks the checksum algorithm and type a
// CreateMultipartUpload names and returns them, the type defaulted: FULL_OBJECT
// for CRC64NVME and COMPOSITE for the others (provider.py:2337-2363).
func uploadChecksumType(algorithm, kind string) (string, string, error) {
	switch {
	case algorithm != "" && !knownAlgorithm(algorithm):
		return "", "", s3api.NewInvalidRequest("Checksum algorithm provided is unsupported. Please try again " +
			"with any of the valid types: [CRC32, CRC32C, CRC64NVME, SHA1, SHA256]")
	case kind != "" && algorithm == "":
		return "", "", s3api.NewInvalidRequest(
			"The x-amz-checksum-type header can only be used with the x-amz-checksum-algorithm header.")
	case algorithm == "":
		return "", "", nil
	case kind == "" && algorithm == algorithmCRC64NVME:
		kind = checksumFullObject
	case kind == "":
		kind = checksumComposite
	default:
	}

	switch {
	case kind == checksumComposite && algorithm == algorithmCRC64NVME:
		return "", "", s3api.NewInvalidRequest(
			"The COMPOSITE checksum type cannot be used with the crc64nvme checksum algorithm.")
	case kind == checksumFullObject && strings.HasPrefix(algorithm, "SHA"):
		return "", "", s3api.NewInvalidRequest(fmt.Sprintf(
			"The FULL_OBJECT checksum type cannot be used with the %s checksum algorithm.", strings.ToLower(algorithm)))
	default:
		return algorithm, kind, nil
	}
}

// UploadPart stores one part, streaming it into a blob while it computes the
// part's ETag and checksum. A Content-MD5 or checksum the body does not match
// leaves the part number as it was.
func (p *Provider) UploadPart(ctx context.Context, in *s3api.UploadPartInput) (*s3api.UploadPartOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	target, err := p.uploadOf(rc.AccountID, in.Bucket, in.Key, in.UploadId, in.PartNumber)
	if err != nil {
		return nil, err
	}

	b, upload, number := target.bucket, target.upload, target.number

	contentMD5 := deref(in.ContentMD5)
	if contentMD5 != "" && !validContentMD5(contentMD5) {
		return nil, (&s3api.InvalidDigest{Content_MD5: &contentMD5}).Err("The Content-MD5 you specified was invalid.")
	}

	algorithm, wantChecksum, err := namedChecksum(
		in.ChecksumSHA1, in.ChecksumSHA256, in.ChecksumCRC32, in.ChecksumCRC32C, in.ChecksumCRC64NVME)
	if err != nil {
		return nil, err
	}

	customer := customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5)

	err = checkPartKey(upload, customer)
	if err != nil {
		return nil, err
	}

	body, algorithm, err := streamingBody(rc.Request.Header, in.Body, algorithm)
	if err != nil {
		return nil, err
	}

	if upload.ChecksumAlgorithm != "" && algorithm != upload.ChecksumAlgorithm {
		return nil, s3api.NewInvalidRequest(fmt.Sprintf(
			"Checksum Type mismatch occurred, expected checksum Type: %s, actual checksum Type: %s",
			strings.ToLower(upload.ChecksumAlgorithm), orNull(strings.ToLower(algorithm))))
	}

	// writeBody stores a version; a part is its body, ETag and checksum.
	written := &Version{ChecksumAlgorithm: algorithm}

	err = p.writeBody(written, body, wantChecksum, contentMD5)
	if err != nil {
		return nil, err
	}

	part := partOf(number, written)

	err = p.installPart(b, upload, part)
	if err != nil {
		return nil, err
	}

	return partOutput(part, upload, customer), nil
}

// partOutput is the answer to an UploadPart that stored part.
func partOutput(part *Part, upload *MultipartUpload, customer customerKey) *s3api.UploadPartOutput {
	out := &s3api.UploadPartOutput{ETag: new(quoted(part.ETag))}
	out.ServerSideEncryption, out.SSEKMSKeyId, out.BucketKeyEnabled = uploadEncryption(upload)
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = customer.echo()

	c := partChecksums(part)
	out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	out.ChecksumSHA1, out.ChecksumSHA256 = c.sha1, c.sha256

	return out
}

// AbortMultipartUpload drops an upload and its parts.
func (p *Provider) AbortMultipartUpload(
	ctx context.Context, in *s3api.AbortMultipartUploadInput,
) (*s3api.AbortMultipartUploadOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	b.mu.Lock()

	upload := b.Uploads[deref(in.UploadId)]
	if upload == nil || upload.Key != deref(in.Key) {
		b.mu.Unlock()

		return nil, noSuchUpload(deref(in.UploadId))
	}

	delete(b.Uploads, upload.ID)
	parts := upload.Parts
	upload.Parts = nil
	b.mu.Unlock()

	for _, part := range parts {
		_ = p.blobs.Remove(part.BlobID)
	}

	return &s3api.AbortMultipartUploadOutput{}, nil
}

// partTarget is the upload and part number a part request names.
type partTarget struct {
	bucket *Bucket
	upload *MultipartUpload
	number int32
}

// uploadOf finds the upload a part request names in its bucket, and checks the
// part number.
func (p *Provider) uploadOf(account string, bucket, key, uploadID *string, partNumber *int32) (partTarget, error) {
	b, err := p.bucket(account, deref(bucket))
	if err != nil {
		return partTarget{}, err
	}

	b.mu.RLock()
	upload := b.Uploads[deref(uploadID)]
	b.mu.RUnlock()

	if upload == nil || upload.Key != deref(key) {
		return partTarget{}, noSuchUpload(deref(uploadID))
	}

	number := int32(0)
	if partNumber != nil {
		number = *partNumber
	}

	if number < 1 || number > maxPartNumber {
		return partTarget{}, invalidArgument("Part number must be an integer between 1 and 10000, inclusive",
			"partNumber", new(strconv.Itoa(int(number))))
	}

	return partTarget{bucket: b, upload: upload, number: number}, nil
}

// installPart makes part the upload's part of its number, once its body is
// stored. The part it replaces loses its body. When the upload or its bucket
// went meanwhile, the new body goes instead.
func (p *Provider) installPart(b *Bucket, upload *MultipartUpload, part *Part) error {
	b.mu.Lock()

	switch {
	case b.gone:
		b.mu.Unlock()
		_ = p.blobs.Remove(part.BlobID)

		return noSuchBucket(b.Name)
	case b.Uploads[upload.ID] != upload:
		b.mu.Unlock()
		_ = p.blobs.Remove(part.BlobID)

		return noSuchUpload(upload.ID)
	default:
	}

	replaced := upload.Parts[part.Number]
	upload.Parts[part.Number] = part
	b.mu.Unlock()

	if replaced != nil {
		_ = p.blobs.Remove(replaced.BlobID)
	}

	return nil
}

// partOf is the part that a written body makes.
func partOf(number int32, written *Version) *Part {
	part := &Part{
		Number:       number,
		BlobID:       written.BlobID,
		Size:         written.Size,
		ETag:         written.ETag,
		LastModified: written.LastModified,
	}

	if written.ChecksumAlgorithm != "" {
		part.ChecksumAlgorithm = written.ChecksumAlgorithm
		part.ChecksumValue = written.ChecksumValue
	}

	return part
}

// partChecksums returns the checksum of a part in the member of its algorithm.
func partChecksums(part *Part) checksumFields {
	return checksumsOf(&Version{ChecksumAlgorithm: part.ChecksumAlgorithm, ChecksumValue: part.ChecksumValue})
}

// hasCurrentObject reports whether key has a current version that is not a
// delete marker. The caller holds b.mu.
func hasCurrentObject(b *Bucket, key string) bool {
	object, ok := b.Objects[key]

	return ok && object.current() != nil && !object.current().DeleteMarker
}

// partsOf returns the upload's parts sorted by number.
func partsOf(upload *MultipartUpload) []*Part {
	parts := make([]*Part, 0, len(upload.Parts))
	for _, part := range upload.Parts {
		parts = append(parts, part)
	}

	slices.SortFunc(parts, func(a, b *Part) int { return int(a.Number) - int(b.Number) })

	return parts
}

// copyBlobs streams the blobs into w in order and closes them.
func copyBlobs(w io.Writer, readers []io.ReadSeekCloser) error {
	defer func() {
		for _, r := range readers {
			_ = r.Close()
		}
	}()

	for _, r := range readers {
		_, err := io.Copy(w, r)
		if err != nil {
			return fmt.Errorf("assemble parts: %w", err)
		}
	}

	return nil
}

func orNull(s string) string {
	if s == "" {
		return "null"
	}

	return s
}

func noSuchUpload(id string) error {
	return (&s3api.NoSuchUpload{UploadId: &id}).Err("The specified upload does not exist. " +
		"The upload ID may be invalid, or the upload may have been aborted or completed.")
}

// owner is the Owner and Initiator every upload reports, as
// get_owner_for_account_id returns it.
func owner() (*s3api.Owner, *s3api.Initiator) {
	return &s3api.Owner{ID: new(ownerID)}, &s3api.Initiator{ID: new(ownerID), DisplayName: new(initiatorName)}
}

func timeOf(t time.Time) *protocol.Time {
	return &protocol.Time{Time: t}
}
