package s3

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// The tag limits: S3 allows 50 tags on a bucket and 10 on an object, keys up to
// 128 characters and values up to 256.
const (
	maxBucketTags  = 50
	maxObjectTags  = 10
	maxTagKeyLen   = 128
	maxTagValueLen = 256

	tagMessageDuplicate = "Cannot provide multiple Tags with the same key"
)

// tagKind says which message a reserved aws: key gets.
type tagKind int

const (
	tagsOfBucket tagKind = iota
	tagsOfObject
	tagsOfNewBucket
)

// tagPattern is LocalStack's TAG_REGEX: word characters, whitespace and
// . : / = + - @, which RE2 spells with Unicode classes. Its \s is Python 3's
// for a str, every Unicode space (Py_UNICODE_ISSPACE: str.isspace), where RE2's
// is ASCII only: \t \n \v \f \r, the separators \x1c-\x1f, space, NEL, no-break
// space, U+1680, U+2000-U+200A, U+2028, U+2029, U+202F, U+205F and U+3000.
var tagPattern = regexp.MustCompile(
	`^[\p{L}\p{N}_\t\n\x0b\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}` +
		`.:/=+\-@]*$`)

func invalidTag(message string, key, value *string) error {
	return (&s3api.InvalidTag{TagKey: key, TagValue: value}).Err(message)
}

// validateTags is LocalStack's validate_tag_set, in its order: a tag without a
// key or value is malformed XML, then duplicate keys, the reserved aws: prefix,
// the characters of the key and of the value, then the limits.
func validateTags(tags []s3api.Tag, kind tagKind) error {
	seen := make(map[string]struct{}, len(tags))

	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return malformedXMLError()
		}

		key, value := *tag.Key, *tag.Value

		if _, duplicate := seen[key]; duplicate {
			return invalidTag(tagMessageDuplicate, &key, nil)
		}

		if strings.HasPrefix(key, "aws:") {
			return reservedTagKey(key, kind)
		}

		switch {
		case !tagPattern.MatchString(key):
			return invalidTag("The TagKey you have provided is invalid", &key, nil)
		case !tagPattern.MatchString(value):
			return invalidTag("The TagValue you have provided is invalid", &key, &value)
		case utf8.RuneCountInString(key) > maxTagKeyLen:
			return invalidTag("The TagKey you have provided is too long, max 128, got "+itoa(utf8.RuneCountInString(key)),
				&key, nil)
		case utf8.RuneCountInString(value) > maxTagValueLen:
			return invalidTag("The TagValue you have provided is too long, max 256, got "+
				itoa(utf8.RuneCountInString(value)), &key, &value)
		}

		seen[key] = struct{}{}
	}

	if kind == tagsOfObject && len(tags) > maxObjectTags {
		return invalidTag("Object tags cannot be greater than 10", nil, nil)
	}

	if kind != tagsOfObject && len(tags) > maxBucketTags {
		return invalidTag("Bucket tag count cannot be greater than 50", nil, nil)
	}

	return nil
}

func reservedTagKey(key string, kind tagKind) error {
	switch kind {
	case tagsOfObject:
		return invalidTag("Your TagKey cannot be prefixed with aws:", &key, nil)
	case tagsOfNewBucket:
		// S3 leaves the key out of this one.
		return invalidTag(`User-defined tag keys can't start with "aws:". This prefix is reserved for system `+
			`tags. Remove "aws:" from your tag keys and try again.`, nil, nil)
	default:
		return invalidTag("System tags cannot be added/updated by requester", &key, nil)
	}
}

// parseTaggingHeader reads the x-amz-tagging header: URL query parameters, a key
// at most once, in the order they appear. A parameter with no value has an empty one.
func parseTaggingHeader(header string) ([]s3api.Tag, error) {
	var tags []s3api.Tag

	seen := make(map[string]struct{})

	for pair := range strings.SplitSeq(header, "&") {
		if pair == "" {
			continue
		}

		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, value := queryUnescape(rawKey), queryUnescape(rawValue)

		_, duplicate := seen[key]
		if duplicate || !tagPattern.MatchString(key) || !tagPattern.MatchString(value) || strings.HasPrefix(key, "aws:") {
			return nil, invalidArgument("The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL "+
				"query parameters without tag name duplicates.", "x-amz-tagging", &header)
		}

		seen[key] = struct{}{}
		tags = append(tags, s3api.Tag{Key: &key, Value: &value})
	}

	return tags, nil
}

// queryUnescape decodes a query parameter, leaving it as it is when it does
// not decode, as Python's parse_qs does.
func queryUnescape(s string) string {
	decoded, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}

	return decoded
}

// tagCountOf is the x-amz-tagging-count of an object: absent for none.
func tagCountOf(v *Version) *int32 {
	if len(v.Tags) == 0 {
		return nil
	}

	return new(int32(len(v.Tags))) //nolint:gosec // at most ten tags.
}

// tagMap returns the tags as the lifecycle filter reads them.
func tagMap(tags []s3api.Tag) map[string]string {
	m := make(map[string]string, len(tags))

	for _, tag := range tags {
		m[deref(tag.Key)] = deref(tag.Value)
	}

	return m
}

// GetBucketTagging returns the bucket's tags. A bucket with none answers
// NoSuchTagSet.
func (p *Provider) GetBucketTagging(
	ctx context.Context, in *s3api.GetBucketTaggingInput,
) (*s3api.GetBucketTaggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketTaggingOutput{}

	err = b.readConfig(func() error {
		if len(b.Tags) == 0 {
			name := b.Name

			return (&s3api.NoSuchTagSet{BucketName: &name}).Err("The TagSet does not exist")
		}

		out.TagSet = append([]s3api.Tag(nil), b.Tags...)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketTagging replaces the bucket's tags. An empty tag set is the same as
// deleting them.
func (p *Provider) PutBucketTagging(
	ctx context.Context, in *s3api.PutBucketTaggingInput,
) (*s3api.PutBucketTaggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if in.Tagging == nil {
		return nil, malformedXMLError()
	}

	err = validateTags(in.Tagging.TagSet, tagsOfBucket)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Tags = in.Tagging.TagSet

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketTaggingOutput{}, nil
}

// DeleteBucketTagging removes the bucket's tags; deleting none succeeds.
func (p *Provider) DeleteBucketTagging(
	ctx context.Context, in *s3api.DeleteBucketTaggingInput,
) (*s3api.DeleteBucketTaggingOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Tags = nil

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.DeleteBucketTaggingOutput{}, nil
}
