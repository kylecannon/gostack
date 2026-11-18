package sqs

// entryHeap is a binary min-heap of entries that knows where each entry sits,
// so an entry can be removed or moved in O(log n) without a scan. An entry is
// in at most one heap per index field.
type entryHeap struct {
	items []*entry
	less  func(a, b *entry) bool
	// index returns the field that holds the position of an entry in this heap.
	index func(*entry) *int
}

func (h *entryHeap) len() int { return len(h.items) }

// peek returns the smallest entry, or nil when the heap is empty.
func (h *entryHeap) peek() *entry {
	if len(h.items) == 0 {
		return nil
	}

	return h.items[0]
}

func (h *entryHeap) push(e *entry) {
	h.items = append(h.items, e)
	*h.index(e) = len(h.items) - 1
	h.up(len(h.items) - 1)
}

// pop removes and returns the smallest entry, or nil when the heap is empty.
func (h *entryHeap) pop() *entry {
	top := h.peek()
	if top != nil {
		h.remove(top)
	}

	return top
}

// remove takes e out of the heap.
func (h *entryHeap) remove(e *entry) {
	i := *h.index(e)
	last := len(h.items) - 1

	h.swap(i, last)
	h.items[last] = nil
	h.items = h.items[:last]
	*h.index(e) = -1

	if i < last {
		h.fix(h.items[i])
	}
}

// fix restores the order after the key of e changed.
func (h *entryHeap) fix(e *entry) {
	i := *h.index(e)
	if !h.down(i) {
		h.up(i)
	}
}

// reset empties the heap.
func (h *entryHeap) reset() {
	for _, e := range h.items {
		*h.index(e) = -1
	}

	clear(h.items)
	h.items = h.items[:0]
}

func (h *entryHeap) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	*h.index(h.items[i]) = i
	*h.index(h.items[j]) = j
}

func (h *entryHeap) up(i int) {
	for i > 0 {
		parent := (i - 1) >> 1
		if !h.less(h.items[i], h.items[parent]) {
			return
		}

		h.swap(i, parent)
		i = parent
	}
}

// down moves the entry at i towards the leaves and reports whether it moved.
func (h *entryHeap) down(i int) bool {
	start := i

	for {
		smallest := i
		left := i<<1 + 1
		right := left + 1

		if left < len(h.items) && h.less(h.items[left], h.items[smallest]) {
			smallest = left
		}

		if right < len(h.items) && h.less(h.items[right], h.items[smallest]) {
			smallest = right
		}

		if smallest == i {
			return i > start
		}

		h.swap(i, smallest)
		i = smallest
	}
}
