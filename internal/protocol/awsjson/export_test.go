package awsjson

import (
	"math"
	"reflect"
)

// HeapEstimate is what DecodeWithin reserves for decoding body into a T when
// it reserves heap (chargeDecode): the most the decoded value keeps live, the
// T included, and the most decoding holds at once beyond that.
func HeapEstimate[T any](body []byte) (live, peak int64) {
	p := planOf(reflect.TypeFor[T]())
	w := walker{dec: newWalkDecoder(body), limit: math.MaxInt64}
	_ = w.value(p)

	return w.heap + allocSize(p.heapSize), w.peak
}
