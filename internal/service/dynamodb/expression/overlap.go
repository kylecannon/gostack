package expression

import (
	"slices"
	"unicode/utf16"
)

// pathClash is the pair of paths DynamoDB Local names when two document paths
// overlap (one is the other, or leads into it) or conflict (they part where
// one names a map key and the other a list index).
type pathClash struct {
	conflict bool
	one, two Path
}

// findPathClash feeds paths into a trie, last to first as DynamoDB Local does
// for an UpdateExpression and a ProjectionExpression, and reports the first
// path that cannot join it. The pair it names is not the pair that clash.
// DynamoDB Local names the path being added as path one, and as path two the
// first leaf reached by taking, at every level from the map that held the
// clash down, the first entry of that level's java.util.HashMap (SQ-333). A
// PartiQL SELECT adds its paths first to last, so its caller passes them
// reversed.
func findPathClash(paths []Path) *pathClash {
	if len(paths) < minClashPaths {
		return nil
	}

	steps := 1 // the root

	for _, path := range paths {
		steps += len(path)
	}

	// The nodes share one allocation, sized for the most the paths can add, so
	// pointers into it stay put.
	nodes := make([]clashNode, 1, steps)
	root := &nodes[0]
	root.kids = make([]clashKid, 0, len(paths)) // most paths start at an attribute of their own

	for _, path := range slices.Backward(paths) {
		if c := root.add(path, &nodes); c != nil {
			return c
		}
	}

	return nil
}

// minClashPaths is the fewest paths that can clash: a path does not clash with
// itself.
const minClashPaths = 2

// clashNode is one step of the trie. kids lists the children in the order they
// were added, which a HashMap keeps for entries that share a bucket.
type clashNode struct {
	leaf bool
	kids []clashKid
	// index finds a kid by its element once there are too many to scan.
	index map[PathElement]*clashNode
}

type clashKid struct {
	elem PathElement
	node *clashNode
}

// clashScan is the most kids a node looks through before it keeps an index.
const clashScan = 16

func (n *clashNode) kid(e PathElement) *clashNode {
	if n.index != nil {
		return n.index[e]
	}

	for _, k := range n.kids {
		if k.elem == e {
			return k.node
		}
	}

	return nil
}

func (n *clashNode) add(path Path, nodes *[]clashNode) *pathClash {
	for i, e := range path {
		if len(n.kids) > 0 && (n.kids[0].elem.Name == "") != (e.Name == "") {
			return &pathClash{conflict: true, one: path, two: n.firstLeaf(path[:i])}
		}

		child := n.kid(e)
		if child != nil && (child.leaf || i == len(path)-1) {
			return &pathClash{one: path, two: n.firstLeaf(path[:i])}
		}

		if child == nil {
			*nodes = append(*nodes, clashNode{})
			child = &(*nodes)[len(*nodes)-1]
			n.kids = append(n.kids, clashKid{elem: e, node: child})

			switch {
			case n.index != nil:
				n.index[e] = child
			case len(n.kids) > clashScan:
				n.index = make(map[PathElement]*clashNode, len(n.kids)*2) //nolint:mnd // room to grow
				for _, k := range n.kids {
					n.index[k.elem] = k.node
				}
			default:
			}
		}

		n = child
	}

	n.leaf = true

	return nil
}

// firstLeaf is the path to the leaf DynamoDB Local names: prefix, the path
// that leads to n, then the first entry of each map below.
func (n *clashNode) firstLeaf(prefix Path) Path {
	out := append(Path(nil), prefix...)

	for !n.leaf && len(n.kids) > 0 {
		k := n.kids[n.firstKid()]
		out = append(out, k.elem)
		n = k.node
	}

	return out
}

// The HashMap table starts at 16 buckets and doubles each time it holds more
// than three quarters of them.
const (
	clashBuckets = 16
	clashLoadNum = 3
	clashLoadDen = 4

	// clashSalt is added to a key's hashCode before HashMap spreads it. The
	// image's order was fitted to it: every probed pair, and the first entry
	// of sets of up to 1,630 keys, agree with it through bit 27, which tables
	// of up to 4,096 buckets read (SQ-333).
	clashSalt = 0xFDF81

	clashSpread = 16 // HashMap.hash xors the high half of the hash into the low
)

// firstKid is the index in n.kids of the entry a HashMap of them iterates
// first: the one in the lowest bucket, the earliest added among those that
// share it. HashMap doubles a table whose bin passes eight entries while it
// holds under 64 buckets, and treeifies the bin at 64; neither is modeled, so
// crafted names whose String hashCodes collide nine or more deep may name a
// different second path than the image does. Whether a request is rejected or
// accepted is unaffected.
func (n *clashNode) firstKid() int {
	buckets := uint32(clashBuckets)
	for len(n.kids)*clashLoadDen > int(buckets)*clashLoadNum {
		buckets *= 2
	}

	first, low := 0, ^uint32(0)

	for i, k := range n.kids {
		if b := clashHash(k.elem) & (buckets - 1); b < low {
			first, low = i, b
		}
	}

	return first
}

// clashHash is the hash of a path element once HashMap has spread it: a name's
// String.hashCode, or a list index as an Integer.
func clashHash(e PathElement) uint32 {
	h := uint32(e.Index) //nolint:gosec // a list index is never negative
	if e.Name != "" {
		h = uint32(javaStringHash(e.Name)) //nolint:gosec // reads the bits of Java's signed int
	}

	h += clashSalt

	return h ^ h>>clashSpread
}

// javaStringHash is String.hashCode, taken over UTF-16 code units.
func javaStringHash(s string) int32 {
	var h int32

	for _, u := range utf16.Encode([]rune(s)) {
		h = h*javaHashMultiplier + int32(u)
	}

	return h
}

const javaHashMultiplier = 31
