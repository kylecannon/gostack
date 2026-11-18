package s3

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	canonicalMeta = "X-Amz-Meta-Q"
	lowerMeta     = "x-amz-meta-q"
)

// TestSpellHeaders: Etag becomes ETag and X-Amz-* goes to lower case, and when
// the target spelling is already in the map its values are kept, none
// overwritten, in a fixed order.
func TestSpellHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		give      http.Header
		want      http.Header
		wantTrims bool
	}{
		{
			name: "canonical names are respelled",
			give: http.Header{"Etag": {`"a"`}, canonicalMeta: {"1"}, "Content-Type": {"x"}},
			want: http.Header{attributeETag: {`"a"`}, lowerMeta: {"1"}, "Content-Type": {"x"}},
		},
		{
			name: "a canonical name does not overwrite its lower-case twin",
			give: http.Header{canonicalMeta: {"canonical"}, lowerMeta: {"lower"}},
			want: http.Header{lowerMeta: {"lower", "canonical"}},
		},
		{
			name: "an Etag does not overwrite an ETag",
			give: http.Header{"Etag": {`"canonical"`}, attributeETag: {`"exact"`}},
			want: http.Header{attributeETag: {`"exact"`, `"canonical"`}},
		},
		{
			name: "every value of a repeated header is kept",
			give: http.Header{canonicalMeta: {"1", "2"}, lowerMeta: {"0"}},
			want: http.Header{lowerMeta: {"0", "1", "2"}},
		},
		{
			name:      "edge white space is reported",
			give:      http.Header{canonicalMeta: {"a\t"}},
			want:      http.Header{lowerMeta: {"a\t"}},
			wantTrims: true,
		},
		{
			name: "inner white space is not",
			give: http.Header{canonicalMeta: {"c d"}},
			want: http.Header{lowerMeta: {"c d"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Go's map order is random, so one run proves nothing.
			for range 50 {
				header := tt.give.Clone()

				assert.Equal(t, tt.wantTrims, spellHeaders(header))
				assert.Equal(t, tt.want, header)
			}
		})
	}
}
