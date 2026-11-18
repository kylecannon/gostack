package s3

import (
	"cmp"
	"context"
	"slices"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

const (
	defaultMaxParts   = 1000
	defaultMaxUploads = 1000
	encodingTypeURL   = "url"
)

// ListParts lists an upload's parts by number, a page at a time.
func (p *Provider) ListParts(ctx context.Context, in *s3api.ListPartsInput) (*s3api.ListPartsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	b.mu.RLock()

	upload := b.Uploads[deref(in.UploadId)]
	if upload == nil || upload.Key != deref(in.Key) {
		b.mu.RUnlock()

		return nil, noSuchUpload(deref(in.UploadId))
	}

	parts := partsOf(upload)
	b.mu.RUnlock()

	marker, maxParts := deref32(in.PartNumberMarker), deref32(in.MaxParts)
	if maxParts == 0 {
		maxParts = defaultMaxParts
	}

	owner, initiator := owner()
	out := &s3api.ListPartsOutput{
		Bucket: in.Bucket, Key: in.Key, UploadId: in.UploadId, Initiator: initiator, Owner: owner,
		StorageClass: &upload.StorageClass, IsTruncated: new(false), MaxParts: &maxParts,
		PartNumberMarker: new(int32(0)), NextPartNumberMarker: new(int32(0)),
		ChecksumAlgorithm: nonEmpty(upload.ChecksumAlgorithm), ChecksumType: nonEmpty(upload.ChecksumType),
	}

	if marker != 0 {
		out.PartNumberMarker = &marker
	}

	for i, part := range parts {
		if part.Number <= marker {
			continue
		}

		out.Parts = append(out.Parts, listedPart(upload, part))
		out.NextPartNumberMarker = &part.Number

		if len(out.Parts) >= int(maxParts) && i != len(parts)-1 {
			out.IsTruncated = new(true)

			break
		}
	}

	return out, nil
}

// listedPart is how ListParts shows a part: with its checksum only when the
// upload has an algorithm.
func listedPart(upload *MultipartUpload, part *Part) s3api.Part {
	listed := s3api.Part{
		ETag: new(quoted(part.ETag)), LastModified: timeOf(part.LastModified), PartNumber: &part.Number,
		Size: &part.Size,
	}

	if upload.ChecksumAlgorithm != "" {
		c := partChecksums(part)
		listed.ChecksumCRC32, listed.ChecksumCRC32C, listed.ChecksumCRC64NVME = c.crc32, c.crc32c, c.crc64nvme
		listed.ChecksumSHA1, listed.ChecksumSHA256 = c.sha1, c.sha256
	}

	return listed
}

// ListMultipartUploads lists a bucket's uploads in progress by key and then
// by start time, a page at a time, rolling keys under a delimiter into common
// prefixes as ListObjectsV2 does (provider.py:2999-3130).
func (p *Provider) ListMultipartUploads(
	ctx context.Context, in *s3api.ListMultipartUploadsInput,
) (*s3api.ListMultipartUploadsOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	encoding := deref(in.EncodingType)
	if in.EncodingType != nil && encoding != encodingTypeURL {
		return nil, invalidArgument("Invalid Encoding Method specified in Request", "encoding-type", in.EncodingType)
	}

	encode := func(s string) string { return s }
	if encoding != "" {
		encode = quoteKey
	}

	keyMarker, uploadIDMarker := deref(in.KeyMarker), deref(in.UploadIdMarker)
	prefix, delimiter := encode(deref(in.Prefix)), encode(deref(in.Delimiter))

	uploads, err := sortedUploads(b, keyMarker, uploadIDMarker, encode)
	if err != nil {
		return nil, err
	}

	maxUploads := deref32(in.MaxUploads)
	if maxUploads == 0 {
		maxUploads = defaultMaxUploads
	}

	out := &s3api.ListMultipartUploadsOutput{
		Bucket: in.Bucket, IsTruncated: new(false), MaxUploads: &maxUploads, KeyMarker: &keyMarker,
		UploadIdMarker: new(""), NextKeyMarker: new(""), NextUploadIdMarker: new(""),
		Delimiter: nonEmpty(delimiter), Prefix: nonEmpty(prefix), EncodingType: nonEmpty(encoding),
	}

	if keyMarker != "" {
		out.UploadIdMarker = &uploadIDMarker
	}

	page := uploadPage{
		keyMarker: keyMarker, uploadIDMarker: uploadIDMarker, prefix: prefix, delimiter: delimiter,
		encode: encode, max: int(maxUploads),
	}
	page.fill(out, uploads)

	return out, nil
}

// sortedUploads returns the bucket's uploads by key, then by start time,
// after checking that an upload-id-marker names an upload of the key-marker.
func sortedUploads(
	b *Bucket, keyMarker, uploadIDMarker string, encode func(string) string,
) ([]*MultipartUpload, error) {
	b.mu.RLock()

	if keyMarker != "" && uploadIDMarker != "" {
		marked := b.Uploads[uploadIDMarker]
		if marked == nil || encode(marked.Key) != keyMarker {
			b.mu.RUnlock()

			return nil, invalidArgument("Invalid uploadId marker", "upload-id-marker", &uploadIDMarker)
		}
	}

	uploads := make([]*MultipartUpload, 0, len(b.Uploads))
	for _, upload := range b.Uploads {
		uploads = append(uploads, upload)
	}

	b.mu.RUnlock()

	slices.SortFunc(uploads, func(x, y *MultipartUpload) int {
		return cmp.Or(strings.Compare(x.Key, y.Key), x.Initiated.Compare(y.Initiated))
	})

	return uploads, nil
}

// uploadPage is one page of ListMultipartUploads being filled.
type uploadPage struct {
	keyMarker, uploadIDMarker string
	prefix, delimiter         string
	encode                    func(string) string
	max                       int
}

func (pg uploadPage) fill(out *s3api.ListMultipartUploadsOutput, uploads []*MultipartUpload) {
	owner, initiator := owner()
	prefixes := make(map[string]bool)
	markerPassed := false
	count := 0

	for i, upload := range uploads {
		key := pg.encode(upload.Key)

		if pg.keyMarker != "" {
			switch {
			case key < pg.keyMarker:
				continue
			case key == pg.keyMarker && (pg.uploadIDMarker == "" || !markerPassed):
				markerPassed = markerPassed || upload.ID == pg.uploadIDMarker

				continue
			default:
			}
		}

		if !strings.HasPrefix(key, pg.prefix) {
			continue
		}

		if rest := strings.TrimPrefix(key, pg.prefix); pg.delimiter != "" && strings.Contains(rest, pg.delimiter) {
			before, _, _ := strings.Cut(rest, pg.delimiter)
			common := pg.prefix + before + pg.delimiter

			if prefixes[common] || strings.HasPrefix(pg.keyMarker, common) && pg.keyMarker != "" {
				continue
			}

			prefixes[common] = true
			out.CommonPrefixes = append(out.CommonPrefixes, s3api.CommonPrefix{Prefix: new(common)})
		} else {
			out.Uploads = append(out.Uploads, s3api.MultipartUpload{
				UploadId: &upload.ID, Key: &upload.Key, Initiated: timeOf(upload.Initiated),
				StorageClass: &upload.StorageClass, Owner: owner, Initiator: initiator,
				ChecksumAlgorithm: nonEmpty(upload.ChecksumAlgorithm), ChecksumType: nonEmpty(upload.ChecksumType),
			})
			out.NextKeyMarker, out.NextUploadIdMarker = &upload.Key, &upload.ID
		}

		count++
		if count >= pg.max && i != len(uploads)-1 {
			out.IsTruncated = new(true)

			break
		}
	}

	slices.SortFunc(out.CommonPrefixes, func(x, y s3api.CommonPrefix) int { return strings.Compare(*x.Prefix, *y.Prefix) })
}
