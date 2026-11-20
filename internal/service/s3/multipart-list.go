package s3

import (
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
		ETag: new(quoted(part.ETag)), LastModified: timeOf(part.LastModified.Time), PartNumber: &part.Number,
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
		encode = quote
	}

	keyMarker, uploadIDMarker := deref(in.KeyMarker), deref(in.UploadIdMarker)
	prefix, delimiter := encode(deref(in.Prefix)), encode(deref(in.Delimiter))

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
		encode: encode, max: int(maxUploads), raw: encoding == "",
	}

	// The page is filled under the read lock, which it holds for the page and no
	// longer: the uploads it names are read in place, from the index.
	b.mu.RLock()
	defer b.mu.RUnlock()

	if keyMarker != "" && uploadIDMarker != "" {
		marked := b.Uploads[uploadIDMarker]
		if marked == nil || encode(marked.Key) != keyMarker {
			return nil, invalidArgument("Invalid uploadId marker", "upload-id-marker", &uploadIDMarker)
		}
	}

	page.fill(out, b.uploadOrder[page.start(b.uploadOrder):])

	return out, nil
}

// uploadPage is one page of ListMultipartUploads being filled.
type uploadPage struct {
	keyMarker, uploadIDMarker string
	prefix, delimiter         string
	encode                    func(string) string
	max                       int
	// raw is set when keys are compared as they are stored, so the uploads, in
	// key order, are also in the order of the keys the page compares.
	raw bool
}

// start is the position in uploads, which are in key order, of the first one the
// page can list: a key below the marker or the prefix is neither listed nor
// the marker's. Under an encoding the order of the compared keys is not the
// order of the stored ones, and the page starts at the first upload.
func (pg uploadPage) start(uploads []*MultipartUpload) int {
	if !pg.raw {
		return 0
	}

	i, _ := slices.BinarySearchFunc(uploads, max(pg.keyMarker, pg.prefix), func(u *MultipartUpload, key string) int {
		return strings.Compare(u.Key, key)
	})

	return i
}

// fill lists the page from uploads, in order by key, then by start time. The
// caller holds the bucket's lock.
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
			// Past the prefix, no key that follows has it.
			if pg.raw && key > pg.prefix {
				break
			}

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
				UploadId: &upload.ID, Key: &upload.Key, Initiated: timeOf(upload.Initiated.Time),
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
