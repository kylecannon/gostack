package awsjson

import (
	"bytes"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"reflect"
	"sync"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

// ErrTooLarge is what DecodeWithin returns for a body that would decode into
// more than its budget. A caller answers it as it answers a body over
// protocol.MaxBody; the value is only compared, never written.
var ErrTooLarge = &awserr.Error{
	Code:       "SerializationException",
	Message:    "The request body decodes into more than gostack allows.",
	StatusCode: http.StatusBadRequest,
	Fault:      awserr.FaultSender,
}

// DecodeWithin is DecodeLenient for a body that may hold at most budget bytes
// once decoded (protocol.DecodeBudget). A body whose bytes could decode into
// more is first walked token by token without decoding, charging what each
// value would cost; past the budget it returns ErrTooLarge before anything is
// decoded. A body too small to reach the budget whatever its shape is not
// walked, so the walk costs nothing for ordinary requests.
//
// Decoding can run twice (DecodeLenient's strict pass, then the lenient one),
// so one pass is held to half the budget.
//
// When hold is not nil, what both passes of a body that could decode into more
// than protocol.FreeDecode would hold is charged to hold before decoding. When
// hold refuses, DecodeWithin returns protocol.ThrottledJSON.
func DecodeWithin[T any](body []byte, budget int64, hold protocol.DecodeHolder) (*T, bool, *awserr.Error) {
	p := planOf(reflect.TypeFor[T]())

	bound := decodePasses * int64(len(body)) * p.perByte
	if bound > budget || (hold != nil && bound > protocol.FreeDecode) {
		err := chargeDecode(body, p, budget, hold)
		if err != nil {
			return nil, false, err
		}
	}

	return DecodeLenient[T](body)
}

// chargeDecode refuses body with ErrTooLarge when decoding it as p plans it
// would pass budget, and otherwise charges what both passes hold past
// protocol.FreeDecode to hold, when there is one.
func chargeDecode(body []byte, p *plan, budget int64, hold protocol.DecodeHolder) *awserr.Error {
	unwalked := int64(math.MaxInt64)
	if hold != nil {
		unwalked = protocol.FreeDecode
	}

	cost, err := decodeCost(body, p, budget, unwalked)
	if err != nil || hold == nil || cost <= protocol.FreeDecode {
		return err
	}

	_, herr := hold.HoldDecode(cost)

	switch {
	case errors.Is(herr, protocol.ErrOverMemory):
		return ErrTooLarge
	case herr != nil:
		return protocol.ThrottledJSON()
	}

	return nil
}

// decodeCost is what both passes of decoding body as p plans it hold at most,
// or ErrTooLarge when that passes budget.
//
// It first bounds the cost from the body's shape without reading it as json
// (structureUnits): its text costs at most its bytes, and only the separators
// amplify. A body that bound already admits is not walked: one under the
// budget whose separators cost at most unwalked: any, when nothing is charged,
// and protocol.FreeDecode when the bound is charged, so the charge is at
// most FreeDecode beyond the body's own bytes. Any other body is walked token by
// token (walkCost), which is as costly as decoding it.
func decodeCost(body []byte, p *plan, budget, unwalked int64) (int64, *awserr.Error) {
	structure := decodePasses * structureUnits(body) * p.perUnit
	bound := decodePasses*int64(len(body)) + structure

	if bound <= budget && structure <= unwalked {
		return bound, nil
	}

	cost, over := walkCost(body, p, budget/decodePasses)
	if over {
		return 0, ErrTooLarge
	}

	return decodePasses * cost, nil
}

// structureUnits is how many places in body a value can begin at: after a
// comma (the next element, or the next member's name), after a colon (a
// member's value), after an opening bracket (an array's first element), and the
// body's own value. Separators inside strings are counted too, which only
// raises the bound.
func structureUnits(body []byte) int64 {
	return int64(bytes.Count(body, []byte{','}) + bytes.Count(body, []byte{':'}) + bytes.Count(body, []byte{'['}) + 1)
}

// What one pass of decoding allocates, beyond the space its container gives a
// value:
//   - an element of a slice: appendCost times its size, for the slice and the
//     copies the decoder leaves behind as it grows it by a quarter at a time;
//   - a map entry: twice the key and value, a bucket's overhead (mapEntryCost),
//     and the key's text;
//   - a pointer: the value it points to;
//   - a string or blob: its text;
//   - a value of type any: anyValueCost, plus the text of a string.
//
// A member the type does not have costs nothing: the decoder skips it.
const (
	appendCost   = 5
	mapEntryCost = 16
	anyValueCost = 64
	decodePasses = 2
)

type planKind uint8

const (
	kindNone planKind = iota // nothing beyond its container: a number, a bool, a time
	kindText                 // a string or a blob: its text
	kindPointer
	kindStruct
	kindSlice
	kindMap
	kindAny
)

// plan is what the budget walk knows of a type.
type plan struct {
	kind   planKind
	size   int64            // the pointed value, the element, or the key and value together
	elem   *plan            // the pointed value, the element, or the map value
	fields map[string]*plan // a struct's members by json name
	// perByte is the most a byte of body can cost anywhere in the type: a
	// body of n bytes cannot cost more than n times it.
	perByte int64
	// perUnit is the most the walk charges at one place a value can begin
	// (structureUnits), beyond the text it reads there: the element or the map
	// entry, what it points to, and for a value of type any its closing token
	// and its member's name as well.
	perUnit int64
}

var plans sync.Map // reflect.Type -> *plan

var (
	unmarshalerFrom = reflect.TypeFor[json.UnmarshalerFrom]()
	unmarshaler     = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// freeLeaves are the only types with an unmarshaler of their own that cost
// nothing beyond their container: the timestamps, which read one string or
// number into a time. Any other type with an unmarshaler is planned through its
// fields when it is a structure (a generated one decodes its own members, with
// its timestamps in the format of their shapes), and otherwise costs what a
// value of type any does, whatever it reads.
var freeLeaves = map[reflect.Type]bool{
	reflect.TypeFor[protocol.Time](): true,
	reflect.TypeFor[ISO8601]():       true,
	reflect.TypeFor[RFC822]():        true,
}

// hasUnmarshaler reports whether a value of type t decodes itself.
func hasUnmarshaler(t reflect.Type) bool {
	pt := reflect.PointerTo(t)

	return pt.Implements(unmarshalerFrom) || pt.Implements(unmarshaler) || pt.Implements(textUnmarshaler)
}

func planOf(t reflect.Type) *plan {
	if p, ok := plans.Load(t); ok {
		return p.(*plan) //nolint:forcetypeassert // only plans are stored
	}

	built := map[reflect.Type]*plan{}
	p := buildPlan(t, built)

	for _, q := range built {
		p.perByte = max(p.perByte, q.ownPerByte())
		p.perUnit = max(p.perUnit, q.unitCost())
	}

	p.perByte = max(p.perByte, 1)
	p.perUnit = max(p.perUnit, p.valueCost())
	plans.Store(t, p)

	return p
}

// ownPerByte is the most one byte of a value of this kind can cost, not
// counting what it holds: an element takes at least two bytes ("0,"), a map
// entry five (`"":0,`), a member holding a pointer four (`"a":{}`) and a value
// of type any two.
func (p *plan) ownPerByte() int64 {
	const (
		elementBytes = 2
		entryBytes   = 5
		memberBytes  = 4
	)

	switch p.kind {
	case kindSlice:
		return ceilDiv(appendCost*p.size, elementBytes)
	case kindMap:
		return ceilDiv(2*p.size+mapEntryCost, entryBytes)
	case kindPointer:
		return ceilDiv(p.size, memberBytes)
	case kindAny:
		return ceilDiv(anyValueCost, elementBytes)
	default:
		return 1
	}
}

// unitCost is the most the walk charges at one place a value of this kind
// holds another, beyond its text: an element after its opening bracket or a
// comma, a map entry or a member after its colon. Inside a value of type any,
// a colon carries the member's name, its value and the value's closing token.
func (p *plan) unitCost() int64 {
	switch p.kind {
	case kindSlice:
		return appendCost*p.size + p.elem.valueCost()
	case kindMap:
		return 2*p.size + mapEntryCost + p.elem.valueCost()
	case kindStruct:
		var most int64
		for _, f := range p.fields {
			most = max(most, f.valueCost())
		}

		return most
	case kindAny:
		return anyUnitCost
	default:
		return 0
	}
}

// valueCost is what the walk charges where a value of this kind begins: what
// a chain of pointers points to, and for a value of type any its own token and
// its closing one. Containers charge at the places their members begin.
func (p *plan) valueCost() int64 {
	var cost int64

	for range maxPointerChain {
		switch p.kind {
		case kindPointer:
			cost += p.size
			p = p.elem
		case kindAny:
			return cost + anyUnitCost
		default:
			return cost
		}
	}

	return unboundedUnit
}

const (
	// anyUnitCost is three tokens of a value of type any: a member's name, its
	// value, and the value's closing token.
	anyUnitCost = 3 * anyValueCost
	// maxPointerChain is longer than any chain of pointers in the API; a
	// longer one makes every body walked (unboundedUnit).
	maxPointerChain = 8
	// unboundedUnit stands in for a longer chain: large enough that a body of
	// such a type is walked.
	unboundedUnit = 1 << 24
)

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

// sizeOf is t's size in bytes.
func sizeOf(t reflect.Type) int64 {
	return int64(t.Size()) //nolint:gosec // a type's size fits an int64
}

func buildPlan(t reflect.Type, built map[reflect.Type]*plan) *plan {
	if p, ok := built[t]; ok {
		return p
	}

	p := &plan{}
	built[t] = p

	if freeLeaves[t] {
		return p
	}

	if t.Kind() != reflect.Struct && hasUnmarshaler(t) {
		p.kind = kindAny

		return p
	}

	switch t.Kind() {
	case reflect.String:
		p.kind = kindText
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			p.kind = kindText

			break
		}

		p.kind, p.size, p.elem = kindSlice, sizeOf(t.Elem()), buildPlan(t.Elem(), built)
	case reflect.Pointer:
		p.kind, p.size, p.elem = kindPointer, sizeOf(t.Elem()), buildPlan(t.Elem(), built)
	case reflect.Map:
		p.kind, p.size, p.elem = kindMap, sizeOf(t.Key())+sizeOf(t.Elem()), buildPlan(t.Elem(), built)
	case reflect.Interface:
		p.kind = kindAny
	case reflect.Struct:
		p.kind, p.fields = kindStruct, structFields(t, built)
	default:
	}

	return p
}

func structFields(t reflect.Type, built map[reflect.Type]*plan) map[string]*plan {
	fields := make(map[string]*plan, t.NumField())

	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}

		tag := f.Tag.Get("json")

		name, _, hasOptions := bytes.Cut([]byte(tag), []byte(","))
		if string(name) == "-" && !hasOptions {
			continue
		}

		key := string(name)
		if key == "" {
			key = f.Name
		}

		fields[key] = buildPlan(f.Type, built)
	}

	return fields
}

// errOverBudget stops a walk that has passed its budget.
var errOverBudget = errors.New("over budget")

// overBudget reports whether body, decoded into a value planned by p, would
// cost more than budget. A body that is not valid json reports false: the
// decode that follows refuses it, having decoded no more than the walk read.
// walkCost walks body as p plans it and returns what one pass of decoding it
// holds, or reports over once that passes budget.
func walkCost(body []byte, p *plan, budget int64) (int64, bool) {
	w := walker{
		dec:   jsontext.NewDecoder(bytes.NewBuffer(body), jsontext.AllowDuplicateNames(true)),
		limit: budget,
	}

	if errors.Is(w.value(p), errOverBudget) {
		return 0, true
	}

	return w.cost, false
}

type walker struct {
	dec         *jsontext.Decoder
	cost, limit int64
	name        []byte
}

func (w *walker) charge(n int64) error {
	w.cost += n
	if w.cost > w.limit {
		return errOverBudget
	}

	return nil
}

// value walks the next value as p plans it. A value of another kind than p
// expects is skipped: decoding refuses it or reads it with the lenient
// readers, which hold no more than a scalar.
func (w *walker) value(p *plan) error {
	kind := w.dec.PeekKind()

	switch {
	case p.kind == kindAny:
		return w.anyValue()
	case p.kind == kindText && kind == '"':
		v, err := w.dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // only errOverBudget is looked at
		}

		return w.charge(int64(len(v)))
	case p.kind == kindPointer && kind != 'n':
		err := w.charge(p.size)
		if err != nil {
			return err
		}

		return w.value(p.elem)
	case p.kind == kindStruct && kind == '{':
		return w.object(func(name []byte) (int64, *plan) { return 0, p.fields[string(name)] })
	case p.kind == kindMap && kind == '{':
		return w.object(func(name []byte) (int64, *plan) {
			return 2*p.size + mapEntryCost + int64(len(name)), p.elem
		})
	case p.kind == kindSlice && kind == '[':
		return w.array(p)
	default:
		return w.dec.SkipValue() //nolint:wrapcheck // only errOverBudget is looked at
	}
}

// object walks an object, asking member for what each name costs and how its
// value is planned. A nil plan skips the value.
func (w *walker) object(member func(name []byte) (int64, *plan)) error {
	_, err := w.dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // only errOverBudget is looked at
	}

	for w.dec.PeekKind() == '"' {
		var raw jsontext.Value

		raw, err = w.dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // only errOverBudget is looked at
		}

		name := raw[1 : len(raw)-1]
		if bytes.IndexByte(name, '\\') >= 0 {
			w.name, err = jsontext.AppendUnquote(w.name[:0], raw)
			if err != nil {
				return err //nolint:wrapcheck // only errOverBudget is looked at
			}

			name = w.name
		}

		cost, elem := member(name)

		err = w.charge(cost)
		if err != nil {
			return err
		}

		if elem == nil {
			err = w.dec.SkipValue()
		} else {
			err = w.value(elem)
		}

		if err != nil {
			return err
		}
	}

	_, err = w.dec.ReadToken()

	return err //nolint:wrapcheck // only errOverBudget is looked at
}

func (w *walker) array(p *plan) error {
	_, err := w.dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // only errOverBudget is looked at
	}

	for kind := w.dec.PeekKind(); kind != ']' && kind != 0; kind = w.dec.PeekKind() {
		err = w.charge(appendCost * p.size)
		if err != nil {
			return err
		}

		err = w.value(p.elem)
		if err != nil {
			return err
		}
	}

	_, err = w.dec.ReadToken()

	return err //nolint:wrapcheck // only errOverBudget is looked at
}

// anyValue walks a value decoded into an any: every value, member name
// included, costs anyValueCost and a string its text as well. It loops rather
// than recursing, so the depth of the value costs no stack.
func (w *walker) anyValue() error {
	depth := w.dec.StackDepth()

	for {
		var (
			text jsontext.Value
			err  error
		)

		if w.dec.PeekKind() == '"' {
			text, err = w.dec.ReadValue()
		} else {
			_, err = w.dec.ReadToken()
		}

		if err != nil {
			return err //nolint:wrapcheck // only errOverBudget is looked at
		}

		err = w.charge(anyValueCost + int64(len(text)))
		if err != nil {
			return err
		}

		if w.dec.StackDepth() == depth {
			return nil
		}
	}
}
