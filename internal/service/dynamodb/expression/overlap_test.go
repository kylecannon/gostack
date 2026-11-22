package expression_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
)

const (
	pairParentFirst = "path one: [a], path two: [a, b]"

	overlapWith  = "overlap with each other; must remove or rewrite one of these paths; "
	conflictWith = "conflict with each other; must remove or rewrite one of these paths; "
)

// overlapRows are what the LocalStack image answered for an UpdateExpression
// ("SET p = :v" for each path of src) and for a ProjectionExpression of the
// paths (SQ-333). The pair it names is not always a pair that overlap: it adds
// the paths to a trie last to first, and as path two names the first leaf of
// the java.util.HashMap that held the clash, a bucket order the rows pin with
// names in different buckets, in one bucket, list indexes, and sets of
// siblings large enough to grow the table to 32 and 64 buckets.
//
//nolint:lll // each row holds the exact pair the LocalStack image named
var overlapRows = []struct {
	name string
	src  string
	want string
}{
	{"parent, then two children", "a, a.b, a.c", overlapWith + "path one: [a], path two: [a, c]"},
	{"child, parent, child", "a.b, a, a.c", overlapWith + "path one: [a], path two: [a, c]"},
	{"two children, then parent", "a.b, a.c, a", overlapWith + "path one: [a, c], path two: [a]"},
	{"the other child first", "a.c, a.b, a", overlapWith + "path one: [a, b], path two: [a]"},
	{"another attribute between", "a, x, a.b", overlapWith + "path one: [a], path two: [x]"},
	{"another attribute, parent last", "a.b, x, a", overlapWith + "path one: [a, b], path two: [x]"},
	{"two parents with children", "a.b, c.d, c, a", overlapWith + "path one: [c, d], path two: [c]"},
	{"two parents, children last", "c.d, a.b, a, c", overlapWith + "path one: [a, b], path two: [c]"},
	{"a chain down", "a.b.c, a.b, a", overlapWith + "path one: [a, b], path two: [a]"},
	{"a chain up", "a, a.b, a.b.c", overlapWith + "path one: [a, b], path two: [a, b, c]"},
	{"the middle of a chain last", "a.b, a, a.b.c", overlapWith + "path one: [a], path two: [a, b, c]"},
	{"names in other buckets", "hello, world, abc, hello.q", overlapWith + "path one: [hello], path two: [abc]"},
	{"names of one bucket, first added", "ab.q, a.q, ab", overlapWith + "path one: [ab, q], path two: [ab]"},
	{"names of one bucket, the other first", "a.q, ab.q, a", overlapWith + "path one: [a, q], path two: [a]"},
	{"a list and a map", "a[17], a[2].b, a.aa", conflictWith + "path one: [a, [2], b], path two: [a, aa]"},
	{"list indexes in bucket order", "a[1].x, a[3], a[2], a[1]", overlapWith + "path one: [a, [1], x], path two: [a, [3]]"},
	{"thirteen siblings", "k19.q, k50, k64, k31, k55, k71, k24, k30, k40, k10, k74, k70, k21, k19", overlapWith + "path one: [k19, q], path two: [k19]"},
	{"twenty-five siblings", "k48.q, k57, k46, k35, k13, k12, k41, k36, k4, k25, k62, k74, k49, k63, k53, k8, k27, k32, k76, k14, k77, k24, k9, k54, k21, k48", overlapWith + "path one: [k48, q], path two: [k74]"},
}

func Test_ParseUpdate_OverlapPair(t *testing.T) {
	t.Parallel()

	for _, tt := range overlapRows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expr := "SET " + strings.ReplaceAll(tt.src, ", ", " = :v, ") + " = :v"
			_, err := expression.ParseUpdate(expr, autoSubs(expr), nil)
			requireUpdateError(t, err, "Invalid UpdateExpression: Two document paths "+tt.want)
		})
	}
}

func Test_ParseProjection_OverlapPair(t *testing.T) {
	t.Parallel()

	for _, tt := range overlapRows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseProjection(tt.src, nil)
			wantMessage(t, err, "Invalid ProjectionExpression: Two document paths "+tt.want)
		})
	}
}

// Test_ParseUpdateRanked_OverlapPair: a PartiQL UPDATE writes its actions
// clause by clause, and DynamoDB Local adds them to its trie in the order the
// statement lists them, first to last (SQ-395). Here the statement lists
// "a" and then "a.b", which the expression also writes in that order.
func Test_ParseUpdateRanked_OverlapPair(t *testing.T) {
	t.Parallel()

	const expr = "SET a = :v, a.b = :v"

	for _, tt := range []struct {
		name string
		rank []int
		want string
	}{
		{"no rank, last to first", nil, pairParentFirst},
		{"statement order", []int{0, 1}, "path one: [a, b], path two: [a]"},
		{"reversed order", []int{1, 0}, pairParentFirst},
		{"too few places", []int{0}, pairParentFirst},
		{"a place twice", []int{0, 0}, pairParentFirst},
		{"a place out of range", []int{0, 2}, pairParentFirst},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := expression.ParseUpdateRanked(expr, autoSubs(expr), nil, tt.rank)
			requireUpdateError(t, err, "Invalid UpdateExpression: Two document paths "+overlapWith+tt.want)
		})
	}
}

// Test_ParseStatementProjection: a SELECT list is held to no size, so 800 paths
// are checked for overlap, where a ProjectionExpression of them is refused for
// its size first (SQ-395).
func Test_ParseStatementProjection(t *testing.T) {
	t.Parallel()

	paths := make([]string, 800)
	for i := range paths {
		paths[i] = "k" + strconv.Itoa(i)
	}

	src := strings.Join(paths, ", ")
	subs, used := autoSubs(src), map[string]struct{}{}

	_, err := expression.ParseProjection(src, subs, used)
	wantMessage(t, err, "Invalid ProjectionExpression: Expression size has exceeded the maximum allowed size;")

	_, err = expression.ParseStatementProjection(src, subs, used)
	wantMessage(t, err, "")

	_, err = expression.ParseStatementProjection("k3.q, "+src, subs, used)
	wantMessage(t, err, "Invalid ProjectionExpression: Two document paths "+overlapWith+
		"path one: [k3, q], path two: [k751]")
}

// Test_ParseStatementUpdate: the actions of a PartiQL UPDATE are held to no
// size, so 400 SET actions, past the 4,096 code units of an UpdateExpression,
// are parsed and checked for overlap, where an UpdateExpression of them is
// refused for its size first (SQ-412).
func Test_ParseStatementUpdate(t *testing.T) {
	t.Parallel()

	actions := make([]string, 400)
	for i := range actions {
		actions[i] = "u" + strconv.Itoa(i) + " = :v"
	}

	src := "SET " + strings.Join(actions, ", ")
	subs, used := autoSubs(src), map[string]struct{}{}

	_, err := expression.ParseUpdateRanked(src, subs, used, nil)
	wantMessage(t, err, "Invalid UpdateExpression: Expression size has exceeded the maximum allowed size;")

	update, err := expression.ParseStatementUpdate(src, subs, used, nil)
	wantMessage(t, err, "")

	if got := len(update.Paths()); got != 400 {
		t.Fatalf("%d paths, want 400", got)
	}

	// Which pair the message names is the overlap tests' business.
	_, err = expression.ParseStatementUpdate(src+", u5.q = :v", subs, used, nil)
	if err == nil || !strings.Contains(err.Error(), "Two document paths "+overlapWith) {
		t.Fatalf("error %v, want an overlap", err)
	}
}

// Test_ParseUpdate_OverlapPairUnicode: the bucket of a name is its UTF-16
// String.hashCode, so names outside ASCII, with surrogate pairs among them,
// land where the image put them (SQ-333).
//
//nolint:gosmopolitan // the names are the point of the test
func Test_ParseUpdate_OverlapPairUnicode(t *testing.T) {
	t.Parallel()

	subs := expression.Substitutions{
		Names:  map[string]string{"#n0": "日本", "#n1": "😀", "#n2": "é", "#n3": "naïve"},
		Values: autoSubs(":v").Values,
	}
	want := "Two document paths " + overlapWith + "path one: [日本, q], path two: [naïve]"

	expr := "SET #n0.q = :v, #n3 = :v, #n2 = :v, #n1 = :v, #n0 = :v"
	_, err := expression.ParseUpdate(expr, subs, nil)
	requireUpdateError(t, err, "Invalid UpdateExpression: "+want)

	_, err = parseProjection("#n0.q, #n3, #n2, #n1, #n0", &subs)
	wantMessage(t, err, "Invalid ProjectionExpression: "+want)
}
