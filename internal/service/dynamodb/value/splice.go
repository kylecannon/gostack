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
// decode and an encode of all of it. Only the old values of names are
// decoded, for the header, and only values is encoded.
//
// names is sorted and holds each name once, and values names none outside
// it. The result is what Pack returns for the item so changed, except for the
// order of its members, which Pack takes from the map: the attributes copied
// keep their order and the ones values holds follow them. It returns "" for
// no stored item, and stored itself when names reach no attribute it holds
// and values is empty. The error is Pack's, for a string in values that is
// not valid UTF-8.
//
// A splice is not always cheaper than Pack: it decodes the old values of
// names and encodes values. When to splice and when to pack is decided by
// spliceWorth in expression/update-apply.go, which counts both.
func Splice(stored Packed, names []string, values Item) (Packed, error) {
	if stored == "" {
		return "", nil
	}

	h := stored.header()

	// Attrs of no names would decode every attribute.
	var old Item
	if len(names) > 0 {
		old = attrsSorted(stored, names)
	}

	if len(old) == 0 && len(values) == 0 {
		return stored, nil
	}

	var oldShape, shape packShape

	oldShape.item(old)
	shape.item(values)

	valuesSize := ItemSize(values)
	next := packHeader{
		size:    h.size - ItemSize(old) + valuesSize,
		scalars: h.scalars - oldShape.scalars + shape.scalars,
		attrs:   h.attrs - len(old) + len(values),
	}

	bp, _ := packScratch.Get().(*[]byte)
	defer packScratch.Put(bp)

	added, err := spliceAdded(bp, values, valuesSize, shape)
	if err != nil {
		return "", err
	}

	return spliceJoin(next, string(stored[h.body:]), names, added), nil
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

// spliceJoin is the Packed of header h whose json is the members of the
// stored json body that names does not list and then the members added.
func spliceJoin(h packHeader, body string, names []string, added string) Packed {
	var header [packHeaderMax]byte

	// A Builder grows into memory it does not clear first, which the copy
	// below writes over whole. The json fits in the stored json's length,
	// added's and a comma.
	var out strings.Builder

	grown := headerLen(h.size, h.scalars, h.attrs) + len(body) + len(added) + len(",")

	out.Grow(grown)
	out.Write(appendHeader(header[:0], h.size, h.scalars, h.attrs))
	out.WriteByte('{')

	kept := spliceKept(&out, body, names)

	if added != "" {
		if kept {
			out.WriteByte(',')
		}

		out.WriteString(added)
	}

	out.WriteByte('}')

	// The buffer is the Packed unless it is left holding much more than the
	// item, as when a large attribute was removed or replaced by a small one.
	// The slack is counted from what was asked for, as Pack counts it: the
	// runtime rounds a large allocation up to whole pages, which Cap reports,
	// and a 64 KiB item would be copied a second time for that rounding.
	if grown-out.Len() > out.Len()/packSlack {
		return Packed(strings.Clone(out.String()))
	}

	return Packed(out.String())
}

// spliceKept writes the members of the object body that names does not list,
// comma separated, as they are written in it, and reports whether there were
// any.
func spliceKept(out *strings.Builder, body string, names []string) bool {
	u := unpacker{s: body, i: 1} // {
	if body[u.i] == '}' {
		return false
	}

	kept := false

	for {
		start := u.i
		raw, _ := u.rawMember()

		if _, written := slices.BinarySearch(names, memberName(raw)); !written {
			if kept {
				out.WriteByte(',')
			}

			out.WriteString(body[start:u.i])
			kept = true
		}

		if body[u.i] == '}' {
			return kept
		}

		u.i++ // ,
	}
}

// Len is the count of the item's attributes, 0 for no item.
func (p Packed) Len() int {
	if p == "" {
		return 0
	}

	return p.header().attrs
}
