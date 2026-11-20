package s3

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
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

	customer := customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5)

	err = checkCopyKeys(in, upload, source.version, customer)
	if err != nil {
		return nil, err
	}

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

	return copyPartOutput(part, upload, source, customer), nil
}

// checkCopyKeys holds an UploadPartCopy to the SSE-C keys it needs: the
// source's, as CopyObject holds them (a source stored with a key does not copy
// out in the clear), and the upload's own, as UploadPart holds them.
func checkCopyKeys(in *s3api.UploadPartCopyInput, upload *MultipartUpload, source *Version, target customerKey) error {
	err := checkCustomerRead(source.SSECustomerKeyMD5, customerKeyOf(
		in.CopySourceSSECustomerAlgorithm, in.CopySourceSSECustomerKey, in.CopySourceSSECustomerKeyMD5), accessDenied)
	if err != nil {
		return err
	}

	return checkPartKey(upload, target)
}

// copyPartOutput is the answer to an UploadPartCopy that made part.
func copyPartOutput(
	part *Part, upload *MultipartUpload, source *copySource, customer customerKey,
) *s3api.UploadPartCopyOutput {
	result := &s3api.CopyPartResult{ETag: new(quoted(part.ETag)), LastModified: timeOf(part.LastModified.Time)}
	c := partChecksums(part)
	result.ChecksumCRC32, result.ChecksumCRC32C, result.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
	result.ChecksumSHA1, result.ChecksumSHA256 = c.sha1, c.sha256

	out := &s3api.UploadPartCopyOutput{CopyPartResult: result}
	out.ServerSideEncryption, out.SSEKMSKeyId, out.BucketKeyEnabled = uploadEncryption(upload)
	out.SSECustomerAlgorithm, out.SSECustomerKeyMD5 = customer.echo()

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

	err = checkReadableSource(v)
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
	path, query := splitCopySource(header)
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

// paramSchemes are the URL schemes whose last path segment may carry ";params",
// which urlparse splits from the path (urllib.parse.uses_params).
// The empty scheme has them too.
var paramSchemes = strings.Fields("ftp hdl prospero http imap https shttp rtsp rtspu sip sips mms sftp tel")

// urlUnsafe holds the bytes urlsplit removes from a URL wherever they are.
var urlUnsafe = strings.NewReplacer("\t", "", "\r", "", "\n", "")

// splitCopySource returns the path and the query of a copy source as Python's
// urllib.parse.urlparse reads it, which is how LocalStack reads it
// (s3/utils.py:162). AWS takes a whole URL too (test_response_structure is
// aws.validated). In order it drops leading control characters and spaces, a
// scheme (a letter, then letters, digits, "+", "-" or "." up to the first
// ":"), a "//host" after it, a "#fragment", which hides a "?versionId" that
// follows it, the "?query", and the ";params" of the last path segment for
// the schemes that have them.
func splitCopySource(source string) (string, string) {
	source = urlUnsafe.Replace(strings.TrimLeftFunc(source, func(r rune) bool { return r <= ' ' }))

	scheme := ""

	i := strings.IndexByte(source, ':')
	if i > 0 && isLetter(rune(source[0])) && !strings.ContainsFunc(source[:i], notSchemeChar) {
		scheme, source = strings.ToLower(source[:i]), source[i+1:]
	}

	if rest, found := strings.CutPrefix(source, "//"); found {
		source = ""

		if end := strings.IndexAny(rest, "/?#"); end >= 0 {
			source = rest[end:]
		}
	}

	source, _, _ = strings.Cut(source, "#")
	path, query, _ := strings.Cut(source, "?")

	if scheme == "" || slices.Contains(paramSchemes, scheme) {
		start := max(strings.LastIndexByte(path, '/'), 0)

		if i := strings.IndexByte(path[start:], ';'); i >= 0 {
			path = path[:start+i]
		}
	}

	return path, query
}

func notSchemeChar(r rune) bool { return !isSchemeChar(r) }

func isLetter(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
}

func isSchemeChar(r rune) bool {
	return isLetter(r) || '0' <= r && r <= '9' || r == '+' || r == '-' || r == '.'
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
