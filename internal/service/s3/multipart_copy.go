package s3

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
)

const copyRangeForm = "The x-amz-copy-source-range value must be of the form bytes=first-last " +
	"where first and last are the zero-based offsets of the first and last bytes to copy"

// UploadPartCopy makes a part from an object or a range of it, streaming the
// source blob into the part's blob.
func (p *Provider) UploadPartCopy(
	ctx context.Context, in *s3api.UploadPartCopyInput,
) (*s3api.UploadPartCopyOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	target, err := p.uploadOf(rc.AccountID, in.Bucket, in.Key, in.UploadId, in.PartNumber)
	if err != nil {
		return nil, err
	}

	b, upload, number := target.bucket, target.upload, target.number

	source, err := p.openCopySource(rc.AccountID, deref(in.CopySource))
	if err != nil {
		return nil, err
	}

	defer func() { _ = source.body.Close() }()

	offset, length, err := copySourceRange(deref(in.CopySourceRange), source.version.Size)
	if err != nil {
		return nil, err
	}

	if condition := failedCopyPrecondition(in, source.version); condition != "" {
		return nil, (&s3api.PreconditionFailed{Condition: &condition}).Err(
			"At least one of the pre-conditions you specified did not hold")
	}

	_, err = source.body.Seek(offset, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("seek copy source: %w", err)
	}

	written := &Version{ChecksumAlgorithm: upload.ChecksumAlgorithm}

	err = p.writeBody(written, io.LimitReader(source.body, length), "", "")
	if err != nil {
		return nil, err
	}

	part := partOf(number, written)

	err = p.installPart(b, upload, part)
	if err != nil {
		return nil, err
	}

	return copyPartOutput(part, upload, source), nil
}

// copyPartOutput is the answer to an UploadPartCopy that made part.
func copyPartOutput(part *Part, upload *MultipartUpload, source *copySource) *s3api.UploadPartCopyOutput {
	result := &s3api.CopyPartResult{ETag: new(quoted(part.ETag)), LastModified: timeOf(part.LastModified)}
	c := partChecksums(part)
	result.ChecksumCRC32, result.ChecksumCRC32C, result.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	result.ChecksumSHA1, result.ChecksumSHA256 = c.sha1, c.sha256

	out := &s3api.UploadPartCopyOutput{
		CopyPartResult: result, ServerSideEncryption: nonEmpty(upload.ServerSideEncryption),
	}
	if source.versioned {
		out.CopySourceVersionId = &source.version.ID
	}

	return out
}

// copySource is the version a copy reads, with its body open, and whether its
// bucket has a versioning status.
type copySource struct {
	version   *Version
	versioned bool
	body      io.ReadSeekCloser
}

// copySourceName is what an x-amz-copy-source names.
type copySourceName struct {
	bucket, key, versionID string
}

// openCopySource finds the version an x-amz-copy-source names and opens its
// body while the source bucket's lock is held.
func (p *Provider) openCopySource(account, header string) (*copySource, error) {
	name, err := parseCopySource(header)
	if err != nil {
		return nil, err
	}

	src, err := p.bucket(account, name.bucket)
	if err != nil {
		return nil, err
	}

	src.mu.RLock()
	defer src.mu.RUnlock()

	v, err := sourceVersion(src, name.key, name.versionID)
	if err != nil {
		return nil, err
	}

	body, err := p.blobs.Open(v.BlobID)
	if err != nil {
		return nil, fmt.Errorf("open copy source: %w", err)
	}

	versioned := src.Versioning != nil && deref(src.Versioning.Status) != ""

	return &copySource{version: v, versioned: versioned, body: body}, nil
}

// parseCopySource splits an x-amz-copy-source, bucket/key?versionId=id with an
// optional leading slash, URL-decoding it with + as a space, as
// extract_bucket_key_version_id_from_copy_source does. A path that does not
// decode is taken as it is, as Python's unquote takes it.
func parseCopySource(header string) (copySourceName, error) {
	path, query, _ := strings.Cut(header, "?")
	path = strings.ReplaceAll(path, "+", " ")

	unescaped, err := url.PathUnescape(path)
	if err == nil {
		path = unescaped
	}

	bucket, key, found := strings.Cut(strings.TrimLeft(path, "/"), "/")
	if !found {
		name := "x-amz-copy-source"

		return copySourceName{}, invalidArgument("Invalid copy source object key", name, &name)
	}

	values, _ := url.ParseQuery(query)

	return copySourceName{bucket: bucket, key: key, versionID: values.Get("versionId")}, nil
}

// sourceVersion returns the version of key a copy reads: the current one, or
// the one versionID names. The caller holds b.mu.
func sourceVersion(b *Bucket, key, versionID string) (*Version, error) {
	object := b.Objects[key]

	if versionID == "" {
		if object == nil || object.current() == nil || object.current().DeleteMarker {
			return nil, noSuchKey(key)
		}

		return object.current(), nil
	}

	if object != nil {
		for _, v := range object.Versions {
			if v.ID != versionID {
				continue
			}

			if v.DeleteMarker {
				return nil, s3api.NewInvalidRequest(
					"The source of a copy request may not specifically refer to a delete marker by version id.")
			}

			return v, nil
		}
	}

	return nil, s3api.NewNoSuchVersion("The specified version does not exist.")
}

// copySourceRange returns the offset and length an x-amz-copy-source-range
// selects of an object of size bytes, the whole object when it is empty. The
// form is stricter than a GET's Range (parse_copy_source_range_header).
func copySourceRange(spec string, size int64) (int64, int64, error) {
	if spec == "" {
		return 0, size, nil
	}

	invalid := invalidArgument(copyRangeForm, "x-amz-copy-source-range", &spec)

	_, rangeSpec, found := strings.Cut(spec, "=")
	if !found || strings.Contains(rangeSpec, "=") || strings.Contains(rangeSpec, ",") {
		return 0, 0, invalid
	}

	first, last, found := strings.Cut(rangeSpec, "-")
	if !found || strings.Contains(last, "-") {
		return 0, 0, invalid
	}

	begin, beginErr := strconv.ParseInt(first, 10, 64)
	end, endErr := strconv.ParseInt(last, 10, 64)

	if beginErr != nil || endErr != nil || begin > end {
		return 0, 0, invalid
	}

	switch {
	case begin > size-1:
		return 0, 0, s3api.NewInvalidRequest("The specified copy range is invalid for the source object size")
	case end > size-1:
		return 0, 0, invalidArgument(fmt.Sprintf("Range specified is not valid for source object of size: %d", size),
			"x-amz-copy-source-range", &spec)
	default:
		return begin, end - begin + 1, nil
	}
}

// failedCopyPrecondition returns the copy-source condition that does not hold
// for source, "" when they all hold. The order matters: it is
// get_failed_upload_part_copy_source_preconditions'. Times compare at second
// resolution, as S3 keeps them.
func failedCopyPrecondition(in *s3api.UploadPartCopyInput, source *Version) string {
	modified := source.LastModified.Truncate(time.Second)
	ifMatch, ifNoneMatch := deref(in.CopySourceIfMatch), deref(in.CopySourceIfNoneMatch)
	ifUnmodifiedSince, ifModifiedSince := in.CopySourceIfUnmodifiedSince, in.CopySourceIfModifiedSince

	if ifMatch != "" {
		if strings.Trim(ifMatch, `"`) != source.ETag {
			return "x-amz-copy-source-If-Match"
		}

		if ifModifiedSince != nil && ifModifiedSince.After(modified) {
			return "x-amz-copy-source-If-Modified-Since"
		}

		if ifUnmodifiedSince != nil {
			return ""
		}
	}

	if ifUnmodifiedSince != nil && seconds(ifUnmodifiedSince).Before(modified) {
		return "x-amz-copy-source-If-Unmodified-Since"
	}

	if ifNoneMatch != "" && strings.Trim(ifNoneMatch, `"`) == source.ETag {
		return "x-amz-copy-source-If-None-Match"
	}

	if since := ifModifiedSince; since != nil && !seconds(since).Before(modified) && seconds(since).Before(time.Now()) {
		return "x-amz-copy-source-If-Modified-Since"
	}

	return ""
}

func seconds(t *protocol.Time) time.Time {
	return t.Truncate(time.Second)
}
