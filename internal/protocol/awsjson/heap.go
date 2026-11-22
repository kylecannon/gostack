package awsjson

import "sort"

// What a decoded value keeps on the heap, from how the Go runtime lays out what
// json/v2 allocates (Go 1.27: internal/runtime/gc/sizeclasses.go,
// internal/runtime/maps, runtime.growslice). Each figure is the most the
// runtime can take for it, so that the walk's heap (walker.heap) is never
// below what the decoded value keeps live; Test_heapEstimate_coversLiveHeap
// holds it to the heap measured after decoding.
const (
	// sizeClasses is the runtime's size classes up to maxSmallSize: an
	// allocation takes the first class that holds it.
	maxSmallSize = 32 << 10
	// pageSize rounds a larger allocation.
	pageSize = 8 << 10
	// tinySize is the block the runtime packs allocations without pointers
	// smaller than it into: one of them alive keeps its block.
	tinySize = 16
	// mallocHeader is what the runtime puts in front of an allocation of more
	// than mallocHeaderOver bytes that holds pointers; it is counted for any.
	mallocHeader     = 8
	mallocHeaderOver = 512

	// mapHeader is a map's own structure (maps.Map), allocated for every
	// map decoded, even an empty one.
	mapHeader = 48
	// mapGroupSlots is the slots of one group, each holding a key and a value,
	// after its 8 control bytes; mapMaxSlot is the most a key or a value takes
	// in a slot: one larger is allocated out of line and the slot holds a
	// pointer to it.
	mapGroupSlots = 8
	mapControl    = 8
	mapMaxSlot    = 128
	pointerSize   = 8
	// A map of more than one group is split into tables of up to
	// maxTableSlots slots, each grown by doubling when it is 7/8 full and
	// split in two when full at its most. A table is at least 7/16 full once
	// it has grown and about half full once split, so mapSlotsPerEntry slots
	// for each entry is the most it takes. Each table adds its own structure
	// and its places in the map's directory (mapTableCost), and holds no fewer
	// than mapEntriesPerTable entries.
	maxTableSlots      = 1024
	mapSlotsPerEntry   = 3
	mapTableCost       = 96
	mapEntriesPerTable = 256

	// growThreshold is where growslice stops doubling a slice's capacity and
	// grows it by a quarter and 192 elements at a time.
	growThreshold = 256
)

var sizeClasses = [...]int64{
	8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256, 288, 320, 352, 384, 416, 448,
	480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280, 1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456, 4096,
	4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880, 12288, 13568, 14336, 16384, 18432, 19072,
	20480, 21760, 24576, 27264, 28672, 32768,
}

// allocSize is the most the heap gives an allocation of n bytes: its size class,
// at least a tiny block, or whole pages for a large one. Zero bytes take
// nothing.
func allocSize(n int64) int64 {
	switch {
	case n <= 0:
		return 0
	case n <= tinySize:
		return tinySize
	case n > mallocHeaderOver:
		n += mallocHeader
	default:
	}

	if n > maxSmallSize {
		return (n + pageSize - 1) / pageSize * pageSize
	}

	return sizeClasses[sort.Search(len(sizeClasses), func(i int) bool { return sizeClasses[i] >= n })]
}

// sliceBacking is the most the array of a slice of n elements of size bytes
// takes, as json/v2 grows it: one element past its capacity at a time
// (reflect.Value.Grow), so its last growth was from a capacity below n.
func sliceBacking(n, size int64) int64 {
	if n == 0 {
		return 0 // json/v2 gives an empty list a shared empty slice
	}

	return allocSize(nextCapacity(n-1) * size)
}

// nextCapacity is the capacity growslice gives a full slice of capacity c
// grown by one, before its size class rounds it up.
func nextCapacity(c int64) int64 {
	switch {
	case c == 0:
		return 1
	case c < growThreshold:
		return 2 * c //nolint:mnd // growslice doubles a small slice
	default:
		return c + (c+3*growThreshold)/4
	}
}

// mapLayout is one group of a map whose keys take key bytes and whose values
// take value bytes, and what an entry allocates out of line.
func mapLayout(key, value int64) (group, outOfLine int64) {
	slot := func(n int64) int64 {
		if n > mapMaxSlot {
			outOfLine += allocSize(n)

			return pointerSize
		}

		return (n + pointerSize - 1) / pointerSize * pointerSize
	}

	return mapControl + mapGroupSlots*(slot(key)+slot(value)), outOfLine
}

// mapTables is the most the groups of a map holding the given number of
// entries take, one group for a map that fits it, and what growing one of its tables holds besides at
// most: the table it is grown from.
func mapTables(entries, group int64) (tables, growing int64) {
	switch {
	case entries == 0:
		return 0, 0
	case entries <= mapGroupSlots:
		return allocSize(group), 0
	}

	slots := entries * mapSlotsPerEntry
	groups := (slots + mapGroupSlots - 1) / mapGroupSlots * group
	// Each table's groups are one allocation; its size class adds at most a
	// quarter.
	tables = groups + groups/4 + (entries/mapEntriesPerTable+1)*mapTableCost

	return tables, min(tables, allocSize(maxTableSlots/mapGroupSlots*group))
}
