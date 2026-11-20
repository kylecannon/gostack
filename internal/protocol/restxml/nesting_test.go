package restxml_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingReader counts the bytes read through it.
type countingReader struct {
	r strings.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n

	return n, err //nolint:wrapcheck // io.Reader: the error of the Read it delegates to.
}

// Test_Decoder_NestedToTheBodyCap: a document of the 16 MiB cap that nests
// elements as deeply as it can is refused with ErrMalformed at
// restxml.MaxDepth, whether walked or skipped, after reading a small part of
// the body.
func Test_Decoder_NestedToTheBodyCap(t *testing.T) {
	t.Parallel()

	size := int(protocol.DefaultMaxBody)
	n := (size - 100) / len("<a></a>")
	body := "<LifecycleConfiguration><Rule>" + strings.Repeat("<a>", n) + strings.Repeat("</a>", n) +
		"</Rule></LifecycleConfiguration>"
	require.LessOrEqual(t, len(body), size)

	for _, skip := range []bool{false, true} {
		src := &countingReader{r: *strings.NewReader(body)}

		var before, after runtime.MemStats

		runtime.ReadMemStats(&before)

		d := restxml.NewDecoder(src)
		_, ok := d.Root()
		require.True(t, ok)

		if skip {
			d.Skip()
		} else {
			entered := 0
			for _, ok := d.Next(); ok; _, ok = d.Next() {
				entered++
			}

			assert.Less(t, entered, restxml.MaxDepth)
		}

		runtime.ReadMemStats(&after)

		require.ErrorIs(t, d.Err(), restxml.ErrMalformed, "skip=%t", skip)
		assert.Less(t, src.n, 1<<20, "skip=%t: the decoder stops near the bound", skip)
		assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(size), "skip=%t", skip)
	}
}
