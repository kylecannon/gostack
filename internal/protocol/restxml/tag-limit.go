package restxml

import (
	"bufio"
	"fmt"
	"io"
	"math"
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
// which encoding/xml holds as one copy of their text, charged the same way.
const MaxTag = 4 << 10

// kindBytes is how many bytes of a token tell whether it is a tag.
const kindBytes = 2

var errTagTooLong = fmt.Errorf("a tag is longer than %d bytes", MaxTag)

// tokenLimit is the io.ByteReader encoding/xml reads a body through. Before
// each RawToken the Decoder marks where the token starts, at the xml.Decoder's
// own InputOffset (begin); once that token is a start or end tag and its span
// passes MaxTag, ReadByte fails and the token is never built.
//
// Where a token starts is the xml.Decoder's own account, so no second parser
// has to agree with it on where a comment, CDATA section, processing
// instruction or directive ends. Whether a token is a tag is read from its
// first two bytes, the way encoding/xml dispatches on them: < followed by
// anything but ! or ?.
type tokenLimit struct {
	br    *bufio.Reader
	n     int64 // the bytes delivered
	start int64 // the offset the current token starts at
	end   int64 // the offset no byte of the current token may reach
	last  byte  // the byte delivered last
	lt    bool  // whether the current token starts with <
}

func newTokenLimit(r io.Reader) *tokenLimit {
	return &tokenLimit{br: bufio.NewReader(r), end: math.MaxInt64}
}

// Read is there so tokenLimit is an io.Reader; encoding/xml reads it only
// through ReadByte.
func (t *tokenLimit) Read(p []byte) (int, error) {
	n := 0

	for n < len(p) {
		b, err := t.ReadByte()
		if err != nil {
			return n, err
		}

		p[n] = b
		n++
	}

	return n, nil
}

func (t *tokenLimit) ReadByte() (byte, error) {
	if t.n >= t.end {
		return 0, errTagTooLong
	}

	b, err := t.br.ReadByte()
	if err != nil {
		return 0, err
	}

	if pos := t.n - t.start; pos < kindBytes {
		t.see(b, pos)
	}

	t.n++
	t.last = b

	return b, nil
}

// begin marks the start of the next token at offset, the xml.Decoder's
// InputOffset. The byte at offset was delivered already when encoding/xml put
// it back after the text before it.
func (t *tokenLimit) begin(offset int64) {
	t.start, t.end, t.lt = offset, math.MaxInt64, false
	if t.n > offset {
		t.see(t.last, 0)
	}
}

// see classifies the current token by b, its byte at pos.
func (t *tokenLimit) see(b byte, pos int64) {
	switch {
	case pos == 0:
		t.lt = b == '<'
	case t.lt && b != '!' && b != '?':
		t.end = t.start + MaxTag
	default:
		// Text, or <! and <?: not a tag.
	}
}
