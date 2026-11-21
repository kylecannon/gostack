package dynamodb_test

import (
	"net/http"
	"testing"
)

// selectOverlapRows are what the LocalStack image answered for a SELECT of the
// paths of src (SQ-333). A SELECT adds its paths to DynamoDB Local's overlap
// trie first to last where UpdateItem and GetItem add theirs last to first,
// and as path two the message names the first leaf of the java.util.HashMap
// that held the clash.
//
//nolint:lll // each row holds the exact pair the LocalStack image named
var selectOverlapRows = []struct {
	name     string
	src      string
	conflict bool
	pair     string
}{
	{"parent, then two children", "a, a.b, a.c", false, "path one: [a, b], path two: [a]"},
	{"child, parent, child", "a.b, a, a.c", false, "path one: [a], path two: [a, b]"},
	{"two children, then parent", "a.b, a.c, a", false, "path one: [a], path two: [a, c]"},
	{"the other child first", "a.c, a.b, a", false, "path one: [a], path two: [a, c]"},
	{"another attribute between", "a, x, a.b", false, "path one: [a, b], path two: [x]"},
	{"another attribute, parent last", "a.b, x, a", false, "path one: [a], path two: [x]"},
	{"two parents with children", "a.b, c.d, c, a", false, "path one: [c], path two: [c, d]"},
	{"two parents, children last", "c.d, a.b, a, c", false, "path one: [a], path two: [c, d]"},
	{"a chain down", "a.b.c, a.b, a", false, "path one: [a, b], path two: [a, b, c]"},
	{"a chain up", "a, a.b, a.b.c", false, "path one: [a, b], path two: [a]"},
	{"the middle of a chain last", "a.b, a, a.b.c", false, "path one: [a], path two: [a, b]"},
	{"names in other buckets", "hello, world, abc, hello.q", false, "path one: [hello, q], path two: [abc]"},
	{"a list and a map", "a[17], a[2].b, a.aa", true, "path one: [a, aa], path two: [a, [2], b]"},
	{"thirteen siblings", "k19.q, k50, k64, k31, k55, k71, k24, k30, k40, k10, k74, k70, k21, k19", false, "path one: [k19], path two: [k19, q]"},
	{"twenty-five siblings", "k48.q, k57, k46, k35, k13, k12, k41, k36, k4, k25, k62, k74, k49, k63, k53, k8, k27, k32, k76, k14, k77, k24, k9, k54, k21, k48", false, "path one: [k48], path two: [k74]"},
}

func Test_partiql_selectOverlapPair(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pover"))

	steps := make([]step, len(selectOverlapRows))
	for i, row := range selectOverlapRows {
		kind := "overlap"
		if row.conflict {
			kind = "conflict"
		}

		steps[i] = step{
			row.name, opStatement, stmt("SELECT " + row.src + " FROM pover WHERE id = `a`"), http.StatusBadRequest,
			validation("Two document paths " + kind + " with each other; must remove or rewrite one of these " +
				"paths; " + row.pair),
		}
	}

	run(t, s, steps)
}
