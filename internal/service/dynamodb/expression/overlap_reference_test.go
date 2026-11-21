package expression_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
)

// refClash is what the pairwise reference says of a set of paths that clash:
// the path that cannot join, whether it conflicts rather than overlaps, the
// step it is refused at, and the earlier paths the trie may name second.
type refClash struct {
	one      expression.Path
	conflict bool
	level    int
	twos     []expression.Path
}

// pairwiseClash is the reference findPathClash is held to: it adds the paths
// last to first, as the trie does, and for the first one that clashes with an
// earlier one it compares every pair. It shares nothing with the trie's
// shortcuts. It returns nil for paths that do not clash.
func pairwiseClash(paths []expression.Path) *refClash {
	for j, p := range slices.Backward(paths) {
		c := refClash{one: p, level: -1}

		for _, q := range paths[j+1:] {
			level, conflict := pairLevel(p, q)
			if level >= 0 && (c.level < 0 || level < c.level) {
				c.level, c.conflict = level, conflict
			}
		}

		if c.level < 0 {
			continue
		}

		for _, q := range paths[j+1:] {
			if len(q) > c.level && slices.Equal(q[:c.level], p[:c.level]) {
				c.twos = append(c.twos, q)
			}
		}

		return &c
	}

	return nil
}

// pairLevel is the step at which a trie holding q refuses p, or -1 when it
// accepts: where they part at a map key and a list index (a conflict), or
// where one ends inside the other (an overlap).
func pairLevel(p, q expression.Path) (level int, conflict bool) {
	for k := range min(len(p), len(q)) {
		switch {
		case (p[k].Name == "") != (q[k].Name == ""):
			return k, true
		case p[k] != q[k]:
			return -1, false
		}
	}

	return min(len(p), len(q)) - 1, false
}

// randomPaths draws n paths of up to three steps. The names of each step come
// from a pool that is small, so names repeat and paths clash, or large, so they
// almost never do; some steps, rarely the first, are list indexes.
func randomPaths(r *rand.Rand, n int) []expression.Path {
	var pools [3]int

	for k := range pools {
		pools[k] = 8 * n * n

		if r.IntN(2) == 0 {
			pools[k] = 1 + r.IntN(2*n)
		}
	}

	depth := 1 + r.IntN(len(pools))
	listPct := [...]int{0, 0, 3, 15}[r.IntN(4)]
	paths := make([]expression.Path, n)

	for i := range paths {
		path := make(expression.Path, 1+r.IntN(depth))
		for k := range path {
			if r.IntN(100) < listPct && (k > 0 || r.IntN(10) == 0) {
				path[k] = expression.PathElement{Index: r.IntN(4)}
			} else {
				path[k] = expression.PathElement{Name: fmt.Sprintf("k%d", r.IntN(pools[k]))}
			}
		}

		paths[i] = path
	}

	return paths
}

// Test_PathsClash_PairwiseReference holds the trie to a comparison of every
// pair on random sets of paths: it refuses exactly the sets the pairwise
// reference refuses, as an overlap or a conflict as the reference says, naming
// as path one the path the reference names, and as path two one of the earlier
// paths that part from it where the reference found the clash. A shortcut that
// accepts or refuses a set the trie would not shows here (SQ-404).
func Test_PathsClash_PairwiseReference(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(333, 404))

	var accepted, overlaps, conflicts int

	for range 8000 {
		n := 2 + r.IntN(70)
		if r.IntN(50) == 0 {
			n = 100 + r.IntN(700) // past what an arena keeps, and past the scan of one node
		}

		paths := randomPaths(r, n)
		want := pairwiseClash(paths)
		got := expression.PathsClash(paths)

		if (got != nil) != (want != nil) {
			t.Fatalf("clash = %v, reference %v for %s", got, want, describePaths(paths))
		}

		if got == nil {
			accepted++

			continue
		}

		if want.conflict {
			conflicts++
		} else {
			overlaps++
		}

		switch {
		case got.Conflict != want.conflict:
			t.Fatalf("conflict = %v, reference %v (step %d) for %s",
				got.Conflict, want.conflict, want.level, describePaths(paths))
		case !slices.Equal(got.One, want.one):
			t.Fatalf("path one = %v, reference %v for %s", got.One, want.one, describePaths(paths))
		case !slices.ContainsFunc(want.twos, func(q expression.Path) bool { return slices.Equal(q, got.Two) }):
			t.Fatalf("path two = %v, not among the earlier paths %v that part at step %d for %s",
				got.Two, want.twos, want.level, describePaths(paths))
		default:
		}
	}

	if accepted < 1000 || overlaps < 1000 || conflicts < 100 {
		t.Fatalf("the random sets cover too little: %d accepted, %d overlaps, %d conflicts", accepted, overlaps, conflicts)
	}
}

func describePaths(paths []expression.Path) string {
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = fmt.Sprint(p)
	}

	return strings.Join(parts, " ")
}
