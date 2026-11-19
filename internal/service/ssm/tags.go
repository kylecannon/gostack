package ssm

import (
	"context"
	"maps"
	"slices"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
)

const (
	resourceParameter = "Parameter"
	resourceWindow    = "MaintenanceWindow"
)

// tagsOf returns the tags map of the resource a request names, moto's
// _validate_resource_type_and_id. A Parameter is found by the GetParameter
// rules, selectors included; a type gostack keeps none of is a resource that
// does not exist. The caller holds the lock.
func (s *State) tagsOf(resourceType, id *string) (*map[string]string, error) {
	var tags *map[string]string

	switch deref(resourceType) {
	case resourceParameter:
		v, _ := s.selectVersion(deref(id))
		if v != nil {
			tags = &s.lookup(v.Name).tags
		}
	case resourceWindow:
		if w := s.windows[deref(id)]; w != nil {
			tags = &w.Tags
		}
	case "Document", "ManagedInstance", "PatchBaseline", "OpsItem", "OpsMetadata":
	default:
		return nil, ssmapi.NewInvalidResourceType("Invalid Resource Type")
	}

	if tags == nil {
		return nil, ssmapi.NewInvalidResourceId("Invalid Resource Id")
	}

	return tags, nil
}

// AddTagsToResource adds tags to a parameter or a maintenance window, replacing
// the value of a key it already has.
func (p *Provider) AddTagsToResource(
	ctx context.Context, in *ssmapi.AddTagsToResourceInput,
) (*ssmapi.AddTagsToResourceOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	tags, err := s.tagsOf(in.ResourceType, in.ResourceId)
	if err != nil {
		return nil, err
	}

	*tags = mergeTags(*tags, in.Tags)

	return &ssmapi.AddTagsToResourceOutput{}, nil
}

// RemoveTagsFromResource removes tags by key; a key the resource does not have
// is ignored.
func (p *Provider) RemoveTagsFromResource(
	ctx context.Context, in *ssmapi.RemoveTagsFromResourceInput,
) (*ssmapi.RemoveTagsFromResourceOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	tags, err := s.tagsOf(in.ResourceType, in.ResourceId)
	if err != nil {
		return nil, err
	}

	for _, key := range in.TagKeys {
		delete(*tags, key)
	}

	return &ssmapi.RemoveTagsFromResourceOutput{}, nil
}

// ListTagsForResource lists the tags of a parameter or a maintenance window by
// key.
func (p *Provider) ListTagsForResource(
	ctx context.Context, in *ssmapi.ListTagsForResourceInput,
) (*ssmapi.ListTagsForResourceOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	ref, err := s.tagsOf(in.ResourceType, in.ResourceId)
	if err != nil {
		return nil, err
	}

	tags := *ref
	out := &ssmapi.ListTagsForResourceOutput{TagList: make([]ssmapi.Tag, 0, len(tags))}
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		out.TagList = append(out.TagList, ssmapi.Tag{Key: new(key), Value: new(tags[key])})
	}

	return out, nil
}
