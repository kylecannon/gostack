package ssm_test

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nullParam = "/n/p1"

// A json null in a list is no entry (awsjson.nulls). The LocalStack image drops
// it for DeleteParameters, UnlabelParameterVersion and RemoveTagsFromResource,
// answering 200 as for the list without it, and gostack answers the same. The
// image answers 500 (a TypeError) for the null in GetParameters,
// LabelParameterVersion, GetParametersByPath and DescribeParameters; gostack
// answers as for the list without it (docs/compat.md, "Null elements").
func Test_a_null_in_a_list_is_no_entry(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, obj{kName: nullParam, kValue: kValueX, kType: kString})
	resource := obj{kResourceType: resourceParameter, kResourceID: nullParam}
	c.ok("AddTagsToResource", obj{
		kResourceType: resourceParameter, kResourceID: nullParam, "Tags": []obj{{"Key": "a", "Value": "b"}},
	})

	t.Run("the image drops it", func(t *testing.T) {
		out := c.ok("RemoveTagsFromResource", obj{
			kResourceType: resourceParameter, kResourceID: nullParam, "TagKeys": []any{nil},
		})
		assert.Empty(t, out)

		tags := list(c.ok("ListTagsForResource", resource), "TagList")
		require.Len(t, tags, 1, "a null tag key removes no tag")

		out = c.ok("UnlabelParameterVersion", obj{kName: nullParam, fieldVersion: 1, fieldLabels: []any{nil}})
		assert.Empty(t, list(out, "RemovedLabels"))
		assert.Empty(t, list(out, "InvalidLabels"))

		out = c.ok("DeleteParameters", obj{"Names": []any{nil, "/n/missing"}})
		assert.Empty(t, list(out, "DeletedParameters"))
		assert.Equal(t, []any{"/n/missing"}, list(out, "InvalidParameters"), "only the name sent is invalid")
	})

	t.Run("the image answers 500", func(t *testing.T) {
		got := c.ok("GetParameters", obj{"Names": []any{nullParam, nil}})
		assert.Len(t, list(got, "Parameters"), 1)
		assert.Empty(t, list(got, "InvalidParameters"))

		label := c.ok(kLabelParameterVersion, obj{kName: nullParam, fieldVersion: 1, fieldLabels: []any{"one", nil}})
		assert.Empty(t, list(label, "InvalidLabels"))

		filter := []obj{{fieldKey: "Type", kValues: []any{nil}}}
		status, _ := c.call(kGetParametersByPath, obj{kPath: "/n", fieldFilters: filter})
		assert.NotEqual(t, http.StatusInternalServerError, status)

		filter = []obj{{fieldKey: kName, kValues: []any{nullParam, nil}}}
		status, _ = c.call("DescribeParameters", obj{fieldFilters: filter})
		assert.Equal(t, http.StatusOK, status)
	})
}
