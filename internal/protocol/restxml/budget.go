package restxml

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
)

// TokenCost is what reading one token charges: about what encoding/xml
// allocates for it (the element's name, its attributes), held until the
// collector runs. An empty element is two tokens. Character data charges twice
// its length beyond it, for the buffer encoding/xml grows to hold it.
const TokenCost = 48

const (
	// mapEntryCost is a map bucket's share of one entry, beyond its key and
	// value.
	mapEntryCost = 16
	// appendCost is what append allocates in all for one element of a long
	// slice, in sizes of the element: it grows the slice by a quarter at a
	// time, and the copies it leaves behind sum to four times the final slice.
	appendCost = 5
)

// attrCost is what one attribute of a start element holds beyond its text: the
// xml.Attr, and its share of the slice that holds them.
const attrCost = 2 * unsafe.Sizeof(xml.Attr{})

func (d *Decoder) chargeToken(tok xml.Token) {
	switch t := tok.(type) {
	case xml.CharData:
		d.Charge(TokenCost + 2*uintptr(len(t)))
	case xml.StartElement:
		n := TokenCost + uintptr(len(t.Name.Space)+len(t.Name.Local))
		for _, a := range t.Attr {
			n += attrCost + uintptr(len(a.Name.Space)+len(a.Name.Local)+len(a.Value))
		}

		d.Charge(n)
	case xml.EndElement:
		d.Charge(TokenCost + uintptr(len(t.Name.Space)+len(t.Name.Local)))
	case xml.Comment:
		d.Charge(TokenCost + 2*uintptr(len(t)))
	case xml.Directive:
		d.Charge(TokenCost + 2*uintptr(len(t)))
	case xml.ProcInst:
		d.Charge(TokenCost + uintptr(len(t.Target)) + 2*uintptr(len(t.Inst)))
	default:
		d.Charge(TokenCost)
	}
}

// errOverBudget is the cause of the error a Decoder stops with when what it
// decoded passes its budget. It wraps rest.ErrBodyTooLarge, so a service
// answers it as it answers a body over protocol.MaxBody.
var errOverBudget = fmt.Errorf("decoded values pass the request's budget: %w", rest.ErrBodyTooLarge)

// ErrOverBudget reports whether err is a Decoder's error for a body that
// decoded into more than its budget.
func ErrOverBudget(err error) bool {
	return errors.Is(err, errOverBudget)
}

// Charge adds n bytes to what decoding holds. Past
// protocol.DecodeBudgetAt of the bytes read so far, decoding stops with an
// error that wraps rest.ErrBodyTooLarge: the budget grows with the body as it
// is read, so a run of elements that each decode into many times their bytes
// is refused soon after DecodeSlack, wherever it sits in the body.
func (d *Decoder) Charge(n uintptr) {
	d.cost += int64(n) //nolint:gosec // a charge is a type's size or a token's, far below 2^63
	if d.cost > d.covered {
		d.cover()
	}

	if d.cost > protocol.DecodeSlack && d.cost > protocol.DecodeBudgetAt(int(d.xd.InputOffset()), d.factor) {
		d.fail(errOverBudget)
	}
}

// Append appends v to s and charges appendCost times v's size: what the slice
// holds, and the copies append leaves behind as it grows. Generated decoders
// add every list element with it.
func Append[T any](d *Decoder, s []T, v T) []T {
	d.Charge(appendCost * unsafe.Sizeof(v))

	return append(s, v)
}

// Put sets m[k] to v and charges twice the key and value, a bucket's share,
// and the key's text. Generated decoders add every map entry with it.
func Put[V any](d *Decoder, m map[string]V, k string, v V) {
	d.Charge(2*(unsafe.Sizeof(k)+unsafe.Sizeof(v)) + mapEntryCost + uintptr(len(k)))
	m[k] = v
}

// New returns a new T and charges its size. Generated decoders allocate every
// structure member with it.
func New[T any](d *Decoder) *T {
	v := new(T)
	d.Charge(unsafe.Sizeof(*v))

	return v
}

// cover charges what decoding holds to the call's hold, or stops decoding with
// its error.
func (d *Decoder) cover() {
	covered, err := d.hold.HoldDecode(d.cost)
	if err != nil {
		d.covered = math.MaxInt64
		d.fail(err)

		return
	}

	d.covered = covered
}
