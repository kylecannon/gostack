package value

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"slices"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// JSONPacker packs an item from its json without decoding it into typed
// values, for a restore, whose snapshot holds each item as the encoder writes
// it. It scans the json, checks it as ValidateItem checks an item, and counts
// what the header of a Packed holds; the json is then the packed json as it
// is. It takes only the json Pack itself would write: no space, each string
// quoted as the encoder quotes it, each number and binary in canonical form.
// Any other json, valid or not, it leaves to the caller, who decodes it,
// validates it and packs the item. It keeps its scratch between calls and is
// not safe for concurrent use.
type JSONPacker struct {
	buf, text, quoted []byte
	set               []string
}

// Pack packs the item whose json is raw, and reports false when raw is not
// json Pack would write for an item ValidateItem accepts. The Packed is its
// own: it shares no byte of raw.
func (k *JSONPacker) Pack(raw []byte) (Packed, bool) {
	if len(raw) == 0 {
		return "", false
	}

	// The scan only reads raw, which the caller does not change meanwhile.
	sc := jsonScan{k: k, s: stringOf(raw)}

	size, attrs, ok := sc.object(1)
	if !ok || sc.i != len(sc.s) {
		return "", false
	}

	k.buf = appendHeader(k.buf[:0], size, sc.scalars, attrs)
	k.buf = append(k.buf, raw...)

	return Packed(k.buf), true
}

// stringOf is b as a string, for a caller that only reads it while b does
// not change.
func stringOf(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b)) //nolint:gosec // G103: see above
}

// bytesOf is s as bytes, for a caller that only reads them.
func bytesOf(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s)) //nolint:gosec // G103: see above
}

// jsonScan reads the json of one item, s, from s[i], adding up the S and N
// values it meets.
type jsonScan struct {
	k       *JSONPacker
	s       string
	i       int
	scalars int
}

// next takes c if it is the byte at i.
func (sc *jsonScan) next(c byte) bool {
	if sc.i < len(sc.s) && sc.s[sc.i] == c {
		sc.i++

		return true
	}

	return false
}

// word takes w if it comes next.
func (sc *jsonScan) word(w string) bool {
	if strings.HasPrefix(sc.s[sc.i:], w) {
		sc.i += len(w)

		return true
	}

	return false
}

// object reads a map of attribute values whose values are at depth, and
// returns its size as ItemSize counts it and its count of attributes.
func (sc *jsonScan) object(depth int) (int, int, bool) {
	if !sc.next('{') {
		return 0, 0, false
	}

	if sc.next('}') {
		return 0, 0, true
	}

	size, count := 0, 0

	for {
		n, ok := sc.str()
		if !ok || n == 0 || !sc.next(':') {
			return 0, 0, false
		}

		v, ok := sc.value(depth)
		if !ok {
			return 0, 0, false
		}

		size += n + v
		count++

		if sc.next('}') {
			return size, count, true
		}

		if !sc.next(',') {
			return 0, 0, false
		}
	}
}

// value reads one attribute value at depth and returns its size as Size
// counts it.
func (sc *jsonScan) value(depth int) (int, bool) {
	const (
		containerOverhead = 3
		oneByte           = 1
	)

	if depth > MaxDepth || !sc.next('{') || !sc.next('"') {
		return 0, false
	}

	end := strings.IndexByte(sc.s[sc.i:], '"')
	if end < 0 {
		return 0, false
	}

	kind := sc.s[sc.i : sc.i+end]
	sc.i += end + 1

	if !sc.next(':') {
		return 0, false
	}

	var (
		size int
		ok   bool
	)

	switch kind {
	case TypeS:
		size, ok = sc.str()
		sc.scalars++
	case TypeN:
		size, ok = sc.number()
		sc.scalars++
	case TypeB:
		size, ok = sc.blob()
	case TypeSS:
		size, ok = sc.set(sc.str)
	case TypeNS:
		size, ok = sc.set(sc.number)
	case TypeBS:
		size, ok = sc.set(sc.blob)
	case TypeM:
		var count int

		size, count, ok = sc.object(depth + 1)
		size += containerOverhead + count
	case TypeL:
		size, ok = sc.list(depth + 1)
	case TypeNULL:
		size, ok = oneByte, sc.word("true")
	case TypeBOOL:
		size, ok = oneByte, sc.word("true") || sc.word("false")
	default:
	}

	return size, ok && sc.next('}')
}

// quotedAt reads the json string at i, quotes included, and reports whether
// it has an escape.
func (sc *jsonScan) quotedAt() (string, bool, bool) {
	start := sc.i
	if !sc.next('"') {
		return "", false, false
	}

	escaped := false

	for {
		end := strings.IndexByte(sc.s[sc.i:], '"')
		if end < 0 {
			return "", false, false
		}

		slash := strings.IndexByte(sc.s[sc.i:sc.i+end], '\\')
		if slash < 0 {
			sc.i += end + 1

			return sc.s[start:sc.i], escaped, true
		}

		escaped = true
		sc.i += slash + len(`\"`) // the backslash and the byte it escapes

		if sc.i > len(sc.s) {
			return "", false, false
		}
	}
}

// str reads a string the encoder quoted, and returns the length of its text.
// A string with an escape is decoded and quoted again: it must come back as
// it was.
func (sc *jsonScan) str() (int, bool) {
	quoted, escaped, ok := sc.quotedAt()
	if !ok {
		return 0, false
	}

	inner := quoted[1 : len(quoted)-1]
	if !escaped {
		return len(inner), utf8.ValidString(inner)
	}

	k := sc.k

	var err error

	k.text, err = jsontext.AppendUnquote(k.text[:0], quoted)
	if err != nil || !utf8.Valid(k.text) {
		return 0, false
	}

	k.quoted = awsjson.AppendQuote(k.quoted[:0], stringOf(k.text))

	return len(k.text), string(k.quoted) == quoted
}

// number reads a number in canonical form, and returns its size.
func (sc *jsonScan) number() (int, bool) {
	quoted, escaped, ok := sc.quotedAt()
	if !ok || escaped {
		return 0, false
	}

	text := quoted[1 : len(quoted)-1]

	n, err := ParseNumber(text)
	if err != nil || n.String() != text {
		return 0, false
	}

	return n.size(), true
}

// strictBase64 refuses base64 whose padding bits are set, which encodes the
// same bytes as the canonical form.
var strictBase64 = base64.StdEncoding.Strict()

// blob reads a binary in canonical base64, and returns the length of its
// bytes.
func (sc *jsonScan) blob() (int, bool) {
	quoted, escaped, ok := sc.quotedAt()
	if !ok || escaped {
		return 0, false
	}

	k := sc.k
	text := quoted[1 : len(quoted)-1]

	var err error

	k.text, err = strictBase64.AppendDecode(k.text[:0], bytesOf(text))
	if err != nil {
		return 0, false
	}

	return len(k.text), true
}

// set reads a set whose elements one reads, and returns their sizes summed.
// A set is not empty and holds no element twice; elements in canonical form
// are equal when their json is.
func (sc *jsonScan) set(one func() (int, bool)) (int, bool) {
	if !sc.next('[') || sc.next(']') {
		return 0, false
	}

	k := sc.k
	k.set = k.set[:0]
	size := 0

	for {
		start := sc.i

		n, ok := one()
		if !ok {
			return 0, false
		}

		k.set = append(k.set, sc.s[start:sc.i])
		size += n

		if sc.next(']') {
			break
		}

		if !sc.next(',') {
			return 0, false
		}
	}

	slices.Sort(k.set)

	unique := true

	for i := 1; i < len(k.set); i++ {
		if k.set[i] == k.set[i-1] {
			unique = false
		}
	}

	clear(k.set) // the elements are views of raw, which the caller reuses

	return size, unique
}

// list reads a list whose elements are at depth, and returns its size.
func (sc *jsonScan) list(depth int) (int, bool) {
	const containerOverhead = 3

	if !sc.next('[') {
		return 0, false
	}

	size := containerOverhead
	if sc.next(']') {
		return size, true
	}

	for {
		n, ok := sc.value(depth)
		if !ok {
			return 0, false
		}

		size += n + 1

		if sc.next(']') {
			return size, true
		}

		if !sc.next(',') {
			return 0, false
		}
	}
}
