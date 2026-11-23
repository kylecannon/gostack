package value

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
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

	size := ItemSize(item)

	var shape packShape
	shape.item(item)

	// Sizes and counts are never negative.
	buf := binary.AppendUvarint((*bp)[:0], uint64(size))   //nolint:gosec // G115: see above
	buf = binary.AppendUvarint(buf, uint64(shape.scalars)) //nolint:gosec // G115: see above
	buf = binary.AppendUvarint(buf, uint64(len(item)))
	// A scratch buffer the pool lost, or one too small for the item, grows to
	// a little over the item's json size, not by append's steps, whose sum is
	// about five times a large item. It usually grows once: the json of a
	// string with control bytes, escaped to six bytes each, can outgrow it.
	buf = slices.Grow(buf, size+shape.extra+packWrapping)
	head := len(buf)

	out := api.GetItemOutput{Item: item}

	buf, err := out.AppendJSON(buf)
	*bp = buf

	if err != nil {
		return "", err
	}

	// The wrapping is checked on the bytes, and the item is copied once, into
	// the Packed: a string(body) here would copy it again.
	body := buf[head:]
	if !bytes.HasPrefix(body, []byte(itemPrefix)) || !bytes.HasSuffix(body, []byte(itemSuffix)) {
		return "", errPackShape
	}

	buf = append(buf[:head], body[len(itemPrefix):len(body)-len(itemSuffix)]...)

	return Packed(buf), nil
}

// The json Pack writes around what ItemSize counts, a little over: the quotes,
// type key and punctuation of a value with its name, of a set element, and of
// the output that wraps the item.
const (
	packPerValue   = 12
	packPerElement = 4
	packWrapping   = 64
)

// packShape is what one walk of an item finds: the count of its S and N
// values, at any depth, for the header, and the bytes its json adds to what
// ItemSize counts: punctuation, number digits, which ItemSize counts about
// two to a byte, and the third base64 adds to binary.
type packShape struct {
	scalars, extra int
}

// item adds the values of item. Each value is taken as the copy the range
// makes: a pointer to that copy would move it to the heap, one allocation for
// each member of each map.
func (s *packShape) item(item Item) {
	for _, v := range item {
		s.value(v)
	}
}

// value takes v by value, so that item's copy stays on the stack.
func (s *packShape) value(v api.AttributeValue) {
	s.extra += packPerValue

	switch {
	case v.S != nil:
		s.scalars++
	case v.N != nil:
		s.scalars++
		s.extra += numberExtra(*v.N)
	case v.M != nil:
		s.item(v.M)
	case v.L != nil:
		for i := range v.L {
			s.value(v.L[i])
		}
	case v.B != nil:
		s.extra += base64Extra(len(v.B))
	case v.SS != nil:
		s.extra += packPerElement * len(v.SS)
	case v.NS != nil:
		for _, n := range v.NS {
			s.extra += packPerElement + numberExtra(n)
		}
	case v.BS != nil:
		for _, b := range v.BS {
			s.extra += packPerElement + base64Extra(len(b))
		}
	case v.BOOL != nil:
		// ItemSize counts one byte for the json's true or false.
		s.extra += len("false")
	case v.NULL != nil:
		s.extra += len("true")
	default:
	}
}

// base64Extra is what base64 adds to n bytes, or a little over.
func base64Extra(n int) int {
	const pad = 2

	return n/3 + pad
}

// numberExtra is what the json of n adds to the size ItemSize counts for it,
// or over. ItemSize counts one byte for two significant digits, and at least
// one, so an exponent or zeros that it does not count, as in 1e125, can make
// the json all but len(n) bytes longer.
func numberExtra(n string) int {
	return len(n) - 1
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

	return u.object(h.attrs, nil, nil)
}

// UnpackJSON decodes the json of an item as Encoded returns it.
func UnpackJSON(j []byte) Item {
	if len(j) == 0 {
		return nil
	}

	u := unpacker{s: string(j)}

	return u.object(0, nil, nil)
}

// Attrs decodes the attributes of p that names lists, and leaves the others
// out, as an item that held only those would be. names must not repeat a
// name. It returns nil for no item.
func (p Packed) Attrs(names []string) Item {
	if p == "" {
		return nil
	}

	h := p.header()
	u := unpacker{s: string(p), i: h.body}

	return u.object(viewHint(h, names), names, nil)
}

// Number returns the text of the attribute name of p when it is a number, and
// false when p has no such attribute or it is of another type. For a name the
// encoder writes without escapes it reads the json in place and allocates
// nothing; another name is decoded as Attrs decodes it.
func (p Packed) Number(name string) (string, bool) {
	if p == "" {
		return "", false
	}

	if !plainName(name) {
		v, ok := p.Attrs([]string{name})[name]
		if !ok || v.N == nil {
			return "", false
		}

		return *v.N, true
	}

	h := p.header()
	u := unpacker{s: string(p), i: h.body + 1} // {

	if u.s[u.i] == '}' {
		return "", false
	}

	for {
		// A name written with an escape holds a backslash, which a plain
		// name does not, so the bytes between the quotes compare as the name.
		start := u.i + 1
		u.skipString()
		raw := u.s[start : u.i-1]
		u.i++ // :

		if raw == name {
			return u.number()
		}

		u.skip()

		if u.s[u.i] == '}' {
			return "", false
		}

		u.i++ // ,
	}
}

// plainName reports whether the encoder writes name between its quotes as it
// is: valid UTF-8, and no quote, backslash or control byte to escape.
func plainName(name string) bool {
	for i := range len(name) {
		if c := name[i]; c < ' ' || c == '"' || c == '\\' {
			return false
		}
	}

	return utf8.ValidString(name)
}

// number reads the attribute value at u.i when it is a number, whose text the
// encoder writes without escapes.
func (u *unpacker) number() (string, bool) {
	const prefix = `{"N":"`

	if !strings.HasPrefix(u.s[u.i:], prefix) {
		return "", false
	}

	start := u.i + len(prefix)
	end := start + strings.IndexByte(u.s[start:], '"')

	return u.s[start:end], true
}

// maxDecoderChunk caps the strings a Decoder takes in one allocation, and
// each chunk it takes is chunkGrowth times the last, up to that.
const (
	maxDecoderChunk = 512
	chunkGrowth     = 2
)

// Decoder decodes the items of one read, sharing allocations between them: its
// S and N values point into chunks of strings that it never writes twice, so
// a value it decoded stays valid after the next call. View and Peek each
// reuse one map, and Peek one slice of strings, taken from a pool that
// Release gives them back to. It is not safe for concurrent use.
type Decoder struct {
	strs    []string
	scratch *viewScratch
}

// viewScratch is what View and Peek reuse: a map each, and the strings of
// Peek's values.
type viewScratch struct {
	view, peek reusedMap
	strs       []string
	// parts holds, for each Selection that keeps part of a map, the map
	// ViewSelect reads that part into, item after item.
	parts map[*Selection]*reusedMap
}

// reusedMap is a map that items are decoded into one after another, each
// value written over the one before it, so the map keeps the slot it took.
// seen collects the names the last item set; wild is set once an item gave
// every attribute, whose names are that item's own bytes.
type reusedMap struct {
	m    Item
	seen []string
	wild bool
}

// The largest scratch Release keeps: a map of more attributes, a list of names
// with room for more than twice that, or more Peek strings, is dropped rather
// than held in the pool. The names' room is what a wide item once took and a
// map, which never gives its slots back, still holds, though a narrower item
// since has left it few; it runs past the attributes by what append rounds
// up (71 for 64).
const (
	maxPooledView    = 64
	maxPooledNames   = 2 * maxPooledView
	maxPooledStrings = maxDecoderChunk
)

var viewScratches = sync.Pool{New: func() any { return new(viewScratch) }}

// View decodes the attributes of p that names lists, every one for nil names,
// into a map the next call to View rewrites, and Release gives back: for a
// filter, which reads the item and drops it, or a projection, which copies
// the values it keeps. Its values stay valid after those calls, as Item's
// do. names must not repeat a name. It returns nil for no item.
func (d *Decoder) View(p Packed, names []string) Item {
	if p == "" {
		return nil
	}

	rm := &d.scratchOf().view

	return d.decode(p, names, nil, rm)
}

// Peek is View into a map of its own, for a caller that reads the values and
// keeps none of them, not even through a copy that shares them, such as a
// filter: their strings are rewritten by the next call to Peek.
func (d *Decoder) Peek(p Packed, names []string) Item {
	if p == "" {
		return nil
	}

	s := d.scratchOf()
	clear(s.strs)

	h := p.header()
	u := unpacker{s: string(p), i: h.body, strs: s.strs[:0]}
	m := u.into(&s.peek, viewHint(h, names), names, nil)
	s.strs = u.strs

	return m
}

// Item decodes as View does into a map of its own, which the caller keeps.
func (d *Decoder) Item(p Packed, names []string) Item {
	if p == "" {
		return nil
	}

	return d.decode(p, names, nil, nil)
}

// ViewSelect decodes the parts of p that sel names into the map View decodes
// into, for a caller that copies out the values it keeps. A value sel names
// whole stays valid as View's do; a map it keeps part of is reused by the
// next call, as the item's own map is. It returns nil for no item.
func (d *Decoder) ViewSelect(p Packed, sel *Selection) Item {
	if p == "" {
		return nil
	}

	s := d.scratchOf()
	if s.parts == nil {
		s.parts = make(map[*Selection]*reusedMap, len(sel.below))
	}

	return d.decode(p, sel.names, sel.below, &s.view)
}

// Release gives back the maps and strings View, ViewSelect and Peek reuse. No
// map they returned may be read after it; values copied out of View's and
// ViewSelect's stay valid, as Item's do.
func (d *Decoder) Release() {
	s := d.scratch
	if s == nil {
		return
	}

	d.scratch = nil

	if s.view.oversized() || s.peek.oversized() || cap(s.strs) > maxPooledStrings {
		return
	}

	s.view.release()
	s.peek.release()
	clear(s.strs)
	clear(s.parts)
	viewScratches.Put(s)
}

// oversized is whether rm holds more than a pool keeps: a map of more
// attributes, or room for more names, which an earlier, wider item left behind
// a narrower one.
func (rm *reusedMap) oversized() bool {
	return len(rm.m) > maxPooledView || cap(rm.seen) > maxPooledNames
}

// release empties what the map holds of the items it was read from, which it
// would otherwise keep alive, and keeps its slots for the next read. seen is
// emptied to its capacity, not its length: the names an earlier, wider item
// set lie past where the last, narrower one stopped, and are substrings of
// that item's packed string.
func (rm *reusedMap) release() {
	if rm.wild {
		clear(rm.m)
		rm.wild = false
	}

	for name := range rm.m {
		rm.m[name] = api.AttributeValue{}
	}

	clear(rm.seen[:cap(rm.seen)])
}

func (d *Decoder) scratchOf() *viewScratch {
	if d.scratch == nil {
		d.scratch, _ = viewScratches.Get().(*viewScratch)
	}

	return d.scratch
}

// viewHint is the attributes a decode of names from an item with header h
// holds at most.
func viewHint(h packHeader, names []string) int {
	if names == nil {
		return h.attrs
	}

	return min(h.attrs, len(names))
}

// decode decodes the attributes of p that names lists, through the
// Selections below them, into rm's map, or a new one for nil rm. Its strings
// go into d's chunks.
func (d *Decoder) decode(p Packed, names []string, below []*Selection, rm *reusedMap) Item {
	h := p.header()

	need := h.scalars
	if names != nil {
		need = min(need, len(names))
	}

	// A chunk that cannot hold the item's scalars is left to the values that
	// point into it, and a new one taken: the strings never move.
	if cap(d.strs)-len(d.strs) < need {
		d.strs = make([]string, 0, max(need, min(chunkGrowth*cap(d.strs), maxDecoderChunk)))
	}

	u := unpacker{s: string(p), i: h.body, strs: d.strs, chunked: true}

	var m Item
	if rm != nil {
		if below != nil {
			u.parts = d.scratch.parts
		}

		m = u.into(rm, viewHint(h, names), names, below)
	} else {
		m = u.object(viewHint(h, names), names, below)
	}

	d.strs = u.strs

	return m
}

// into reads the item, as object does, into rm's map: each value written
// over the one the map holds under its name, and then the names this item
// does not hold removed.
func (u *unpacker) into(rm *reusedMap, hint int, names []string, below []*Selection) Item {
	if rm.m == nil {
		rm.m = make(Item, hint)
	}

	// When every attribute is read, the names the item does not hold are
	// found by emptying each value first, since a value read has a member
	// set: looking each name up in seen, as a list of names is, takes as many
	// steps as the square of a wide item's attributes. A map's slots are kept
	// for the next item, as clearing it would give them up and the next
	// item take them again.
	if names == nil {
		for name := range rm.m {
			rm.m[name] = api.AttributeValue{}
		}
	}

	rm.wild = rm.wild || names == nil
	u.reuse, u.seen = rm.m, rm.seen[:0]

	m := u.object(0, names, below)
	rm.seen = u.seen

	switch {
	case len(m) <= len(rm.seen):
	case names == nil:
		dropUnset(m)
	default:
		dropUnseen(m, rm.seen)
	}

	return m
}

// dropUnset removes from m the names whose value is still empty: those an
// earlier item set that the last, which read every attribute, did not.
func dropUnset(m Item) {
	for name, v := range m {
		if v.MembersSet() == 0 {
			delete(m, name)
		}
	}
}

// dropUnseen removes from m the names an earlier item set that the last did
// not: those not in seen.
func dropUnseen(m Item, seen []string) {
	for name := range m {
		if !slices.Contains(seen, name) {
			delete(m, name)
		}
	}
}

// Selection names the parts of an item a read keeps: each attribute it names,
// whole, or, for a map, only the members its own Selection names. The zero
// Selection names nothing.
type Selection struct {
	names []string
	// below is the Selection of each name, nil for the whole value.
	below []*Selection
}

// Add names the value at path, a name and the names of the map members below
// it. A value named whole stays whole, whatever is added below it.
func (s *Selection) Add(path []string) {
	for len(path) > 0 {
		i := slices.Index(s.names, path[0])

		switch {
		case i < 0 && len(path) == 1:
			s.names = append(s.names, path[0])
			s.below = append(s.below, nil)

			return
		case i < 0:
			s.names = append(s.names, path[0])
			s.below = append(s.below, &Selection{})
			i = len(s.names) - 1
		case s.below[i] == nil:
			return
		case len(path) == 1:
			s.below[i] = nil

			return
		default:
		}

		s, path = s.below[i], path[1:]
	}
}

// Names are the attributes s names.
func (s *Selection) Names() []string {
	return s.names
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
	// reuse, when not nil, is the map of an earlier item the top object is
	// read into (into), and seen collects the names written in it.
	reuse Item
	seen  []string
	// open is set when the last object read stopped before its end, once it
	// had every name it wanted.
	open bool
	// chunked is set when strs is a Decoder's chunk, whose strings outlive
	// the read.
	chunked bool
	// parts, when not nil, holds the map each Selection's part of a map is
	// read into (ViewSelect).
	parts map[*Selection]*reusedMap
}

// object reads a map of attribute values, all of them or, when want is not
// nil, those it names; hint sizes the map. below, when not nil, is the
// Selection of each name of want, nil for the whole value.
func (u *unpacker) object(hint int, want []string, below []*Selection) Item {
	m, reused := u.reuse, u.reuse != nil
	if !reused {
		m = make(Item, hint)
	}

	u.reuse = nil
	u.i++ // {

	if u.s[u.i] != '}' {
		u.members(m, want, below, reused)
	} else {
		u.i++
		u.open = false
	}

	return m
}

// members reads the members of an object, from its first, into m.
func (u *unpacker) members(m Item, want []string, below []*Selection, reused bool) {
	found := 0

	for {
		name := u.str()
		u.i++ // :

		j := -1
		if want != nil && found < len(want) {
			j = slices.Index(want, name)
		}

		switch {
		case want != nil && j < 0:
			u.skip()
		case reused:
			// A name from want shares no bytes with the item.
			if j >= 0 {
				name = want[j]
			}

			m[name] = u.value(selectionAt(below, j))
			u.seen = append(u.seen, name)
			found++
		default:
			m[name] = u.value(selectionAt(below, j))
			found++
		}

		c := u.s[u.i]
		u.i++

		if c == '}' {
			u.open = false

			return
		}

		if want != nil && found == len(want) {
			u.open = true

			return
		}
	}
}

// member reads a map value: whole for nil sel, else the members sel names.
func (u *unpacker) member(sel *Selection) Item {
	if sel == nil {
		return u.object(0, nil, nil)
	}

	// The map's own size is not known: a long list of names it may not hold
	// would size it for all of them.
	const maxHint = 8

	var m Item

	if u.parts != nil {
		rm := u.parts[sel]
		if rm == nil {
			rm = &reusedMap{}
			u.parts[sel] = rm
		}

		seen := u.seen
		m = u.into(rm, min(len(sel.names), maxHint), sel.names, sel.below)
		u.seen = seen
	} else {
		m = u.object(min(len(sel.names), maxHint), sel.names, sel.below)
	}

	if u.open {
		u.skipMembers()
		u.open = false
	}

	return m
}

// skipMembers steps over the members of an object from the one at u.i, and
// its closing brace.
func (u *unpacker) skipMembers() {
	for {
		u.skipString()
		u.i++ // :
		u.skip()

		c := u.s[u.i]
		u.i++

		if c == '}' {
			return
		}
	}
}

// selectionAt is below[j], nil when there is none.
func selectionAt(below []*Selection, j int) *Selection {
	if below == nil || j < 0 {
		return nil
	}

	return below[j]
}

// value reads one attribute value; sel, when not nil, names the members of a
// map it keeps.
func (u *unpacker) value(sel *Selection) api.AttributeValue {
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
		v.M = u.member(sel)
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
// read that outgrows it leaves the strings already taken where they are, in a
// chunk of their own when they are kept (chunked) rather than in a copy.
func (u *unpacker) ptr(s string) *string {
	if u.chunked && len(u.strs) == cap(u.strs) {
		const least = 8

		u.strs = make([]string, 0, min(max(chunkGrowth*cap(u.strs), least), maxDecoderChunk))
	}

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
		out = append(out, u.value(nil))
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
