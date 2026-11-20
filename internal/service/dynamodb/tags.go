package dynamodb

import (
	"context"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// Tags live in the account's state by ARN, as LocalStack's TABLE_TAGS does:
// any ARN can be tagged, from any region, whether a table has it or not.
// CreateTable sets a table's tags and DeleteTable drops them.

// TagResource adds tags to an ARN, replacing the value of a key it has.
func (p *Provider) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	arn := deref(in.ResourceArn)
	a.tags[arn] = mergeTags(a.tags[arn], in.Tags)

	return &api.TagResourceOutput{}, nil
}

// UntagResource removes the named tag keys from an ARN.
func (p *Provider) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	arn := deref(in.ResourceArn)

	tags, ok := a.tags[arn]
	if ok {
		a.tags[arn] = slices.DeleteFunc(slices.Clone(tags), func(tag api.Tag) bool {
			return slices.Contains(in.TagKeys, deref(tag.Key))
		})
	}

	return &api.UntagResourceOutput{}, nil
}

// ListTagsOfResource lists an ARN's tags in the order their keys were first
// set, all on one page.
func (p *Provider) ListTagsOfResource(
	ctx context.Context, in *api.ListTagsOfResourceInput,
) (*api.ListTagsOfResourceOutput, error) {
	a := p.account(p.caller(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	tags := slices.Clone(a.tags[deref(in.ResourceArn)])
	if tags == nil {
		tags = []api.Tag{}
	}

	return &api.ListTagsOfResourceOutput{Tags: tags}, nil
}

// setTags replaces the tags of arn, as CreateTable does; no tags forget it.
func (a *accountState) setTags(arn string, tags []api.Tag) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(tags) == 0 {
		delete(a.tags, arn)

		return
	}

	a.tags[arn] = mergeTags(nil, tags)
}

// dropTags forgets the tags of the given ARNs, as DeleteTable does.
func (a *accountState) dropTags(arns ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, arn := range arns {
		delete(a.tags, arn)
	}
}

// mergeTags returns tags with more applied in order: a new key is appended, a
// key it has takes the new value in place. tags is not changed.
func mergeTags(tags, more []api.Tag) []api.Tag {
	out := slices.Clone(tags)

	for _, tag := range more {
		key := deref(tag.Key)

		i := slices.IndexFunc(out, func(t api.Tag) bool { return deref(t.Key) == key })
		if i >= 0 {
			out[i] = api.Tag{Key: new(key), Value: tag.Value}
		} else {
			out = append(out, api.Tag{Key: new(key), Value: tag.Value})
		}
	}

	return out
}

// deref is the string s points at, or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
