package ssm_test

import (
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

const (
	// cursorFilters and cursorFilterValues make a listing request of about
	// 250 KB: fifty tag filters, each with five values of a thousand
	// characters. SQ-277 found the cost with fifty values per filter, 2.4 MB a
	// request, where the heap grew by 341 MB; this is a tenth of that, which
	// is enough, and runs in seconds under the race detector.
	cursorFilters      = 50
	cursorFilterValues = 4
	cursorValueLength  = 1000

	// cursorListings is the number of different listings the test pages once:
	// as many as the cache keeps cursors for, so none is dropped to make room.
	cursorListings = 128

	// cursorHeapLimit is the growth of the heap, after a collection, the
	// listings above may leave behind. The cache is cursorListings entries of a
	// 32-byte key, an offset and an index, about 16 KiB, and the State holds
	// one small parameter. The race detector, the test binary and the
	// allocator's own bookkeeping move the figure by a few MiB, so the limit is
	// 8 MiB. The requests, 24 MiB of text, do not fit in it: a cache keyed by
	// the request text grew the heap by 27 MiB here.
	cursorHeapLimit = 8 << 20
)

// Test_theCursorCacheKeepsAFixedNumberOfBytes: a page that ends full is
// remembered, and the cache's key must not be the request, which can be
// megabytes. It sends listings with large filters, one page each, and asserts
// the heap after a collection holds none of them.
func Test_theCursorCacheKeepsAFixedNumberOfBytes(t *testing.T) {
	p := ssm.New()
	d := direct{tb: t, h: handlerFor(t, p, nil)}

	tags := make([]obj, 0, cursorFilters)
	for i := range cursorFilters {
		tags = append(tags, obj{fieldKey: "k" + strconv.Itoa(i), kValue: "v"})
	}

	status, out := d.do(kPutParameter, obj{kName: "/m/x", kValue: "v", kType: kString, kTags: tags})
	require.Equal(t, 200, status, string(out))

	long := strings.Repeat("x", cursorValueLength)

	runtime.GC() //nolint:revive // The heap is read after a collection, and the test needs the heap collected.

	var before runtime.MemStats

	runtime.ReadMemStats(&before)

	sent := 0

	for n := range cursorListings {
		filters := make([]obj, 0, cursorFilters)

		for i := range cursorFilters {
			values := make([]string, 1, 1+cursorFilterValues)
			values[0] = "v"

			for j := range cursorFilterValues {
				values = append(values, long+strconv.Itoa(n)+"-"+strconv.Itoa(j))
			}

			filters = append(filters, obj{fieldKey: "tag:k" + strconv.Itoa(i), kValues: values})
		}

		body := obj{kPath: "/m", fieldMaxResults: 1, fieldFilters: filters}

		raw, err := json.Marshal(body)
		require.NoError(t, err)

		sent += len(raw)

		status, out = d.do(kGetParametersByPath, body)
		require.Equal(t, 200, status, string(out[:min(len(out), 300)]))
		require.Contains(t, string(out), fieldNextToken, "a full page gives a token, so the listing is remembered")
	}

	runtime.GC() //nolint:revive // The heap is read after a collection, and the test needs the heap collected.

	var after runtime.MemStats

	runtime.ReadMemStats(&after)

	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d MiB of requests sent; the heap after a collection grew by %d KiB", sent>>20, grew>>10)
	require.Less(t, grew, int64(cursorHeapLimit), "the cursors hold the requests they were made for")

	runtime.KeepAlive(p)
}
