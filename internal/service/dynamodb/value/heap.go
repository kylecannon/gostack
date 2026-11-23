package value

import (
	"unsafe"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// The heap the engine's values take on a 64-bit platform, as go1.27 lays them
// out, for HeapSize. heap_test.go measures the estimate against the real heap.
const (
	// attributeBytes is one api.AttributeValue: a map slot's value or a list
	// element.
	attributeBytes = int(unsafe.Sizeof(api.AttributeValue{}))
	// stringHeader and sliceHeader are a string set's and a binary set's
	// element.
	stringHeader = int(unsafe.Sizeof(""))
	sliceHeader  = int(unsafe.Sizeof([]byte(nil)))
	// pointerAlloc is the allocation an S, N, NULL or BOOL pointer points to:
	// a string header or a bool, in the smallest size class.
	pointerAlloc = 16

	// A map of at most smallMap entries is a header and one group sized to
	// it: smallMapBase plus smallMapEntry an entry. A larger one is tables
	// of groups kept under seven-eighths full, which a map grown by insertion
	// or made with a hint a little past its length leaves at up to
	// largeMapEntry an entry.
	smallMap      = 8
	smallMapBase  = 256
	smallMapEntry = 176
	largeMapBase  = 64
	largeMapEntry = 240

	// The size classes are multiples of 16 up to fineClasses bytes, besides
	// 8 and 24, which a multiple of 16 covers. Up to maxSmallAlloc they round
	// a request up by less than a fifth plus classFloor bytes. A larger
	// allocation takes whole pages.
	fineClasses   = 256
	fineStep      = 16
	maxSmallAlloc = 32 << 10
	classSlack    = 5
	classFloor    = 16
	page          = 8 << 10
)

// HeapSize estimates the heap item takes: its map, its attribute names and
// each value's own allocations, from the lengths and capacities of the value
// tree. It never reads a type at run time. Where the layout leaves a range,
// such as a map's capacity, it takes the top of it, so the estimate is at
// least what item holds. It is not DynamoDB's size, which ItemSize is.
func HeapSize(item Item) int64 {
	if item == nil {
		return 0
	}

	return int64(mapHeap(item))
}

func mapHeap(m map[string]api.AttributeValue) int {
	total := mapBytes(len(m))
	for name, v := range m {
		total += allocBytes(len(name)) + valueHeap(&v)
	}

	return total
}

// mapBytes is the map of n entries itself: its header, groups and slots.
func mapBytes(n int) int {
	if n <= smallMap {
		return smallMapBase + n*smallMapEntry
	}

	return largeMapBase + n*largeMapEntry
}

// valueHeap is what v points to, not v itself, which its map slot or list
// element holds. It counts every member that is set, so a value with more
// than one is not undercounted.
func valueHeap(v *api.AttributeValue) int {
	total := scalarHeap(v)

	if v.M != nil {
		total += mapHeap(v.M)
	}

	if v.L != nil {
		total += allocBytes(cap(v.L) * attributeBytes)
		for i := range v.L {
			total += valueHeap(&v.L[i])
		}
	}

	return total + setHeap(v)
}

func scalarHeap(v *api.AttributeValue) int {
	total := allocBytes(cap(v.B))

	if v.S != nil {
		total += pointerAlloc + allocBytes(len(*v.S))
	}

	if v.N != nil {
		total += pointerAlloc + allocBytes(len(*v.N))
	}

	if v.NULL != nil {
		total += pointerAlloc
	}

	if v.BOOL != nil {
		total += pointerAlloc
	}

	return total
}

func setHeap(v *api.AttributeValue) int {
	total := allocBytes(cap(v.SS)*stringHeader) + allocBytes(cap(v.NS)*stringHeader) +
		allocBytes(cap(v.BS)*sliceHeader)

	for _, s := range v.SS {
		total += allocBytes(len(s))
	}

	for _, s := range v.NS {
		total += allocBytes(len(s))
	}

	for _, b := range v.BS {
		total += allocBytes(cap(b))
	}

	return total
}

// allocBytes is at least what an allocation of n bytes takes once its size
// class or pages round it up.
func allocBytes(n int) int {
	switch {
	case n <= 0:
		return 0
	case n <= fineClasses:
		return (n + fineStep - 1) &^ (fineStep - 1)
	case n <= maxSmallAlloc:
		return n + n/classSlack + classFloor
	default:
		return (n + page - 1) &^ (page - 1)
	}
}
