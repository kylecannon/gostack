package value

import (
	"encoding/base64"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// EqualPacked reports whether p holds the attributes of item, as EqualItems
// compares two items, without unpacking p: it reads p's json in place and
// looks each name up in item. It decodes only a name or a string written with
// an escape, and the members of a set, whose order the two need not share.
// The empty Packed equals only a nil or empty item.
func EqualPacked(p Packed, item Item) bool {
	if p == "" {
		return len(item) == 0
	}

	h := p.header()
	if h.attrs != len(item) {
		return false
	}

	u := unpacker{s: string(p), i: h.body}

	return u.equalObject(item)
}

// equalObject reports whether the object at u.i holds the attributes of m.
// It leaves u.i past the object only when it reports true.
func (u *unpacker) equalObject(m Item) bool {
	u.i++ // {

	if u.s[u.i] == '}' {
		u.i++

		return len(m) == 0
	}

	// A map holds a name once, so every member found in m, and as many
	// members as m holds, is m.
	members := 0

	for {
		name := u.str()
		u.i++ // :

		v, ok := m[name]
		if !ok || !u.equalValue(&v) {
			return false
		}

		members++

		c := u.s[u.i]
		u.i++

		if c == '}' {
			return members == len(m)
		}
	}
}

// equalValue reports whether the attribute value at u.i is Equal to v. It
// leaves u.i past the value only when it reports true.
func (u *unpacker) equalValue(v *api.AttributeValue) bool {
	u.i += len(`{"`)
	end := u.i + strings.IndexByte(u.s[u.i:], '"')
	kind := u.s[u.i:end]
	u.i = end + len(`":`)

	if kind != Type(v) {
		return false
	}

	var equal bool

	switch kind {
	case TypeS:
		equal = u.str() == *v.S
	case TypeN:
		equal = u.str() == *v.N
	case TypeB:
		equal = u.equalBlob(v.B)
	case TypeSS:
		equal = sameStrings(u.strings(), v.SS)
	case TypeNS:
		equal = sameStrings(u.strings(), v.NS)
	case TypeBS:
		equal = sameBytes(u.blobs(), v.BS)
	case TypeM:
		equal = u.equalObject(v.M)
	case TypeL:
		equal = u.equalList(v.L)
	case TypeNULL:
		u.boolean()

		equal = true
	case TypeBOOL:
		equal = *u.boolean() == *v.BOOL
	default: // no type a packed item holds
		return false
	}

	if !equal {
		return false
	}

	u.i++ // }

	return true
}

// equalList reports whether the list at u.i holds the elements of l, in
// order.
func (u *unpacker) equalList(l []api.AttributeValue) bool {
	u.i++ // [

	if u.s[u.i] == ']' {
		u.i++

		return len(l) == 0
	}

	for i := 0; ; i++ {
		if i == len(l) || !u.equalValue(&l[i]) {
			return false
		}

		c := u.s[u.i]
		u.i++

		if c == ']' {
			return i+1 == len(l)
		}
	}
}

// blobChunk is the bytes of a binary equalBlob encodes at a time: a multiple
// of 3, so each chunk but the last encodes to base64 with no padding.
const blobChunk = 384

// equalBlob reports whether the binary at u.i is b. The packed json holds a
// binary in canonical base64, as the encoder writes it and as JSONPacker
// takes it, so b is encoded and compared, a chunk at a time on the stack.
func (u *unpacker) equalBlob(b []byte) bool {
	start := u.i + 1
	end := start + strings.IndexByte(u.s[start:], '"')
	text := u.s[start:end]
	u.i = end + 1

	if len(text) != base64.StdEncoding.EncodedLen(len(b)) {
		return false
	}

	var buf [blobChunk / 3 * 4]byte

	for len(b) > 0 {
		n := min(len(b), blobChunk)
		k := base64.StdEncoding.EncodedLen(n)
		base64.StdEncoding.Encode(buf[:k], b[:n])

		if string(buf[:k]) != text[:k] {
			return false
		}

		b, text = b[n:], text[k:]
	}

	return true
}
