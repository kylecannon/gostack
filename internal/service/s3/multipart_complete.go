package s3

import (
	"context"
	"crypto/md5" //nolint:gosec // a multipart ETag is the MD5 of the part MD5s by definition.
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// completion is a validated CompleteMultipartUpload: the parts in the order
// the object takes them, and what the object's ETag and checksum are.
type completion struct {
	upload *MultipartUpload
	// readers are the bodies of parts, open.
	readers []io.ReadSeekCloser
	parts   []*Part
	layout  []ObjectPart
	etag    string
	// composite is the COMPOSITE checksum, "" for any other type: a full-object
	// checksum is computed over the body as the parts are assembled.
	composite string
	// want is the full-object checksum the request sent, "" for none.
	want string
}

// CompleteMultipartUpload assembles the parts the request lists into the
// object. Every check runs before any body is copied, as
// S3Multipart.complete_multipart runs them, so a refused request leaves the
// upload as it was. The parts are copied into one blob outside the bucket's
// lock, hashing the full-object checksum on the way, which equals the
// combined part CRCs LocalStack computes (checksums.py) without porting the
// combine.
func (p *Provider) CompleteMultipartUpload(
	ctx context.Context, in *s3api.CompleteMultipartUploadInput,
) (*s3api.CompleteMultipartUploadOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	key := deref(in.Key)

	plan, err := p.prepareCompletion(b, key, in)
	if err != nil {
		return nil, err
	}

	v, err := p.assemble(plan)
	if err != nil {
		return nil, err
	}

	upload := plan.upload
	b.mu.Lock()

	if b.Uploads[upload.ID] != upload {
		b.mu.Unlock()
		_ = p.blobs.Remove(v.BlobID)

		return nil, noSuchUpload(upload.ID)
	}

	delete(b.Uploads, upload.ID)
	parts := upload.Parts
	upload.Parts = nil
	b.mu.Unlock()

	for _, part := range parts {
		_ = p.blobs.Remove(part.BlobID)
	}

	err = p.commitVersion(b, key, v)
	if err != nil {
		return nil, err
	}

	return completeOutput(in, v, p.bucketURL(b.Name)+quoteKey(key)), nil
}

// completeOutput is the answer to a CompleteMultipartUpload that made v. S3
// leaves the checksum out of it for an object with a KMS key, though a GET
// still has it (provider.py:2870).
func completeOutput(
	in *s3api.CompleteMultipartUploadInput, v *Version, location string,
) *s3api.CompleteMultipartUploadOutput {
	out := &s3api.CompleteMultipartUploadOutput{
		Bucket: in.Bucket, Key: in.Key, ETag: new(quoted(v.ETag)), Location: &location,
	}
	out.ServerSideEncryption, out.SSEKMSKeyId, out.BucketKeyEnabled = versionEncryption(v)

	if v.SSEKMSKeyID != "" {
		return out
	}

	c := checksumsOf(v)
	out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	out.ChecksumSHA1, out.ChecksumSHA256, out.ChecksumType = c.sha1, c.sha256, c.kind

	return out
}

// prepareCompletion finds the upload, checks the request against it and opens
// the bodies of the parts it lists, all under the bucket's read lock: a reader
// keeps its blob even if the part is replaced before the copy.
func (p *Provider) prepareCompletion(
	b *Bucket, key string, in *s3api.CompleteMultipartUploadInput,
) (*completion, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	upload := b.Uploads[deref(in.UploadId)]
	if upload == nil || upload.Key != key {
		return nil, noSuchUpload(deref(in.UploadId))
	}

	err := completePrecondition(b, upload, deref(in.IfMatch), deref(in.IfNoneMatch))
	if err != nil {
		return nil, err
	}

	plan, err := planCompletion(upload, in)
	if err != nil {
		return nil, err
	}

	plan.upload = upload
	readers := make([]io.ReadSeekCloser, 0, len(plan.parts))

	for _, part := range plan.parts {
		r, openErr := p.blobs.Open(part.BlobID)
		if openErr != nil {
			for _, opened := range readers {
				_ = opened.Close()
			}

			return nil, fmt.Errorf("open part %d: %w", part.Number, openErr)
		}

		readers = append(readers, r)
	}

	plan.readers = readers

	return plan, nil
}

// assemble copies the parts into a new blob and returns the version it makes.
// An upload that named no checksum gets a full-object CRC64NVME, as LocalStack
// computes one by reading the object back (provider.py:2839-2845).
func (p *Provider) assemble(plan *completion) (*Version, error) {
	upload, readers := plan.upload, plan.readers
	algorithm, kind := upload.ChecksumAlgorithm, upload.ChecksumType
	// An SSE-C upload that named none keeps none: S3 drops the default
	// checksum there, though not for a PutObject (provider.py:2420).
	if algorithm == "" && upload.SSECustomerKeyMD5 == "" {
		algorithm, kind = algorithmCRC64NVME, checksumFullObject
	}

	writer, err := p.blobs.Create()
	if err != nil {
		for _, r := range readers {
			_ = r.Close()
		}

		return nil, fmt.Errorf("store object: %w", err)
	}

	var (
		dst  io.Writer = writer
		full hash.Hash
	)

	if kind == checksumFullObject {
		full = newChecksum(algorithm)
		dst = io.MultiWriter(writer, full)
	}

	err = copyBlobs(dst, readers)
	if err != nil {
		writer.Abort()

		return nil, err
	}

	checksum := plan.composite
	if kind == checksumFullObject {
		checksum = encodeChecksum(full)

		if plan.want != "" && plan.want != checksum {
			writer.Abort()

			return nil, s3api.NewBadDigest(fmt.Sprintf("The %s you specified did not match the calculated checksum.",
				strings.ToLower(algorithm)))
		}
	}

	id, size, err := writer.Commit()
	if err != nil {
		return nil, fmt.Errorf("store object: %w", err)
	}

	v := upload.object(plan)
	v.BlobID, v.Size = id, size
	v.ChecksumAlgorithm, v.ChecksumValue, v.ChecksumType = algorithm, checksum, kind

	return v, nil
}

// object is the version an upload completes into, before its body is stored.
func (u *MultipartUpload) object(plan *completion) *Version {
	return &Version{
		ID:                 versionNull,
		ETag:               plan.etag,
		LastModified:       time.Now().UTC(),
		Metadata:           u.Metadata,
		ContentType:        u.ContentType,
		CacheControl:       u.CacheControl,
		ContentDisposition: u.ContentDisposition,
		ContentEncoding:    u.ContentEncoding,
		ContentLanguage:    u.ContentLanguage,
		Expires:            u.Expires,
		StorageClass:       u.StorageClass,
		Parts:              plan.layout,

		ServerSideEncryption: u.ServerSideEncryption,
		SSEKMSKeyID:          u.SSEKMSKeyID,
		BucketKeyEnabled:     u.BucketKeyEnabled,
		SSECustomerKeyMD5:    u.SSECustomerKeyMD5,
	}
}

// planCompletion checks the parts a CompleteMultipartUpload lists against the
// upload, as S3Multipart.complete_multipart and the provider's checks after it
// do (models.py:518-604, provider.py:2771-2826). The caller holds b.mu.
func planCompletion(upload *MultipartUpload, in *s3api.CompleteMultipartUploadInput) (*completion, error) {
	var listed []s3api.CompletedPart
	if in.MultipartUpload != nil {
		listed = in.MultipartUpload.Parts
	}

	err := checkPartList(upload, listed, deref(in.ChecksumType))
	if err != nil {
		return nil, err
	}

	plan := &completion{parts: make([]*Part, 0, len(listed)), layout: make([]ObjectPart, 0, len(listed))}
	etags := md5.New() //nolint:gosec // see the import.

	var composite hash.Hash
	if upload.ChecksumType == checksumComposite {
		composite = newChecksum(upload.ChecksumAlgorithm)
	}

	var offset int64

	for i, listedPart := range listed {
		minSize := int64(minPartSize)
		if i == len(listed)-1 {
			minSize = 0
		}

		part, err := checkPart(upload, listedPart, minSize)
		if err != nil {
			return nil, err
		}

		raw, _ := hex.DecodeString(part.ETag)
		etags.Write(raw)

		layout := ObjectPart{Number: part.Number, Offset: offset, Size: part.Size, ETag: part.ETag}

		if composite != nil {
			sum, _ := base64.StdEncoding.DecodeString(part.ChecksumValue)
			composite.Write(sum)
			layout.Checksum = part.ChecksumValue
		}

		plan.parts = append(plan.parts, part)
		plan.layout = append(plan.layout, layout)
		offset += part.Size
	}

	if size := in.MpuObjectSize; size != nil && *size != 0 && *size != offset {
		return nil, s3api.NewInvalidRequest(fmt.Sprintf(
			"The provided 'x-amz-mp-object-size' header value %d does not match what was computed: %d", *size, offset))
	}

	suffix := "-" + strconv.Itoa(len(listed))
	plan.etag = hex.EncodeToString(etags.Sum(nil)) + suffix

	if composite != nil {
		plan.composite = encodeChecksum(composite) + suffix
	}

	return plan, requestChecksums(upload, in, plan)
}

// checkPartList checks that a completion lists parts, in order, and names the
// upload's checksum type if it names one.
func checkPartList(upload *MultipartUpload, listed []s3api.CompletedPart, kind string) error {
	if len(listed) == 0 {
		return s3api.NewInvalidRequest("You must specify at least one part")
	}

	for i := 1; i < len(listed); i++ {
		if deref32(listed[i].PartNumber) < deref32(listed[i-1].PartNumber) {
			return (&s3api.InvalidPartOrder{UploadId: &upload.ID}).Err(
				"The list of parts was not in ascending order. Parts must be ordered by part number.")
		}
	}

	if kind != "" && kind != upload.ChecksumType {
		return s3api.NewInvalidRequest(fmt.Sprintf("The upload was created using the %s checksum mode. "+
			"The complete request must use the same checksum mode.", orNull(upload.ChecksumType)))
	}

	return nil
}

// requestChecksums checks the object checksum a completion sends. A
// FULL_OBJECT value is kept to compare once the body is assembled; a value
// sent without x-amz-checksum-type, or one of another algorithm, is refused
// (provider.py:2809-2826).
func requestChecksums(upload *MultipartUpload, in *s3api.CompleteMultipartUploadInput, plan *completion) error {
	if upload.ChecksumAlgorithm == "" {
		return nil
	}

	sent := checksumMembers(in.ChecksumCRC32, in.ChecksumCRC32C, in.ChecksumCRC64NVME, in.ChecksumSHA1,
		in.ChecksumSHA256)
	value := sent[upload.ChecksumAlgorithm]

	mismatch := value != "" && upload.ChecksumType == checksumFullObject && deref(in.ChecksumType) == ""
	for algorithm, other := range sent {
		if algorithm != upload.ChecksumAlgorithm && other != "" {
			mismatch = true
		}
	}

	if mismatch {
		return s3api.NewBadDigest(fmt.Sprintf("The %s you specified did not match the calculated checksum.",
			strings.ToLower(upload.ChecksumAlgorithm)))
	}

	if upload.ChecksumType == checksumFullObject {
		plan.want = value
	}

	return nil
}

// checkPart returns the upload's part that a listed part names, after the
// checks complete_multipart makes of it. A part under minSize bytes is too
// small: every part but the last must hold 5 MiB.
func checkPart(upload *MultipartUpload, listed s3api.CompletedPart, minSize int64) (*Part, error) {
	number := deref32(listed.PartNumber)
	etag := strings.Trim(deref(listed.ETag), `"`)
	sent := checksumMembers(listed.ChecksumCRC32, listed.ChecksumCRC32C, listed.ChecksumCRC64NVME,
		listed.ChecksumSHA1, listed.ChecksumSHA256)
	invalidPart := (&s3api.InvalidPart{ETag: &etag, PartNumber: &number, UploadId: &upload.ID}).Err(
		"One or more of the specified parts could not be found.  The part may not have been uploaded, " +
			"or the specified entity tag may not match the part's entity tag.")

	part := upload.Parts[number]
	if part == nil || part.ETag != etag || (upload.ChecksumAlgorithm == "" && len(sent) > 0) {
		return nil, invalidPart
	}

	if upload.ChecksumAlgorithm != "" {
		value, ok := sent[upload.ChecksumAlgorithm]

		switch {
		case !ok && upload.ChecksumType == checksumComposite:
			for algorithm := range sent {
				return nil, s3api.NewBadDigest(fmt.Sprintf("The %s you specified for part %d did not match what we received.",
					strings.ToLower(algorithm), number))
			}

			return nil, s3api.NewInvalidRequest(fmt.Sprintf("The upload was created using a %s checksum. "+
				"The complete request must include the checksum for each part. It was missing for part %d in the request.",
				strings.ToLower(upload.ChecksumAlgorithm), number))
		case ok && value != part.ChecksumValue:
			return nil, invalidPart
		default:
		}
	}

	if part.Size < minSize {
		return nil, (&s3api.EntityTooSmall{
			ETag: &etag, PartNumber: &number, MinSizeAllowed: new(int64(minPartSize)), ProposedSize: &part.Size,
		}).Err("Your proposed upload is smaller than the minimum allowed size")
	}

	return part, nil
}

// completePrecondition checks the If-Match and If-None-Match of a
// completion against the key's object (provider.py:2731-2768). The caller holds
// b.mu.
func completePrecondition(b *Bucket, upload *MultipartUpload, ifMatch, ifNoneMatch string) error {
	notImplemented := func(header, why string) error {
		return (&s3api.NotImplemented{Header: &header, AdditionalMessage: &why}).Err(
			"A header you provided implies functionality that is not implemented")
	}
	refused := "We don't accept the provided value of If-None-Match header for this API"

	switch {
	case ifMatch != "" && ifNoneMatch != "":
		return notImplemented("If-Match,If-None-Match", "Multiple conditional request headers present in the request")
	case ifNoneMatch != "" && ifNoneMatch != "*":
		return notImplemented("If-None-Match", refused)
	case ifNoneMatch != "" && hasCurrentObject(b, upload.Key):
		return (&s3api.PreconditionFailed{Condition: new("If-None-Match")}).Err(
			"At least one of the pre-conditions you specified did not hold")
	case ifNoneMatch != "" && upload.KeyExisted:
		return conditionalConflict("If-None-Match", upload.Key)
	case ifMatch == "*":
		return notImplemented("If-None-Match", refused)
	case ifMatch != "":
		if !hasCurrentObject(b, upload.Key) {
			return noSuchKey(upload.Key)
		}

		current := b.Objects[upload.Key].current()
		if current.ETag != strings.Trim(ifMatch, `"`) {
			return (&s3api.PreconditionFailed{Condition: new("If-Match")}).Err(
				"At least one of the pre-conditions you specified did not hold")
		}

		if upload.Initiated.Before(current.LastModified) {
			return conditionalConflict("If-Match", upload.Key)
		}
	default:
	}

	return nil
}

func conditionalConflict(condition, key string) error {
	return (&s3api.ConditionalRequestConflict{Condition: &condition, Key: &key}).Err(
		"The conditional request cannot succeed due to a conflicting operation against this resource.")
}

// checksumMembers maps the algorithm of each checksum member that is set to
// its value.
func checksumMembers(crc32Value, crc32cValue, crc64nvmeValue, sha1Value, sha256Value *string) map[string]string {
	sent := make(map[string]string)

	for algorithm, value := range map[string]*string{
		algorithmCRC32: crc32Value, algorithmCRC32C: crc32cValue, algorithmCRC64NVME: crc64nvmeValue,
		algorithmSHA1: sha1Value, algorithmSHA256: sha256Value,
	} {
		if value != nil {
			sent[algorithm] = *value
		}
	}

	return sent
}

// quoteKey is urllib.parse.quote of a key: every byte but the unreserved
// characters and the slash is percent-encoded.
func quoteKey(key string) string {
	var b strings.Builder

	for i := range len(key) {
		c := key[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("_.-~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}

	return b.String()
}

func deref32(n *int32) int32 {
	if n == nil {
		return 0
	}

	return *n
}
