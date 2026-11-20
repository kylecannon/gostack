package restxml

import (
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

// limitBuffer is how much of the body tokenLimit reads at once, bufio's
// default, which is what encoding/xml would read through without it.
const limitBuffer = 4 << 10

// maxEmptyReads is how many reads in a row may return nothing before
// tokenLimit gives up, as bufio does.
const maxEmptyReads = 100

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
//
// It buffers the body itself, so that a byte costs encoding/xml what a
// bufio.Reader's would: ReadByte compares the read index with stop, the index
// up to which neither the buffer's end, the token's limit nor its first two
// bytes need a look, and everything else is done in slowByte.
type tokenLimit struct {
	r    io.Reader
	err  error // what r returned with the bytes buffered last, not yet reported
	buf  []byte
	pos  int   // the index in buf of the next byte to deliver
	fill int   // how many bytes of buf hold the body
	stop int   // ReadByte delivers buf[pos] without a check while pos < stop
	base int64 // the body offset of buf[0]

	start int64 // the offset the current token starts at
	end   int64 // the offset no byte of the current token may reach
	last  byte  // the byte delivered before buf[0]
	lt    bool  // whether the current token starts with <
	known bool  // whether the current token's kind is read
}

func newTokenLimit(r io.Reader) *tokenLimit {
	return &tokenLimit{r: r, buf: make([]byte, limitBuffer), end: math.MaxInt64}
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
	if t.pos < t.stop {
		b := t.buf[t.pos]
		t.pos++

		return b, nil
	}

	return t.slowByte()
}

// slowByte delivers the next byte when ReadByte cannot: the buffer is spent,
// the token's limit is near, or the byte tells the token's kind.
func (t *tokenLimit) slowByte() (byte, error) {
	if t.pos >= t.fill {
		err := t.refill()
		if err != nil {
			return 0, err
		}
	}

	offset := t.base + int64(t.pos)
	if offset >= t.end {
		return 0, errTagTooLong
	}

	b := t.buf[t.pos]
	if !t.known {
		pos := offset - t.start
		if pos < kindBytes {
			t.see(b, pos)
		}

		t.known = pos >= kindBytes-1
	}

	t.pos++
	t.setStop()

	return b, nil
}

// refill reads the next part of the body into the buffer, keeping the byte
// delivered last.
func (t *tokenLimit) refill() error {
	if t.err != nil {
		err := t.err
		t.err = nil

		return err
	}

	if t.fill > 0 {
		t.last = t.buf[t.fill-1]
	}

	t.base += int64(t.fill)
	t.pos, t.fill, t.stop = 0, 0, 0

	for range maxEmptyReads {
		n, err := t.r.Read(t.buf)
		if n > 0 {
			t.fill, t.err = n, err

			return nil
		}

		if err != nil {
			return err
		}
	}

	return io.ErrNoProgress
}

// setStop sets how far ReadByte may deliver from the buffer unchecked: not
// while the current token's kind is unread, and not up to its limit.
func (t *tokenLimit) setStop() {
	if !t.known {
		t.stop = t.pos

		return
	}

	t.stop = t.fill
	if room := t.end - t.base; room < int64(t.stop) {
		t.stop = int(max(room, 0))
	}
}

// begin marks the start of the next token at offset, the xml.Decoder's
// InputOffset. The byte at offset was delivered already when encoding/xml put
// it back after the text before it. When the token's first two bytes are in
// the buffer, its kind is read from there, so ReadByte delivers it unchecked;
// otherwise slowByte reads the kind as the bytes are delivered.
func (t *tokenLimit) begin(offset int64) {
	t.start, t.end, t.lt, t.known = offset, math.MaxInt64, false, false

	if i := offset - t.base; i >= 0 && i+kindBytes <= int64(t.fill) {
		t.see(t.buf[i], 0)
		t.see(t.buf[i+1], 1)
		t.known = true
	} else if t.base+int64(t.pos) > offset {
		t.see(t.delivered(), 0)
	}

	t.setStop()
}

// delivered is the byte ReadByte delivered last.
func (t *tokenLimit) delivered() byte {
	if t.pos > 0 {
		return t.buf[t.pos-1]
	}

	return t.last
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
