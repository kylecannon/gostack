package query_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
)

// interp walks a fixture's shape model the way the generated code walks the
// same model, calling the same Decoder and Writer methods. Generated code is
// that walk written out per type; the fixtures give models no generated
// package exists for, so this does it dynamically. Values are maps, slices,
// strings, float64s and bools.
type interp struct {
	shapes map[string]*spec.Shape
}

// resolved is a shape with the traits of the reference that points at it.
type resolved struct {
	name  string
	shape *spec.Shape
	loc   string
	flat  bool
	tsfmt string
}

func (in *interp) resolve(r *spec.Ref) resolved {
	shape := in.shapes[r.Shape]
	if shape == nil {
		panic("unknown shape " + r.Shape)
	}

	out := resolved{
		name: r.Shape, shape: shape, loc: shape.LocationName, flat: shape.Flattened, tsfmt: shape.TimestampFormat,
	}
	if r.LocationName != "" {
		out.loc = r.LocationName
	}

	if r.Flattened {
		out.flat = true
	}

	if r.TimestampFormat != "" {
		out.tsfmt = r.TimestampFormat
	}

	return out
}

func serial(loc, name string) string {
	if loc != "" {
		return loc
	}

	return name
}

func complexType(typ string) bool {
	return typ == spec.TypeStructure || typ == spec.TypeList || typ == spec.TypeMap
}

// decodeStruct reads the members of a structure under prefix.
func (in *interp) decodeStruct(d *query.Decoder, shape *spec.Shape, prefix string) map[string]any {
	out := map[string]any{}

	for _, m := range shape.Members {
		r := in.resolve(&m.Ref)

		if v := in.decodeMember(d, prefix, m.Name, r); v != nil {
			out[m.Name] = v
		}
	}

	return out
}

func (in *interp) decodeMember(d *query.Decoder, prefix, member string, r resolved) any {
	name := serial(r.loc, member)

	switch r.shape.Type {
	case spec.TypeStructure:
		key := query.Key(prefix, name)
		if d.Has(key) {
			return in.decodeStruct(d, r.shape, key)
		}

		return nil
	case spec.TypeList:
		listName := name
		if r.flat {
			listName = serial(in.resolve(r.shape.Member).loc, name)
		}

		return in.decodeList(d, prefix, listName, r)
	case spec.TypeMap:
		return in.decodeMap(d, prefix, name, r)
	default:
		return decodeScalar(d, query.Key(prefix, name), r)
	}
}

func (in *interp) decodeList(d *query.Decoder, prefix, name string, r resolved) any {
	elem := in.resolve(r.shape.Member)
	ls := &query.ListSpec{
		Names: []string{name}, Member: elem.loc, Flattened: r.flat, Complex: complexType(elem.shape.Type),
	}

	keys := d.List(prefix, ls)
	if len(keys) == 0 {
		return nil
	}

	out := make([]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, in.decodeElement(d, key, elem))
	}

	return out
}

func (in *interp) decodeMap(d *query.Decoder, prefix, name string, r resolved) any {
	key, value := in.resolve(r.shape.Key), in.resolve(r.shape.Value)
	ms := &query.MapSpec{
		Names: []string{name}, Key: key.loc, Value: value.loc, Flattened: r.flat,
		ComplexValue: complexType(value.shape.Type),
	}

	out := map[string]any{}

	for _, ent := range d.Map(prefix, ms) {
		k := query.Value(d.String(ent.Key))
		out[k] = in.decodeElement(d, ent.Value, value)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// decodeElement reads a list element or map value whose key is key.
func (in *interp) decodeElement(d *query.Decoder, key string, r resolved) any {
	switch r.shape.Type {
	case spec.TypeStructure:
		return in.decodeStruct(d, r.shape, key)
	case spec.TypeList:
		return in.decodeList(d, key, "", r)
	case spec.TypeMap:
		return in.decodeMap(d, key, "", r)
	default:
		return decodeScalar(d, key, r)
	}
}

func decodeScalar(d *query.Decoder, key string, r resolved) any {
	switch r.shape.Type {
	case spec.TypeString:
		return deref(d.String(key))
	case spec.TypeInteger:
		return deref(d.Int32(key))
	case spec.TypeLong:
		return deref(d.Int64(key))
	case spec.TypeFloat, spec.TypeDouble:
		return deref(d.Float64(key))
	case spec.TypeBoolean:
		return deref(d.Bool(key))
	case spec.TypeBlob:
		if b := d.Blob(key); b != nil {
			return string(b)
		}

		return nil
	case spec.TypeTimestamp:
		if t := d.Time(key, r.tsfmt); t != nil {
			return float64(t.UnixNano()) / float64(time.Second)
		}

		return nil
	default:
		panic("unsupported scalar type " + r.shape.Type)
	}
}

// deref returns what p points to as a plain value, or nil.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}

	return *p
}

// encodeStruct writes the members of a structure that params has.
func (in *interp) encodeStruct(w *query.Writer, shape *spec.Shape, params map[string]any) error {
	for _, m := range shape.Members {
		v, ok := params[m.Name]
		if !ok || v == nil {
			continue
		}

		r := in.resolve(&m.Ref)

		err := in.encodeValue(w, serial(r.loc, m.Name), r, v)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
	}

	return nil
}

func (in *interp) encodeValue(w *query.Writer, name string, r resolved, v any) error {
	switch r.shape.Type {
	case spec.TypeStructure:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("structure value is %T", v)
		}

		w.Open(name)

		err := in.encodeStruct(w, r.shape, m)

		w.Close(name)

		return err
	case spec.TypeList:
		return in.encodeList(w, name, r, v)
	case spec.TypeMap:
		return in.encodeMap(w, name, r, v)
	default:
		return encodeScalar(w, name, r, v)
	}
}

func (in *interp) encodeList(w *query.Writer, name string, r resolved, v any) error {
	items, ok := v.([]any)
	if !ok {
		return fmt.Errorf("list value is %T", v)
	}

	elem := in.resolve(r.shape.Member)
	itemName := serial(elem.loc, "member")

	if r.flat {
		itemName = serial(elem.loc, name)
	} else {
		w.Open(name)
		defer w.Close(name)
	}

	for _, item := range items {
		err := in.encodeValue(w, itemName, elem, item)
		if err != nil {
			return err
		}
	}

	return nil
}

func (in *interp) encodeMap(w *query.Writer, name string, r resolved, v any) error {
	entries, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("map value is %T", v)
	}

	key, value := in.resolve(r.shape.Key), in.resolve(r.shape.Value)
	entry := "entry"

	if r.flat {
		entry = name
	} else {
		w.Open(name)
		defer w.Close(name)
	}

	for _, k := range slices.Sorted(maps.Keys(entries)) {
		w.Open(entry)
		w.String(serial(key.loc, "key"), k)

		err := in.encodeValue(w, serial(value.loc, "value"), value, entries[k])
		if err != nil {
			return err
		}

		w.Close(entry)
	}

	return nil
}

func encodeScalar(w *query.Writer, name string, r resolved, v any) error {
	switch r.shape.Type {
	case spec.TypeString:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("string value is %T", v)
		}

		w.String(name, s)
	case spec.TypeInteger, spec.TypeLong:
		n, err := integer(v)
		if err != nil {
			return err
		}

		w.Int(name, n)
	case spec.TypeFloat, spec.TypeDouble:
		f, err := float(v)
		if err != nil {
			return err
		}

		w.Float(name, f)
	case spec.TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("boolean value is %T", v)
		}

		w.Bool(name, b)
	case spec.TypeBlob:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("blob value is %T", v)
		}

		w.Blob(name, []byte(s))
	case spec.TypeTimestamp:
		num, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("timestamp value is %T", v)
		}

		t := &protocol.Time{}

		err := t.UnmarshalJSON([]byte(num))
		if err != nil {
			return fmt.Errorf("timestamp: %w", err)
		}

		w.Time(name, t, r.tsfmt)
	default:
		return fmt.Errorf("unsupported scalar type %q", r.shape.Type)
	}

	return nil
}

func integer(v any) (int64, error) {
	num, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("integer value is %T", v)
	}

	n, err := num.Int64()
	if err != nil {
		return 0, fmt.Errorf("integer: %w", err)
	}

	return n, nil
}

// float reads a json number, or one of the strings that stand for a value json
// has no number for.
func float(v any) (float64, error) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, fmt.Errorf("float: %w", err)
		}

		return f, nil
	case string:
		return specialFloat(x)
	default:
		return 0, fmt.Errorf("float value is %T", v)
	}
}

func specialFloat(s string) (float64, error) {
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		return math.Inf(1), nil
	case "-Infinity":
		return math.Inf(-1), nil
	default:
		return 0, fmt.Errorf("float value %q", s)
	}
}

// node is an element of a parsed XML document.
type node struct {
	name string
	text string
	kids []*node
}

func (n *node) child(name string) *node {
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}

	return nil
}

func (n *node) children(name string) []*node {
	var out []*node

	for _, k := range n.kids {
		if k.name == name {
			out = append(out, k)
		}
	}

	return out
}

// parseXML reads a document into a tree, ignoring namespaces and attributes.
func parseXML(body []byte) (*node, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))

	var (
		root  *node
		stack []*node
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("read xml: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: t.Name.Local}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.kids = append(parent.kids, n)
			} else {
				root = n
			}

			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		default:
			// comments, directives and the declaration carry nothing a client reads
		}
	}

	if root == nil {
		return nil, errors.New("no root element")
	}

	return root, nil
}

// parseStruct reads a structure the way a client does: a flattened list or map
// is found among the siblings, any other member under its own element.
func (in *interp) parseStruct(shape *spec.Shape, n *node) (map[string]any, error) {
	out := map[string]any{}

	for _, m := range shape.Members {
		r := in.resolve(&m.Ref)
		name := serial(r.loc, m.Name)

		if r.shape.Type == spec.TypeList && r.flat {
			name = serial(in.resolve(r.shape.Member).loc, name)
		}

		matches := n.children(name)
		if len(matches) == 0 {
			continue
		}

		v, err := in.parseValue(r, matches)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}

		out[m.Name] = v
	}

	return out, nil
}

// parseValue reads the elements that hold one member. A flattened list or map
// has one element for each item; anything else has one.
func (in *interp) parseValue(r resolved, matches []*node) (any, error) {
	switch {
	case r.shape.Type == spec.TypeList && r.flat:
		return in.parseItems(in.resolve(r.shape.Member), matches)
	case r.shape.Type == spec.TypeMap && r.flat:
		return in.parseEntries(r, matches)
	default:
		return in.parseElement(r, matches[0])
	}
}

func (in *interp) parseElement(r resolved, n *node) (any, error) {
	switch r.shape.Type {
	case spec.TypeStructure:
		return in.parseStruct(r.shape, n)
	case spec.TypeList:
		elem := in.resolve(r.shape.Member)

		return in.parseItems(elem, n.children(serial(elem.loc, "member")))
	case spec.TypeMap:
		return in.parseEntries(r, n.children("entry"))
	default:
		return parseScalar(r, n.text)
	}
}

func (in *interp) parseItems(elem resolved, items []*node) (any, error) {
	out := make([]any, 0, len(items))

	for _, item := range items {
		v, err := in.parseElement(elem, item)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, nil
}

func (in *interp) parseEntries(r resolved, entries []*node) (any, error) {
	key, value := in.resolve(r.shape.Key), in.resolve(r.shape.Value)
	out := map[string]any{}

	for _, entry := range entries {
		k, v := entry.child(serial(key.loc, "key")), entry.child(serial(value.loc, "value"))
		if k == nil || v == nil {
			return nil, errors.New("map entry lacks a key or a value")
		}

		pv, err := in.parseElement(value, v)
		if err != nil {
			return nil, err
		}

		out[k.text] = pv
	}

	return out, nil
}

func parseScalar(r resolved, text string) (any, error) {
	switch r.shape.Type {
	case spec.TypeString:
		return text, nil
	case spec.TypeInteger, spec.TypeLong:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer: %w", err)
		}

		return float64(n), nil
	case spec.TypeFloat, spec.TypeDouble:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("float: %w", err)
		}

		return f, nil
	case spec.TypeBoolean:
		return strings.EqualFold(text, "true"), nil
	case spec.TypeBlob:
		b, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("blob: %w", err)
		}

		return string(b), nil
	case spec.TypeTimestamp:
		t, err := query.ParseTime(text, r.tsfmt)
		if err != nil {
			return nil, fmt.Errorf("timestamp: %w", err)
		}

		return float64(t.UnixNano()) / float64(time.Second), nil
	default:
		return nil, fmt.Errorf("unsupported scalar type %q", r.shape.Type)
	}
}

// emptiness says what canonical does with an empty list, map or structure.
type emptiness int

const (
	// keepEmpty leaves them in, as a response can say "empty".
	keepEmpty emptiness = iota + 1
	// dropEmpty removes them, since a query request has no way to say "empty".
	dropEmpty
)

// canonical turns a dynamic value into one that compares with ==: numbers are
// float64s rounded to the microsecond, and the floats json cannot hold are the
// strings the fixtures use.
func canonical(v any, empties emptiness) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}

		for k, e := range x {
			if n := canonical(e, empties); n != nil {
				out[k] = n
			}
		}

		if len(out) == 0 && empties == dropEmpty {
			return nil
		}

		return out
	case []any:
		out := make([]any, 0, len(x))

		for _, e := range x {
			out = append(out, canonical(e, empties))
		}

		if len(out) == 0 && empties == dropEmpty {
			return nil
		}

		return out
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}

		return canonical(f, empties)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		default:
			return math.Round(x*1e6) / 1e6
		}
	default:
		return v
	}
}
