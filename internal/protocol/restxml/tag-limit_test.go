package restxml_test

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/kylecannon/gostack/internal/protocol/restxml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tagOf is a start tag of exactly n bytes: <R a="..."/>.
func tagOf(n int) string {
	const head, tail = `<R a="`, `"/>`

	return head + strings.Repeat("v", n-len(head)-len(tail)) + tail
}

// Test_Decoder_TagLimit: a start or end tag up to MaxTag decodes and one byte
// more is refused, wherever it sits and whatever came before it; text,
// comments, CDATA, processing instructions and directives are not tags and are
// not bounded by it. Each case runs through readers that hand the body over in
// different pieces, since the limit reads it through a buffer of its own.
func Test_Decoder_TagLimit(t *testing.T) {
	long := strings.Repeat("x", 4*restxml.MaxTag)
	endName := strings.Repeat("E", restxml.MaxTag-3)

	type tagCase struct {
		name, body string
		wantErr    bool
	}

	tests := []tagCase{
		{name: "a root tag of MaxTag", body: tagOf(restxml.MaxTag)},
		{name: "a root tag of MaxTag+1", body: tagOf(restxml.MaxTag + 1), wantErr: true},
		{name: "a child tag of MaxTag after text", body: "<A>t" + tagOf(restxml.MaxTag) + "</A>"},
		{name: "a child tag of MaxTag+1 after text", body: "<A>t" + tagOf(restxml.MaxTag+1) + "</A>", wantErr: true},
		{name: "an end tag of MaxTag", body: "<" + endName + "></" + endName + ">"},
		{name: "an end tag of MaxTag+1", body: "<" + endName + "E></" + endName + "E>", wantErr: true},
		{name: "long text", body: "<A>" + long + "</A>"},
		{name: "a long comment", body: "<A><!--" + long + "--></A>"},
		{name: "a long CDATA section", body: "<A><![CDATA[" + long + "]]></A>"},
		{name: "a long processing instruction", body: "<?pi " + long + "?><A/>"},
		{name: "a long directive", body: "<!DOCTYPE " + long + "><A/>"},
		{
			name: "a comment with a stray > and a quote, then a long tag",
			body: `<A><!-- > <a " -->` + `<R x="" y=">" z="` + long + `"/></A>`, wantErr: true,
		},
		{
			name: "a CDATA section with a stray > and a quote, then a long tag",
			body: `<A><![CDATA[ > <a " ]]>` + `<R x="" y=">" z="` + long + `"/></A>`, wantErr: true,
		},
		{
			name: "a processing instruction with a stray > and a quote, then a long tag",
			body: `<?pi > <a " ?><A><R x="" y=">" z="` + long + `"/></A>`, wantErr: true,
		},
		{
			name: "a directive with a quoted > and a quote, then a long tag",
			body: `<!DOCTYPE x '> <a "' ><A><R x="" y=">" z="` + long + `"/></A>`, wantErr: true,
		},
	}

	// A tag whose < sits at each offset around the decoder's 4 KiB read buffer,
	// so its first two bytes and its last arrive in different reads.
	offsets := []int{4094, 4095, 4096, 4097}
	edges := make([]tagCase, 0, 2*len(offsets))

	for _, at := range offsets {
		pad := "<A>" + strings.Repeat("t", at-len("<A>"))
		edges = append(edges,
			tagCase{name: fmt.Sprintf("a tag of MaxTag at offset %d", at), body: pad + tagOf(restxml.MaxTag) + "</A>"},
			tagCase{
				name: fmt.Sprintf("a tag of MaxTag+1 at offset %d", at), body: pad + tagOf(restxml.MaxTag+1) + "</A>",
				wantErr: true,
			},
		)
	}

	tests = slices.Concat(tests, edges)

	readers := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{name: "whole", wrap: func(r io.Reader) io.Reader { return r }},
		{name: "one byte at a time", wrap: iotest.OneByteReader},
		{name: "EOF with the last bytes", wrap: iotest.DataErrReader},
		{name: "half at a time", wrap: iotest.HalfReader},
	}

	for _, rd := range readers {
		for _, tt := range tests {
			t.Run(rd.name+"/"+tt.name, func(t *testing.T) {
				d := restxml.NewDecoder(rd.wrap(strings.NewReader(tt.body)))

				if _, ok := d.Root(); ok {
					walk(d, 0)
				}

				if !tt.wantErr {
					require.NoError(t, d.Err())

					return
				}

				require.ErrorIs(t, d.Err(), restxml.ErrMalformed)
				assert.ErrorContains(t, d.Err(), "a tag is longer than")
			})
		}
	}
}
