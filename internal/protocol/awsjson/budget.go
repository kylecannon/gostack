package awsjson

import (
	"bytes"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"reflect"
	"sync"

	"github.com/kylecannon/gostack/internal/awserr"
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
func DecodeWithin[T any](body []byte, budget int64) (v *T, usedLenient bool, err *awserr.Error) {
	p := planOf(reflect.TypeFor[T]())
	if decodePasses*int64(len(body))*p.perByte > budget && overBudget(body, p, budget/decodePasses) {
		return nil, false, ErrTooLarge
	}

	return DecodeLenient[T](body)
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
}

var plans sync.Map // reflect.Type -> *plan

var (
	unmarshalerFrom = reflect.TypeFor[json.UnmarshalerFrom]()
	unmarshaler     = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

func planOf(t reflect.Type) *plan {
	if p, ok := plans.Load(t); ok {
		return p.(*plan) //nolint:forcetypeassert // only plans are stored
	}

	built := map[reflect.Type]*plan{}
	p := buildPlan(t, built)

	for _, q := range built {
		p.perByte = max(p.perByte, q.ownPerByte())
	}

	p.perByte = max(p.perByte, 1)
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

	pt := reflect.PointerTo(t)
	if pt.Implements(unmarshalerFrom) || pt.Implements(unmarshaler) || pt.Implements(textUnmarshaler) {
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
func overBudget(body []byte, p *plan, budget int64) bool {
	w := walker{
		dec:   jsontext.NewDecoder(bytes.NewBuffer(body), jsontext.AllowDuplicateNames(true)),
		limit: budget,
	}

	return errors.Is(w.value(p), errOverBudget)
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
