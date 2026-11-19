package restxml

import (
	"bytes"
	"fmt"
	"io"
)

// MaxTag is the longest start or end tag a body may hold, from its < to its >,
// attributes included. encoding/xml builds a whole tag, every attribute in it,
// before a Decoder can charge it, so a tag is refused while it is read instead:
// one tag of 16 MiB of attributes would otherwise allocate about 15 times that.
// The longest tag an S3 request holds is a Grantee with its xmlns:xsi and
// xsi:type, under 100 bytes; 4 KiB leaves room for any namespace.
//
// Text is not bounded here: it costs what Charge counts for it, in proportion.
// Nor are comments, processing instructions, directives and CDATA (<! and <?),
// which encoding/xml holds as one copy of their text.
const MaxTag = 4 << 10

var errTagTooLong = fmt.Errorf("a tag is longer than %d bytes", MaxTag)

const (
	outside = iota // in text
	opened         // just after a <
	inTag          // in a start or end tag
	inQuote        // in a quoted attribute value
	inOther        // in <! or <?, until the next >
)

// tagLimit reads r and fails, delivering nothing more, once a tag passes
// MaxTag. It scans what it delivers with bytes.IndexByte and IndexAny, so text
// costs a vector search and a tag a few calls.
type tagLimit struct {
	r     io.Reader
	state uint8
	quote byte
	n     int // the bytes of the open tag so far
	err   error
}

func (t *tagLimit) Read(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}

	n, err := t.r.Read(p)
	if !t.scan(p[:n]) {
		t.err = errTagTooLong

		return 0, t.err
	}

	return n, err
}

// tagEnd is where in b the quote or tag the state is in may end.
func (t *tagLimit) tagEnd(b []byte) int {
	if t.state == inQuote {
		return bytes.IndexByte(b, t.quote)
	}

	return bytes.IndexAny(b, `"'>`)
}

// scan follows b through the tag states and reports false once a tag passes
// MaxTag.
func (t *tagLimit) scan(b []byte) bool {
	for i := 0; i < len(b); {
		var j int

		switch t.state {
		case outside:
			j = bytes.IndexByte(b[i:], '<')
			if j < 0 {
				return true
			}

			t.state, t.n = opened, 1
			i += j + 1

			continue
		case opened:
			t.state = inTag
			if b[i] == '!' || b[i] == '?' {
				t.state = inOther
			}

			continue
		case inTag, inQuote:
			j = t.tagEnd(b[i:])
		default: // inOther
			j = bytes.IndexByte(b[i:], '>')
			if j < 0 {
				return true
			}

			t.state = outside
			i += j + 1

			continue
		}

		if j < 0 {
			t.n += len(b) - i

			return t.n <= MaxTag
		}

		t.n += j + 1
		if t.n > MaxTag {
			return false
		}

		switch c := b[i+j]; {
		case t.state == inQuote:
			t.state = inTag
		case c == '>':
			t.state = outside
		default:
			t.state, t.quote = inQuote, c
		}

		i += j + 1
	}

	return true
}
