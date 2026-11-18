package sts

import (
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

// sessionTags returns the tags of the session AssumeRole issues: the transitive
// tags of the calling session, which a role chain passes on, and the tags the
// call passes. The call's tags are checked in LocalStack's order, and only when
// it passes some (a call without tags ignores TransitiveTagKeys):
//
//  1. two tags whose keys are equal without regard to case are refused;
//  2. a tag with the key of an inherited transitive tag is refused, as AWS
//     refuses it;
//  3. a transitive key that no passed tag carries is refused.
func (p *provider) sessionTags(rc *request.Context, in *api.AssumeRoleInput) ([]sessionTag, *awserr.Error) {
	caller, _ := p.sessionOf(rc, accessKeyOf(rc))

	inherited := make(map[string]struct{})
	tags := make([]sessionTag, 0, len(caller.tags)+len(in.Tags))

	for _, tag := range caller.tags {
		if tag.transitive {
			inherited[strings.ToLower(tag.key)] = struct{}{}

			tags = append(tags, tag)
		}
	}

	passed := make(map[string]struct{}, len(in.Tags))

	for _, tag := range in.Tags {
		key := strings.ToLower(deref(tag.Key))
		if _, dup := passed[key]; dup {
			return nil, invalidParameter("Duplicate tag keys found. Please note that Tag keys are case insensitive.")
		}

		passed[key] = struct{}{}
	}

	for key := range passed {
		if _, conflict := inherited[key]; conflict {
			return nil, invalidParameter("One of the specified transitive tag keys can't be set because " +
				"it conflicts with a transitive tag key from the calling session.")
		}
	}

	transitive := make(map[string]struct{}, len(in.TransitiveTagKeys))

	for _, key := range in.TransitiveTagKeys {
		key = strings.ToLower(key)
		if _, ok := passed[key]; !ok && len(passed) > 0 {
			return nil, invalidParameter("The specified transitive tag key must be included in the requested tags.")
		}

		transitive[key] = struct{}{}
	}

	for _, tag := range in.Tags {
		_, isTransitive := transitive[strings.ToLower(deref(tag.Key))]
		tags = append(tags, sessionTag{key: deref(tag.Key), value: deref(tag.Value), transitive: isTransitive})
	}

	return tags, nil
}
