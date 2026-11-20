package ssm_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

const (
	tagParam     = "/tags/p"
	fieldTagKeys = "TagKeys"

	// The rules of the Tag shape, as the constraint message spells them.
	mustNotBeNull = " failed to satisfy constraint: Member must not be null"
	atLeastOne    = " failed to satisfy constraint: Member must have length greater than or equal to 1"

	nullTagKey     = "1 validation error detected: Value null at 'tags.1.member.key'" + mustNotBeNull
	nullTagValue   = "1 validation error detected: Value null at 'tags.1.member.value'" + mustNotBeNull
	emptyTagKey    = "1 validation error detected: Value '' at 'tags.1.member.key'" + atLeastOne
	emptyTagKeyTwo = "1 validation error detected: Value '' at 'tags.2.member.key'" + atLeastOne
	bothTagMembers = "2 validation errors detected: Value null at 'tags.1.member.key'" + mustNotBeNull +
		"; Value null at 'tags.1.member.value'" + mustNotBeNull
	emptyRemovedKey = "1 validation error detected: Value '' at 'tagKeys.2.member'" + atLeastOne
)

// Test_aTagNeedsAKeyAndAValue: the Tag shape of the API model requires Key and
// Value, and bounds Key at one character or more. A tag with no key was stored
// under the empty key and listed back as {"Key":"","Value":"v"}, a tag AWS
// cannot hold. PutParameter and AddTagsToResource answer 400 and change
// nothing, and so does RemoveTagsFromResource for a key shorter than one.
func Test_aTagNeedsAKeyAndAValue(t *testing.T) {
	c := newClient(t, ssm.New())
	url := c.url

	c.ok(kPutParameter, obj{kName: tagParam, kValue: "1", kType: kString})

	good := obj{fieldKey: "a", kValue: "1"}

	for _, tt := range []struct {
		name string
		tags []obj
		want string
	}{
		{"no key", []obj{{kValue: "v"}}, nullTagKey},
		{"no value", []obj{{fieldKey: "k"}}, nullTagValue},
		{"an empty key", []obj{{fieldKey: "", kValue: "v"}}, emptyTagKey},
		{"neither member", []obj{{}}, bothTagMembers},
		{"the second tag", []obj{good, {fieldKey: "", kValue: "v"}}, emptyTagKeyTwo},
	} {
		t.Run("AddTagsToResource/"+tt.name, func(t *testing.T) {
			sub := &client{t: t, url: url}

			msg := sub.fails(kAddTagsToResource, obj{
				kResourceType: resourceParameter, kResourceID: tagParam, kTags: tt.tags,
			}, kValidationException)
			require.Equal(t, tt.want, msg)
		})

		t.Run("PutParameter/"+tt.name, func(t *testing.T) {
			sub := &client{t: t, url: url}

			msg := sub.fails(kPutParameter, obj{
				kName: "/tags/new", kValue: "1", kType: kString, kTags: tt.tags,
			}, kValidationException)
			require.Equal(t, tt.want, msg)
		})

		t.Run("CreateMaintenanceWindow/"+tt.name, func(t *testing.T) {
			sub := &client{t: t, url: url}

			msg := sub.fails(kCreateMaintenanceWindow, obj{
				kName: "w", kSchedule: "cron(0 0 * * ? *)", kDuration: 2, kCutoff: 1, kAllowUnassociated: false,
				kTags: tt.tags,
			}, kValidationException)
			require.Equal(t, tt.want, msg)
			require.Empty(t, list(sub.ok("DescribeMaintenanceWindows", obj{}), "WindowIdentities"))
		})
	}

	// None of the refused calls stored anything: not the tags of the one that
	// also had a good tag, and not the parameter a put would have made.
	listed := c.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: tagParam})
	require.Empty(t, list(listed, kTagList))
	c.fails(kGetParameter, obj{kName: "/tags/new"}, kParameterNotFound)

	msg := c.fails(kRemoveTagsFromResource, obj{
		kResourceType: resourceParameter, kResourceID: tagParam, fieldTagKeys: []string{"a", ""},
	}, kValidationException)
	require.Equal(t, emptyRemovedKey, msg)

	// A value may be empty: the model's minimum for it is 0.
	c.ok(kAddTagsToResource, obj{
		kResourceType: resourceParameter, kResourceID: tagParam, kTags: []obj{{fieldKey: "k", kValue: ""}},
	})
	require.Equal(t, []any{map[string]any{fieldKey: "k", kValue: ""}},
		list(c.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: tagParam}), kTagList))
}
