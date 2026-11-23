package value

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"sync"
	"unsafe"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// Packed is a stored item in one immutable string: a header of three uvarints,
// the item's size as ItemSize counts it, the count of its S and N values and
// the count of its attributes, then the item as json, byte for byte as the
// generated encoder writes an attribute value map on the wire. A read with
// nothing to evaluate writes the json into its response as it is (Encoded),
// and one with a filter or a projection decodes only the attributes it names
// (Attrs). The empty Packed is no item.
type Packed string

// itemPrefix and itemSuffix wrap the item in what GetItemOutput.AppendJSON
// writes for an output holding the item alone.
const (
	itemPrefix = `{"Item":`
	itemSuffix = `}`
)

// errPackShape is a GetItemOutput encoding that does not wrap the item as
// Pack expects: the generated encoder changed.
var errPackShape = errors.New("value: the encoded item is not wrapped as Pack expects")

// packScratch is the buffer Pack encodes into before it copies the packed
// item into a string of its own size.
var packScratch = sync.Pool{New: func() any { return new([]byte) }}

// Pack encodes item, which ValidateItem accepted. The error is one the
// generated encoder returns, a string that is not valid UTF-8, which a
// request's decoder refuses before an item gets here.
func Pack(item Item) (Packed, error) {
	if item == nil {
		return "", nil
	}

	bp, _ := packScratch.Get().(*[]byte)
	defer packScratch.Put(bp)

	// Sizes and counts are never negative.
	buf := binary.AppendUvarint((*bp)[:0], uint64(ItemSize(item))) //nolint:gosec // G115: see above
	buf = binary.AppendUvarint(buf, uint64(countScalars(item)))    //nolint:gosec // G115: see above
	buf = binary.AppendUvarint(buf, uint64(len(item)))
	head := len(buf)

	out := api.GetItemOutput{Item: item}

	buf, err := out.AppendJSON(buf)
	*bp = buf

	if err != nil {
		return "", err
	}

	body := buf[head:]
	if !strings.HasPrefix(string(body), itemPrefix) || !strings.HasSuffix(string(body), itemSuffix) {
		return "", errPackShape
	}

	buf = append(buf[:head], body[len(itemPrefix):len(body)-len(itemSuffix)]...)

	return Packed(buf), nil
}

// countScalars counts the S and N values of item, at any depth.
func countScalars(item Item) int {
	n := 0
	for _, v := range item {
		n += scalarsOf(&v)
	}

	return n
}

func scalarsOf(v *api.AttributeValue) int {
	switch {
	case v.S != nil, v.N != nil:
		return 1
	case v.M != nil:
		return countScalars(v.M)
	case v.L != nil:
		n := 0
		for i := range v.L {
			n += scalarsOf(&v.L[i])
		}

		return n
	default:
		return 0
	}
}

// packHeader is what the header of a Packed holds, and where its json starts.
type packHeader struct {
	size, scalars, attrs, body int
}

// uvarintBits are the value bits of a byte of a uvarint; the byte above them
// marks one that another follows.
const (
	uvarintBits = 0x7f
	uvarintMore = 0x80
)

func uvarint(s string, i int) (int, int) {
	v := 0

	for shift := 0; ; shift += 7 {
		c := s[i]
		i++

		v |= int(c&uvarintBits) << shift
		if c < uvarintMore {
			return v, i
		}
	}
}

// Size is the item's size as ItemSize counts it, 0 for no item.
func (p Packed) Size() int {
	if p == "" {
		return 0
	}

	size, _ := uvarint(string(p), 0)

	return size
}

// JSON is the item as the wire writes it, "" for no item.
func (p Packed) JSON() string {
	if p == "" {
		return ""
	}

	return string(p[p.header().body:])
}

// Encoded is the item's json as a response's pre-encoded member takes it. The
// bytes are the packed string's own, which never changes: the writer reads
// them and nothing writes to them.
func (p Packed) Encoded() awsjson.Encoded {
	j := p.JSON()
	if j == "" {
		return nil
	}

	return unsafe.Slice(unsafe.StringData(j), len(j)) //nolint:gosec // G103: read only, see above
}

// Unpack decodes p into a fresh item, nil for no item. Its strings share the
// bytes of p, and its S and N values point into one slice the item owns.
func Unpack(p Packed) Item {
	if p == "" {
		return nil
	}

	h := p.header()
	u := unpacker{s: string(p), i: h.body, strs: make([]string, 0, h.scalars)}

	return u.object(h.attrs, nil)
}

// UnpackJSON decodes the json of an item as Encoded returns it.
func UnpackJSON(j []byte) Item {
	if len(j) == 0 {
		return nil
	}

	u := unpacker{s: string(j)}

	return u.object(0, nil)
}

// Attrs decodes the attributes of p that names lists, and leaves the others
// out, as an item that held only those would be. names must not repeat a
// name. It returns nil for no item.
func (p Packed) Attrs(names []string) Item {
	if p == "" {
		return nil
	}

	u := unpacker{s: string(p), i: p.header().body}

	return u.object(len(names), names)
}

// header reads the header of p.
func (p Packed) header() packHeader {
	var h packHeader

	h.size, h.body = uvarint(string(p), 0)
	h.scalars, h.body = uvarint(string(p), h.body)
	h.attrs, h.body = uvarint(string(p), h.body)

	return h
}

// unpacker reads the json of a packed item, which the generated encoder wrote:
// no space, each attribute value an object of one member.
type unpacker struct {
	s    string
	i    int
	strs []string
}

// object reads a map of attribute values, all of them or, when want is not
// nil, those it names; hint sizes the map.
func (u *unpacker) object(hint int, want []string) Item {
	m := make(Item, hint)
	u.i++ // {

	if u.s[u.i] == '}' {
		u.i++

		return m
	}

	found := 0

	for {
		name := u.str()
		u.i++ // :

		if want == nil || (found < len(want) && slices.Contains(want, name)) {
			m[name] = u.value()
			found++
		} else {
			u.skip()
		}

		c := u.s[u.i]
		u.i++

		if c == '}' {
			return m
		}

		if want != nil && found == len(want) {
			return m
		}
	}
}

// value reads one attribute value.
func (u *unpacker) value() api.AttributeValue {
	u.i += len(`{"`)
	end := u.i + strings.IndexByte(u.s[u.i:], '"')
	kind := u.s[u.i:end]
	u.i = end + len(`":`)

	var v api.AttributeValue

	switch kind {
	case TypeS:
		v.S = u.ptr(u.str())
	case TypeN:
		v.N = u.ptr(u.str())
	case TypeB:
		v.B = u.blob()
	case TypeSS:
		v.SS = u.strings()
	case TypeNS:
		v.NS = u.strings()
	case TypeBS:
		v.BS = u.blobs()
	case TypeM:
		v.M = u.object(0, nil)
	case TypeL:
		v.L = u.list()
	case TypeNULL:
		v.NULL = u.boolean()
	case TypeBOOL:
		v.BOOL = u.boolean()
	default:
	}

	u.i++ // }

	return v
}

// ptr keeps s in the item's slice of strings and points at it. The slice is
// made at the count of the item's S and N values, so it never moves; a partial
// read that outgrows it leaves the strings already taken where they are.
func (u *unpacker) ptr(s string) *string {
	u.strs = append(u.strs, s)

	return &u.strs[len(u.strs)-1]
}

var (
	packedTrue  = true
	packedFalse = false
)

func (u *unpacker) boolean() *bool {
	if u.s[u.i] == 't' {
		u.i += len("true")

		return &packedTrue
	}

	u.i += len("false")

	return &packedFalse
}

// str reads a json string: the bytes between its quotes when it has no
// escape, shared, and its decoded text otherwise.
func (u *unpacker) str() string {
	start := u.i + 1
	end := start + strings.IndexByte(u.s[start:], '"')

	raw := u.s[start:end]
	if strings.IndexByte(raw, '\\') < 0 {
		u.i = end + 1

		return raw
	}

	k := start
	for u.s[k] != '"' {
		if u.s[k] == '\\' {
			k++
		}

		k++
	}

	u.i = k + 1

	out, _ := jsontext.AppendUnquote(nil, u.s[start-1:k+1])

	return string(out)
}

func (u *unpacker) blob() []byte {
	b, _ := base64.StdEncoding.DecodeString(u.str())

	return b
}

func (u *unpacker) strings() []string {
	u.i++ // [

	out := []string{}

	if u.s[u.i] == ']' {
		u.i++

		return out
	}

	for {
		out = append(out, u.str())
		c := u.s[u.i]
		u.i++

		if c == ']' {
			return out
		}
	}
}

func (u *unpacker) blobs() [][]byte {
	u.i++ // [

	out := [][]byte{}

	if u.s[u.i] == ']' {
		u.i++

		return out
	}

	for {
		out = append(out, u.blob())
		c := u.s[u.i]
		u.i++

		if c == ']' {
			return out
		}
	}
}

func (u *unpacker) list() []api.AttributeValue {
	u.i++ // [

	out := []api.AttributeValue{}

	if u.s[u.i] == ']' {
		u.i++

		return out
	}

	for {
		out = append(out, u.value())
		c := u.s[u.i]
		u.i++

		if c == ']' {
			return out
		}
	}
}

// skip steps over one attribute value, an object, and what it holds.
func (u *unpacker) skip() {
	depth := 0

	for {
		switch u.s[u.i] {
		case '"':
			u.skipString()

			continue
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		default:
		}

		u.i++

		if depth == 0 {
			return
		}
	}
}

// skipString steps over the string at u.i.
func (u *unpacker) skipString() {
	k := u.i + 1

	for {
		k += strings.IndexByte(u.s[k:], '"')

		backslashes := 0
		for j := k - 1; u.s[j] == '\\'; j-- {
			backslashes++
		}

		if backslashes%2 == 0 {
			u.i = k + 1

			return
		}

		k++
	}
}
