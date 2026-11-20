package s3

import (
	"context"
	"slices"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	attributeETag         = "ETag"
	attributeStorageClass = "StorageClass"
	attributeObjectSize   = "ObjectSize"
	attributeObjectParts  = "ObjectParts"
	attributeChecksum     = "Checksum"
)

// GetObjectAttributes answers the attributes of the object that the request
// names in x-amz-object-attributes: ETag, StorageClass, ObjectSize, Checksum
// and ObjectParts. The parts of a multipart object are listed only when its
// checksum is COMPOSITE; a FULL_OBJECT one reports their count alone.
func (p *Provider) GetObjectAttributes(
	ctx context.Context, in *s3api.GetObjectAttributesInput,
) (*s3api.GetObjectAttributesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	b, err := p.bucket(rc.AccountID, deref(in.Bucket))
	if err != nil {
		return nil, err
	}

	if len(in.ObjectAttributes) == 0 {
		return nil, invalidArgument(
			"x-amz-object-attributes header specifying the attributes to be retrieved is either missing or empty",
			"x-amz-object-attributes", nil)
	}

	var out *s3api.GetObjectAttributesOutput

	_, err = readableIn(b, deref(in.Key), in.VersionId, func(v *Version) error {
		keyErr := checkCustomerRead(v.SSECustomerKeyMD5,
			customerKeyOf(in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5), accessDenied)
		if keyErr != nil {
			return keyErr
		}

		out = attributesOf(v, in)

		if b.Versioning != nil && deref(b.Versioning.Status) != "" {
			out.VersionId = &v.ID
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// attributesOf selects from the version the attributes the request asks for.
func attributesOf(v *Version, in *s3api.GetObjectAttributesInput) *s3api.GetObjectAttributesOutput {
	out := &s3api.GetObjectAttributesOutput{LastModified: &protocol.Time{Time: v.LastModified.Time}}
	asked := func(name string) bool { return slices.Contains(in.ObjectAttributes, name) }

	if asked(attributeETag) {
		out.ETag = new(v.ETag)
	}

	if asked(attributeStorageClass) {
		out.StorageClass = new(v.StorageClass)
	}

	if asked(attributeObjectSize) {
		out.ObjectSize = new(v.Size)
	}

	if asked(attributeChecksum) && v.ChecksumAlgorithm != "" {
		out.Checksum = checksumOfAttributes(v)
	}

	if asked(attributeObjectParts) && len(v.Parts) > 0 {
		out.ObjectParts = partsOfAttributes(v, in)
	}

	return out
}

// checksumOfAttributes is the Checksum of GetObjectAttributes. A multipart
// object's value has the part count after a dash; the attribute leaves it out.
func checksumOfAttributes(v *Version) *s3api.Checksum {
	whole := *v
	if len(v.Parts) > 0 {
		whole.ChecksumValue, _, _ = strings.Cut(v.ChecksumValue, "-")
	}

	c := checksumsOf(&whole)

	return &s3api.Checksum{
		ChecksumCRC32: c.crc32, ChecksumCRC32C: c.crc32c, ChecksumCRC64NVME: c.crc64nvme,
		ChecksumSHA1: c.sha1, ChecksumSHA256: c.sha256, ChecksumType: c.kind,
	}
}

// partsOfAttributes is the ObjectParts of GetObjectAttributes: the part count,
// and for a COMPOSITE checksum a page of the parts as ListParts would give it.
func partsOfAttributes(v *Version, in *s3api.GetObjectAttributesInput) *s3api.GetObjectAttributesParts {
	total := int32(len(v.Parts)) //nolint:gosec // a multipart object has at most 10000 parts.
	if v.ChecksumType == checksumFullObject {
		return &s3api.GetObjectAttributesParts{TotalPartsCount: &total}
	}

	marker := int32(0)
	if in.PartNumberMarker != nil {
		marker = *in.PartNumberMarker
	}

	maxParts := int32(defaultMaxParts)
	if in.MaxParts != nil && *in.MaxParts > 0 {
		maxParts = *in.MaxParts
	}

	out := &s3api.GetObjectAttributesParts{
		TotalPartsCount: &total, IsTruncated: new(false), MaxParts: &maxParts,
		PartNumberMarker: &marker, NextPartNumberMarker: new(int32(0)),
	}

	lastNumber := v.Parts[len(v.Parts)-1].Number

	for _, part := range v.Parts {
		if part.Number <= marker {
			continue
		}

		out.Parts = append(out.Parts, objectPartOf(v.ChecksumAlgorithm, part))
		out.NextPartNumberMarker = new(part.Number)

		full := len(out.Parts) >= int(maxParts)
		if full && part.Number != lastNumber {
			out.IsTruncated = new(true)

			break
		}
	}

	return out
}

// objectPartOf is a part as GetObjectAttributes lists it, with its checksum in
// the member of the object's algorithm.
func objectPartOf(algorithm string, part ObjectPart) s3api.ObjectPart {
	listed := s3api.ObjectPart{PartNumber: new(part.Number), Size: new(part.Size)}

	if part.Checksum == "" {
		return listed
	}

	switch algorithm {
	case algorithmCRC32:
		listed.ChecksumCRC32 = &part.Checksum
	case algorithmCRC32C:
		listed.ChecksumCRC32C = &part.Checksum
	case algorithmSHA1:
		listed.ChecksumSHA1 = &part.Checksum
	case algorithmSHA256:
		listed.ChecksumSHA256 = &part.Checksum
	default:
		listed.ChecksumCRC64NVME = &part.Checksum
	}

	return listed
}
