package ssm

import (
	"context"
	"maps"
	"slices"
	"strconv"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
)

const (
	resourceParameter = "Parameter"
	resourceWindow    = "MaintenanceWindow"
)

// tagsOf returns the tags map of the resource a request names, moto's
// _validate_resource_type_and_id. A Parameter is found by the GetParameter
// rules, selectors included; a type gostack keeps none of is a resource that
// does not exist. A parameter ARN names the parameter it ends in. The caller
// holds the lock.
func (s *State) tagsOf(resourceType, id *string) (*map[string]string, error) {
	var tags *map[string]string

	switch deref(resourceType) {
	case resourceParameter:
		name := deref(id)
		if _, isARN := arnResource(name); isARN {
			name = normalizeName(name)
		}

		v, err := s.selectVersion(name)
		if err != nil {
			return nil, err
		}

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

// checkTags is the check of the API model's Tag shape: Key and Value are
// required, and a Key has at least one character. LocalStack's own answer to a
// tag with a member missing is a crash (a 500 that names the member), and
// there is no tag with no key for a listing to show, so the request is
// refused whole, before anything changes. Every failure is reported, in the
// order of the request.
func checkTags(tags []ssmapi.Tag) error {
	var (
		report constraint.Report
		root   *constraint.Path
	)

	for i := range tags {
		at := root.Item("tags", i)

		switch key := tags[i].Key; {
		case key == nil:
			report.Null(&at, "key")
		case *key == "":
			report.Value(&at, "key", "", minLengthOne)
		default:
			// A key of one character or more passes.
		}

		if tags[i].Value == nil {
			report.Null(&at, "value")
		}
	}

	return validationOf(&report)
}

// checkTagKeys is the check of the model's KeyList: a key has at least one
// character. The model does not name the path of an element of a list of
// text; it is spelled as the Path of the constraint package spells a list
// element, "tagKeys.2.member".
func checkTagKeys(keys []string) error {
	var report constraint.Report

	for i, key := range keys {
		if key == "" {
			report.Value(nil, "tagKeys."+strconv.Itoa(i+1)+".member", "", minLengthOne)
		}
	}

	return validationOf(&report)
}

// minLengthOne is the rule of a text the model bounds at one character.
const minLengthOne = "Member must have length greater than or equal to 1"

// validationOf is the ValidationException for the failures in report, or nil
// when it holds none.
func validationOf(report *constraint.Report) error {
	if !report.Failed() {
		return nil
	}

	return ssmapi.NewValidationException(report.Message())
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

	err = checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.lock()
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

	err = checkTagKeys(in.TagKeys)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.lock()
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
