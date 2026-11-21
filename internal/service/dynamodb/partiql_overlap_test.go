package dynamodb_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The pairs rows of both tests name more than twice.
const (
	pairParentFirst       = "path one: [a], path two: [a, b]"
	pairChildFirst        = "path one: [a, b], path two: [a]"
	pairParentSecondChild = "path one: [a], path two: [a, c]"
)

// updateOverlapRows are what the LocalStack image answered for an UPDATE of
// the actions in src (SQ-395). The image adds an UPDATE's paths to its overlap
// trie in the order the statement lists them, first to last, whichever clause
// each action is, where the UpdateExpression the statement compiles to writes
// its clauses one after the other and is checked last to first.
//
//nolint:lll // each row holds the exact pair the LocalStack image named
var updateOverlapRows = []struct {
	name     string
	src      string
	conflict bool
	pair     string
}{
	{"parent, then child", "SET a = 1 SET a.b = 2", false, pairChildFirst},
	{"child, then parent", "SET a.b = 2 SET a = 1", false, pairParentFirst},
	{"remove child, set parent", "REMOVE a.b SET a = 1", false, pairParentFirst},
	{"set parent, remove child", "SET a = 1 REMOVE a.b", false, pairChildFirst},
	{"remove between", "SET a = 1 REMOVE b REMOVE a", false, "path one: [a], path two: [b]"},
	{"set_add, then child", "SET s = set_add(s, <<`x`>>) SET s.b = 1", false, "path one: [s, b], path two: [s]"},
	{"child, then set_delete", "SET s.b = 1 SET s = set_delete(s, <<`x`>>) SET t = 1", false, "path one: [s], path two: [s, b]"},
	{"two children, then parent", "SET a.b = 1 SET a.c = 1 SET a = 3", false, pairParentSecondChild},
	{"clauses interleaved", "SET a.b = 1 REMOVE a.c SET a = 3", false, pairParentSecondChild},
	{"comma form", "SET a = 1, a.b = 2", false, pairChildFirst},
	{"list, then map", "SET a[0] = 1 SET a.b = 1", true, "path one: [a, b], path two: [a, [0]]"},
	{"map, then list", "SET a.b = 1 SET a[0] = 1", true, "path one: [a, [0]], path two: [a, b]"},
	{"the same path twice", "SET a = 1 SET a = 2", false, "path one: [a], path two: [a]"},
	{"two parents, children first", "SET c.d = 1 SET a.b = 1 REMOVE a REMOVE c", false, "path one: [a], path two: [c, d]"},
	{"a chain down", "SET a.b.c = 1 SET a.b = 1 SET a = 1", false, "path one: [a, b], path two: [a, b, c]"},
	{"another attribute between", "SET a = 1 SET x = 1 SET a.b = 1", false, "path one: [a, b], path two: [x]"},
	{
		"a clash after others", "SET k5 = 1 REMOVE k9 SET k15.k14 = 1 SET k14 = 1 SET k15 = 1 SET k12.k1 = 1", false,
		"path one: [k15], path two: [k9]",
	},
}

// overlapMessage is the answer for two paths that overlap, conflictMessage for
// two that conflict.
func overlapMessage(pair string) string {
	return "Two document paths overlap with each other; must remove or rewrite one of these paths; " + pair
}

func conflictMessage(pair string) string {
	return "Two document paths conflict with each other; must remove or rewrite one of these paths; " + pair
}

func Test_partiql_updateOverlapPair(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pover"))

	steps := make([]step, len(updateOverlapRows))
	for i, row := range updateOverlapRows {
		msg := overlapMessage(row.pair)
		if row.conflict {
			msg = conflictMessage(row.pair)
		}

		steps[i] = step{
			row.name, opStatement, stmt("UPDATE pover " + row.src + " WHERE id = `a`"), http.StatusBadRequest,
			validation(msg),
		}
	}

	run(t, s, steps)
}

// Test_partiql_updateOverlapPairInABatchAndATransaction: a batch and a
// transaction name the pair a single statement does.
func Test_partiql_updateOverlapPairInABatchAndATransaction(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pover"))

	src := jsonString("UPDATE pover SET a = 1 REMOVE b REMOVE a WHERE id = 'a'")
	msg := overlapMessage("path one: [a], path two: [b]")

	got := s.call(t, usEast1, "BatchExecuteStatement", `{"Statements":[{"Statement":`+src+`}]}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"Responses":[{"Error":{"Code":"ValidationError","Message":`+jsonString(msg)+
		`}}]}`, got.body)

	got = s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+src+`}]}`)
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	require.Contains(t, got.body, msg)
}

// selectLongRows are what the LocalStack image answered for a SELECT of
// hundreds of paths (SQ-395). It holds a SELECT list to no size, so a list of
// 769 or more paths is checked for overlap as a short one is, where a
// ProjectionExpression of the same paths is refused for its size first.
//
//nolint:lll // each row holds the exact pair the LocalStack image named
var selectLongRows = []struct {
	name  string
	paths int
	extra func(paths []string) []string
	pair  string
}{
	{"child last", 800, func(p []string) []string { return append(p, "k3.q") }, "path one: [k3, q], path two: [k751]"},
	{"child first", 800, func(p []string) []string { return append([]string{"k3.q"}, p...) }, "path one: [k3], path two: [k3, q]"},
	{
		"child in the middle", 800,
		func(p []string) []string { return append(p[:400:400], append([]string{"k3.q"}, p[400:]...)...) },
		"path one: [k3, q], path two: [k70]",
	},
	{"769 paths", 769, func(p []string) []string { return append(p, "k5.q") }, "path one: [k5, q], path two: [k751]"},
	{
		"1,000 paths and two children", 998, func(p []string) []string { return append(p, "k3.q", "k9.z") },
		"path one: [k3, q], path two: [k991]",
	},
}

func Test_partiql_selectLongOverlapPair(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pover"))

	steps := make([]step, len(selectLongRows))

	for i, row := range selectLongRows {
		paths := make([]string, row.paths)
		for k := range paths {
			paths[k] = "k" + strconv.Itoa(k)
		}

		steps[i] = step{
			row.name, opStatement,
			stmt("SELECT " + strings.Join(row.extra(paths), ", ") + " FROM pover WHERE id = `a`"),
			http.StatusBadRequest, validation(overlapMessage(row.pair)),
		}
	}

	run(t, s, steps)
}

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
	{"parent, then two children", "a, a.b, a.c", false, pairChildFirst},
	{"child, parent, child", "a.b, a, a.c", false, pairParentFirst},
	{"two children, then parent", "a.b, a.c, a", false, pairParentSecondChild},
	{"the other child first", "a.c, a.b, a", false, pairParentSecondChild},
	{"another attribute between", "a, x, a.b", false, "path one: [a, b], path two: [x]"},
	{"another attribute, parent last", "a.b, x, a", false, "path one: [a], path two: [x]"},
	{"two parents with children", "a.b, c.d, c, a", false, "path one: [c], path two: [c, d]"},
	{"two parents, children last", "c.d, a.b, a, c", false, "path one: [a], path two: [c, d]"},
	{"a chain down", "a.b.c, a.b, a", false, "path one: [a, b], path two: [a, b, c]"},
	{"a chain up", "a, a.b, a.b.c", false, pairChildFirst},
	{"the middle of a chain last", "a.b, a, a.b.c", false, pairParentFirst},
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
