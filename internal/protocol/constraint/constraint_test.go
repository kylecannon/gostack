package constraint_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/stretchr/testify/assert"
)

func Test_Report_Message(t *testing.T) {
	t.Parallel()

	var r constraint.Report

	assert.False(t, r.Failed())

	outer := (*constraint.Path)(nil).Item("entries", 0)
	inner := outer.Entry("attrs", "k")
	field := inner.Field("value")

	r.Null(&field, "dataType")
	assert.True(t, r.Failed())
	assert.Equal(t, "1 validation error detected: Value null at 'entries.1.member.attrs.k.member.value.dataType' "+
		"failed to satisfy constraint: Member must not be null", r.Message())

	r.Value(nil, "name", "x", "Member must have length greater than or equal to 2")
	r.Hidden(nil, "plaintext", "Member must have length less than or equal to 1")
	assert.Equal(t, "3 validation errors detected: "+
		"Value null at 'entries.1.member.attrs.k.member.value.dataType' failed to satisfy constraint: "+
		"Member must not be null; "+
		"Value 'x' at 'name' failed to satisfy constraint: Member must have length greater than or equal to 2; "+
		"Value at 'plaintext' failed to satisfy constraint: Member must have length less than or equal to 1", r.Message())
}

func Test_Length_CountsUTF16Units(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 3, constraint.Length("abc"))
	assert.Equal(t, 1, constraint.Length("é"))
	assert.Equal(t, 2, constraint.Length("😀"))
}

func Test_Text(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "[]", constraint.Items([]string{}))
	assert.Equal(t, "[a, b]", constraint.Items([]string{"a", "b"}))
	assert.Equal(t, "-6", constraint.Number(int32(-6)))
	assert.Equal(t, "1.5", constraint.Number(1.5))
	assert.Equal(t, []string{"a", "b"}, constraint.Keys(map[string]int{"b": 1, "a": 2}))
}

func Test_Pattern_MatchString(t *testing.T) {
	t.Parallel()

	p := constraint.NewPattern("^(?:[a-z]+)$")
	assert.True(t, p.MatchString("abc"))
	assert.False(t, p.MatchString("abc1"))
}
