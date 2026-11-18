package sts

import (
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

// sessionTags returns the tags of the session AssumeRole issues: the transitive
// tags of the calling session, which a role chain passes on, and the tags the
// call passes. A call that passes a tag with the key of an inherited transitive
// tag is refused, as AWS refuses it; tag keys are case insensitive.
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

	transitive := make(map[string]struct{}, len(in.TransitiveTagKeys))
	for _, key := range in.TransitiveTagKeys {
		transitive[strings.ToLower(key)] = struct{}{}
	}

	for _, tag := range in.Tags {
		key := strings.ToLower(deref(tag.Key))
		if _, conflict := inherited[key]; conflict {
			return nil, invalidParameter("One of the specified transitive tag keys can't be set because " +
				"it conflicts with a transitive tag key from the calling session.")
		}

		_, isTransitive := transitive[key]
		tags = append(tags, sessionTag{key: deref(tag.Key), value: deref(tag.Value), transitive: isTransitive})
	}

	return tags, nil
}
