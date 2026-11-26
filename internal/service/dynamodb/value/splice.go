package value

import (
	"slices"
	"strings"
	"unsafe"
)

// Splice returns stored with the top-level attributes names lists written
// anew: values holds the new value of each one the item keeps, and a name it
// lacks is removed. Every other attribute is copied as the encoder wrote it,
// so a write of a few attributes costs about one copy of the item, not a
// decode and an encode of all of it. Only values is encoded.
//
// names is sorted and holds each name once, and values names none outside
// it. The result is what Pack returns for the item so changed, except for the
// order of its members, which Pack takes from the map: the attributes copied
// keep their order and the ones values holds follow them. It returns "" for
// no stored item, and stored itself when names reach no attribute it holds
// and values is empty. The error is Pack's, for a string in values that is
// not valid UTF-8.
//
// Splice decodes the old values of names to size the item. A caller that
// knows the size calls SpliceSized, which decodes none of them.
//
// A splice is not always cheaper than Pack: it walks the old values of names
// and encodes values, through a map whose every entry is allocated. When to
// splice and when to pack is decided by spliceWorth in
// expression/update-apply.go, which counts both.
func Splice(stored Packed, names []string, values Item) (Packed, error) {
	if stored == "" {
		return "", nil
	}

	// Attrs of no names would decode every attribute.
	var old Item
	if len(names) > 0 {
		old = attrsSorted(stored, names)
	}

	size := stored.header().size - ItemSize(old) + ItemSize(values)

	return SpliceSized(stored, names, values, size)
}

// SpliceSized is Splice of an item whose size once spliced, as ItemSize counts
// it, is size. The rest of the header is counted from the json: the
// attributes from the members it keeps and adds, and the S and N values from
// those it adds and the ones it removes, which it steps over counting them.
func SpliceSized(stored Packed, names []string, values Item, size int) (Packed, error) {
	if stored == "" {
		return "", nil
	}

	h := stored.header()
	body := string(stored[h.body:])

	var inline [spliceRunsInline]span

	runs, old := spliceRuns(inline[:0], body, names)
	if old.attrs == 0 && len(values) == 0 {
		return stored, nil
	}

	var shape packShape

	shape.item(values)

	next := packHeader{
		size:    size,
		scalars: h.scalars - old.scalars + shape.scalars,
		attrs:   h.attrs - old.attrs + len(values),
	}

	bp, _ := packScratch.Get().(*[]byte)
	defer packScratch.Put(bp)

	added, err := spliceAdded(bp, values, ItemSize(values), shape)
	if err != nil {
		return "", err
	}

	return spliceJoin(next, body, runs, added), nil
}

// spliceAdded encodes values into the scratch buffer *bp as Pack encodes an
// item, and returns the members between the braces of its object, which stay
// valid until *bp is next written. size and shape are those of values.
func spliceAdded(bp *[]byte, values Item, size int, shape packShape) (string, error) {
	if len(values) == 0 {
		return "", nil
	}

	buf, packed, err := packInto((*bp)[:0], values, size, shape.scalars, size+shape.extra+packWrapping)
	*bp = buf

	if err != nil {
		return "", err
	}

	json := packed[headerLen(size, shape.scalars, len(values)):]

	return unsafe.String(unsafe.SliceData(json[1:]), len(json)-len("{}")), nil //nolint:gosec // G103: see above
}

// span is the bytes of a string at from and up to, not including, to.
type span struct {
	from, to int
}

// spliceRunsInline is how many runs of kept members a splice holds on the
// stack. A splice that removes fewer attributes than that leaves at most that
// many runs, one more than it removes.
const spliceRunsInline = 8

// removed is what the members a splice removes held: how many there were and
// the count of S and N values in them, at any depth.
type removed struct {
	attrs, scalars int
}

// spliceRuns appends to runs the runs of consecutive members of the object
// body that names does not list, each from the first byte of its first member
// to the last of its last, and returns them with what the members names lists
// held.
func spliceRuns(runs []span, body string, names []string) ([]span, removed) {
	var old removed

	u := unpacker{s: body, i: 1} // {
	if body[u.i] == '}' {
		return runs, old
	}

	inRun := false

	for {
		start := u.i
		u.skipString()
		name := memberName(u.s[start:u.i])
		u.i++ // :

		_, written := slices.BinarySearch(names, name)

		switch {
		case written:
			old.attrs++
			old.scalars += u.scalars()
			inRun = false
		case inRun:
			u.skip()
			runs[len(runs)-1].to = u.i
		default:
			u.skip()
			runs = append(runs, span{from: start, to: u.i})
			inRun = true
		}

		if body[u.i] == '}' {
			return runs, old
		}

		u.i++ // ,
	}
}

// scalars steps over the attribute value at u.i, as skip does, and returns
// the count of S and N values in it at any depth, as packShape counts them.
func (u *unpacker) scalars() int {
	u.i += len(`{"`)
	end := u.i + strings.IndexByte(u.s[u.i:], '"')
	kind := u.s[u.i:end]
	u.i = end + len(`":`)

	n := 0

	switch kind {
	case TypeS, TypeN:
		u.skipString()

		n = 1
	case TypeM:
		n = u.mapScalars()
	case TypeL:
		n = u.listScalars()
	default:
		u.skipPlain()
	}

	u.i++ // }

	return n
}

// mapScalars steps over the map of attribute values at u.i and returns the
// count of S and N values in it.
func (u *unpacker) mapScalars() int {
	u.i++ // {

	n := 0

	if u.s[u.i] == '}' {
		u.i++

		return n
	}

	for {
		u.skipString()
		u.i++ // :

		n += u.scalars()
		c := u.s[u.i]
		u.i++

		if c == '}' {
			return n
		}
	}
}

// listScalars steps over the list of attribute values at u.i and returns the
// count of S and N values in it.
func (u *unpacker) listScalars() int {
	u.i++ // [

	n := 0

	if u.s[u.i] == ']' {
		u.i++

		return n
	}

	for {
		n += u.scalars()
		c := u.s[u.i]
		u.i++

		if c == ']' {
			return n
		}
	}
}

// skipPlain steps over a value that holds no attribute value: a string, a
// boolean, or a set, an array of strings.
func (u *unpacker) skipPlain() {
	switch u.s[u.i] {
	case '"':
		u.skipString()
	case '[':
		u.i++

		for u.s[u.i] != ']' {
			if u.s[u.i] == '"' {
				u.skipString()

				continue
			}

			u.i++ // ,
		}

		u.i++
	default:
		// true or false
		for u.s[u.i] != '}' {
			u.i++
		}
	}
}

// spliceJoin is the Packed of header h whose json is the runs of the stored
// json body, comma separated, and then the members added. Its length is
// counted first, so it is written in one allocation of its own size.
func spliceJoin(h packHeader, body string, runs []span, added string) Packed {
	var header [packHeaderMax]byte

	head := appendHeader(header[:0], h.size, h.scalars, h.attrs)

	n := len(head) + len("{}") + len(added)

	for i, r := range runs {
		n += r.to - r.from
		if i > 0 {
			n += len(",")
		}
	}

	if len(runs) > 0 && added != "" {
		n += len(",")
	}

	// A Builder grows into memory it does not clear first, which the writes
	// below fill whole.
	var out strings.Builder

	out.Grow(n)
	out.Write(head)
	out.WriteByte('{')

	for i, r := range runs {
		if i > 0 {
			out.WriteByte(',')
		}

		out.WriteString(body[r.from:r.to])
	}

	if added != "" {
		if len(runs) > 0 {
			out.WriteByte(',')
		}

		out.WriteString(added)
	}

	out.WriteByte('}')

	return Packed(out.String())
}

// Len is the count of the item's attributes, 0 for no item.
func (p Packed) Len() int {
	if p == "" {
		return 0
	}

	return p.header().attrs
}
