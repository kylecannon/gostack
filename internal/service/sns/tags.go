package sns

import (
	"context"
	"maps"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/sns"
)

// TagResource adds tags to a resource, replacing the value of a key it has,
// as LocalStack's tag_resource. Like LocalStack, it does not check that the
// resource exists.
func (p *Provider) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	tags := tagsOf(in.Tags)

	keys := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		keys[t.key] = struct{}{}
	}

	if len(keys) < len(tags) {
		return nil, api.NewInvalidParameterException("Invalid parameter: Duplicated keys are not allowed.")
	}

	p.accounts.Get(rc.AccountID, accountRegion).update(deref(in.ResourceArn), tags)

	return &api.TagResourceOutput{}, nil
}

// UntagResource removes tag keys from a resource. A key it does not have is
// no error.
func (p *Provider) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	p.accounts.Get(rc.AccountID, accountRegion).untag(deref(in.ResourceArn), in.TagKeys)

	return &api.UntagResourceOutput{}, nil
}

// ListTagsForResource lists a resource's tags in the order their keys were
// first set. A resource with none, or none known, has an empty list.
func (p *Provider) ListTagsForResource(
	ctx context.Context, in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	tags := p.accounts.Get(rc.AccountID, accountRegion).list(deref(in.ResourceArn))

	out := make([]api.Tag, len(tags))
	for i, t := range tags {
		out[i] = api.Tag{Key: new(t.key), Value: new(t.value)}
	}

	return &api.ListTagsForResourceOutput{Tags: out}, nil
}

// tagsOf reads the tags of a request. A missing key or value is empty.
func tagsOf(in []api.Tag) []tag {
	tags := make([]tag, len(in))
	for i, t := range in {
		tags[i] = tag{key: deref(t.Key), value: deref(t.Value)}
	}

	return tags
}

// update sets each of tags on the resource arn, in order: a key it has keeps
// its place and takes the new value, a new key goes last.
func (a *AccountState) update(arn string, tags []tag) {
	a.mu.Lock()
	defer a.mu.Unlock()

	stored := a.tags[arn]

	for _, t := range tags {
		if i := slices.IndexFunc(stored, func(s tag) bool { return s.key == t.key }); i >= 0 {
			stored[i].value = t.value

			continue
		}

		stored = append(stored, t)
	}

	a.tags[arn] = stored
}

// untag removes keys from the resource arn.
func (a *AccountState) untag(arn string, keys []string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	stored, ok := a.tags[arn]
	if !ok {
		return
	}

	a.tags[arn] = slices.DeleteFunc(stored, func(t tag) bool { return slices.Contains(keys, t.key) })
}

// list returns a copy of the tags of the resource arn.
func (a *AccountState) list(arn string) []tag {
	a.mu.Lock()
	defer a.mu.Unlock()

	return slices.Clone(a.tags[arn])
}

// covers reports whether the resource arn has every one of tags, key and value,
// as LocalStack's _check_matching_tags asks before it answers a repeated
// CreateTopic with the existing topic.
func (a *AccountState) covers(arn string, tags []tag) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	stored := a.tags[arn]

	for _, t := range tags {
		if !slices.Contains(stored, t) {
			return false
		}
	}

	return true
}

// drop removes every tag of the resource arn, for a resource that is deleted.
func (a *AccountState) drop(arn string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.tags, arn)
}

// dropRegion removes the tags of every resource in region, for a reset of that
// region's state.
func (a *AccountState) dropRegion(region string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	maps.DeleteFunc(a.tags, func(arn string, _ []tag) bool {
		parts, ok := parseARN(arn)

		return ok && parts.region == region
	})
}
