package kms

import (
	"context"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

const (
	maxTags          = 50
	maxTagKeyLen     = 128
	maxTagValueLen   = 256
	defaultTagsLimit = 50
)

// TagResource sets tags on a key that is enabled or disabled.
func (p *Provider) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowEnabled|allowDisabled)
	if err != nil {
		return nil, err
	}

	if len(in.Tags) == 0 {
		return &api.TagResourceOutput{}, nil
	}

	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	k.setTags(tags)

	return &api.TagResourceOutput{}, nil
}

// UntagResource removes tags from a key that is enabled or disabled.
func (p *Provider) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowEnabled|allowDisabled)
	if err != nil {
		return nil, err
	}

	k.removeTags(in.TagKeys)

	return &api.UntagResourceOutput{}, nil
}

// ListResourceTags pages the tags of a key in any state. The marker is the
// key of the first tag of the next page.
func (p *Provider) ListResourceTags(
	ctx context.Context, in *api.ListResourceTagsInput,
) (*api.ListResourceTagsOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, err := p.localKey(rc, in.KeyId, allowAny)
	if err != nil {
		return nil, err
	}

	tags, next := page(k.tagList(), func(t tag) string { return t.Key }, in.Marker, in.Limit, defaultTagsLimit)

	// Tags is [] for a key with none: clients read it without a check.
	out := &api.ListResourceTagsOutput{Tags: make([]api.Tag, 0, len(tags)), Truncated: new(next != "")}
	for _, t := range tags {
		out.Tags = append(out.Tags, api.Tag{TagKey: new(t.Key), TagValue: new(t.Value)})
	}

	if next != "" {
		out.NextMarker = new(next)
	}

	return out, nil
}

// checkTags is validate_and_filter_tags: duplicate keys, then more than 50
// tags, then each tag in order (its key's length, its value's length, the aws:
// prefix), the first failure answered alone. The _custom_key_material_ tag is
// neither checked nor kept; _custom_id_ is both, as in LocalStack.
func checkTags(in []api.Tag) ([]tag, error) {
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		seen[deref(t.TagKey)] = true
	}

	if len(seen) < len(in) {
		return nil, api.NewTagException("Duplicate tag keys")
	}

	if len(in) > maxTags {
		return nil, api.NewTagException("Too many tags")
	}

	out := make([]tag, 0, len(in))

	for i, t := range in {
		key, value := deref(t.TagKey), deref(t.TagValue)
		if key == tagCustomMaterial {
			continue
		}

		err := checkTag(i, key, value)
		if err != nil {
			return nil, err
		}

		out = append(out, tag{Key: key, Value: value})
	}

	return out, nil
}

// checkTag is validate_tag for the tag at index i of the request.
func checkTag(i int, key, value string) error {
	var root *constraint.Path

	at := root.Item("tags", i)

	switch {
	case constraint.Length(key) > maxTagKeyLen:
		var r constraint.Report

		r.Value(&at, "tagKey", key, "Member must have length less than or equal to 128")

		return validation(r.Message())
	case constraint.Length(value) > maxTagValueLen:
		var r constraint.Report

		r.Value(&at, "tagValue", value, "Member must have length less than or equal to 256")

		return validation(r.Message())
	case strings.HasPrefix(strings.ToLower(key), "aws:"):
		return api.NewTagException("Tags beginning with aws: are reserved")
	default:
		return nil
	}
}
