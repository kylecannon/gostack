package value

import (
	"encoding/json/jsontext"
	"slices"
	"strings"
)

// Orderer writes packed items with the names of every map in byte order, as
// the DynamoDB snapshot holds them, from the packed json: the values are
// copied as the encoder wrote them, and only a map's members are put in
// order. A packed item's names are in map order, so the same item can pack
// two ways; ordered, it gives the same bytes each time. It keeps its scratch
// between calls and is not safe for concurrent use.
type Orderer struct {
	members []member
}

// member is one name and value of a map in the packed json: the name decoded
// for the order, and the name and value as the encoder wrote them.
type member struct {
	name, raw, value string
}

// ordererMembers sizes the scratch of an Orderer, which holds the members of
// a map and of every map it is inside.
const ordererMembers = 64

// Append appends the json of p to dst with each map's names in order. It
// appends nothing for no item.
func (o *Orderer) Append(dst []byte, p Packed) []byte {
	if p == "" {
		return dst
	}

	if o.members == nil {
		o.members = make([]member, 0, ordererMembers)
	}

	return o.object(dst, string(p), p.header().body)
}

// object appends the map whose json starts at s[i]. Its members are kept
// above those of the maps it is inside, so one scratch serves every depth.
func (o *Orderer) object(dst []byte, s string, i int) []byte {
	base := len(o.members)
	u := unpacker{s: s, i: i + 1} // {

	if s[u.i] != '}' {
		for {
			raw, val := u.rawMember()
			o.members = append(o.members, member{name: memberName(raw), raw: raw, value: val})

			if s[u.i] == '}' {
				break
			}

			u.i++ // ,
		}
	}

	slices.SortFunc(o.members[base:], func(a, b member) int { return strings.Compare(a.name, b.name) })

	dst = append(dst, '{')

	for k := base; k < len(o.members); k++ {
		if k > base {
			dst = append(dst, ',')
		}

		m := o.members[k]
		dst = append(dst, m.raw...)
		dst = append(dst, ':')
		dst = o.value(dst, m.value)
	}

	o.members = o.members[:base]

	return append(dst, '}')
}

// value appends one attribute value, v, putting the names of a map in it in
// order. Every other value is copied as it is.
func (o *Orderer) value(dst []byte, v string) []byte {
	switch {
	case strings.HasPrefix(v, `{"M":`):
		dst = append(dst, `{"M":`...)
		return append(o.object(dst, v, len(`{"M":`)), '}')
	case strings.HasPrefix(v, `{"L":`):
		return o.list(dst, v)
	default:
		return append(dst, v...)
	}
}

// list appends a list value, v, putting the names of each map in it in
// order.
func (o *Orderer) list(dst []byte, v string) []byte {
	dst = append(dst, `{"L":[`...)
	u := unpacker{s: v, i: len(`{"L":[`)}

	if v[u.i] != ']' {
		for {
			from := u.i
			u.skip()
			dst = o.value(dst, v[from:u.i])

			if v[u.i] == ']' {
				break
			}

			dst = append(dst, ',')
			u.i++ // ,
		}
	}

	return append(dst, "]}"...)
}

// memberName is the name a quoted member name, raw, holds: the bytes between
// its quotes when it has no escape, and its decoded text otherwise.
func memberName(raw string) string {
	if strings.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1]
	}

	out, _ := jsontext.AppendUnquote(nil, raw)

	return string(out)
}
