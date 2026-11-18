package awsjson_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// interp walks a fixture's shape model the way the generated types and encoding/json/v2
// do: a member is under its locationName or its name, a timestamp is epoch
// seconds unless the shape says otherwise, and a blob is base64. It converts
// between what is on the wire and plain values (maps, slices, strings,
// float64s and bools), using the same protocol.Time and awsjson types the
// generated code does.
type interp struct {
	shapes map[string]*spec.Shape
}

type resolved struct {
	name  string
	shape *spec.Shape
	loc   string
	tsfmt string
}

func (in *interp) resolve(r *spec.Ref) resolved {
	shape := in.shapes[r.Shape]
	if shape == nil {
		panic("unknown shape " + r.Shape)
	}

	out := resolved{name: r.Shape, shape: shape, loc: shape.LocationName, tsfmt: shape.TimestampFormat}
	if r.LocationName != "" {
		out.loc = r.LocationName
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

// fromWire turns a decoded json value into the plain value the shape stands
// for.
func (in *interp) fromWire(r resolved, v any) (any, error) {
	switch r.shape.Type {
	case spec.TypeStructure:
		return in.structFromWire(r, v)
	case spec.TypeList:
		return in.listFromWire(r, v)
	case spec.TypeMap:
		return in.mapFromWire(r, v)
	case spec.TypeBlob:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("blob is %T", v)
		}

		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("blob: %w", err)
		}

		return string(b), nil
	case spec.TypeTimestamp:
		return timeFromWire(r.tsfmt, v)
	case spec.TypeFloat, spec.TypeDouble:
		if s, ok := v.(string); ok {
			return specialFloat(s)
		}

		return v, nil
	default:
		return v, nil
	}
}

func (in *interp) structFromWire(r resolved, v any) (any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("structure is %T", v)
	}

	out := map[string]any{}

	for _, m := range r.shape.Members {
		raw, present := obj[serial(in.resolve(&m.Ref).loc, m.Name)]
		if !present || raw == nil {
			continue
		}

		pv, err := in.fromWire(in.resolve(&m.Ref), raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}

		out[m.Name] = pv
	}

	return out, nil
}

func (in *interp) listFromWire(r resolved, v any) (any, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("list is %T", v)
	}

	elem := in.resolve(r.shape.Member)
	out := make([]any, 0, len(items))

	for _, item := range items {
		pv, err := in.fromWire(elem, item)
		if err != nil {
			return nil, err
		}

		out = append(out, pv)
	}

	return out, nil
}

func (in *interp) mapFromWire(r resolved, v any) (any, error) {
	entries, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("map is %T", v)
	}

	elem := in.resolve(r.shape.Value)
	out := make(map[string]any, len(entries))

	for k, item := range entries {
		pv, err := in.fromWire(elem, item)
		if err != nil {
			return nil, err
		}

		out[k] = pv
	}

	return out, nil
}

// timeFromWire reads a timestamp in the form the shape's format gives and
// returns it as epoch seconds.
func timeFromWire(format string, v any) (any, error) {
	raw, err := awsjson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}

	var t protocol.Time

	switch strings.ToLower(format) {
	case "iso8601":
		err = awsjson.Unmarshal(raw, (*awsjson.ISO8601)(&t))
	case "rfc822":
		err = awsjson.Unmarshal(raw, (*awsjson.RFC822)(&t))
	default:
		err = awsjson.Unmarshal(raw, &t)
	}

	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}

	return float64(t.UnixNano()) / float64(time.Second), nil
}

// toWire turns a plain value into the value that is encoded to json for the
// shape.
func (in *interp) toWire(r resolved, v any) (any, error) {
	switch r.shape.Type {
	case spec.TypeStructure:
		return in.structToWire(r, v)
	case spec.TypeList:
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("list is %T", v)
		}

		return in.itemsToWire(in.resolve(r.shape.Member), items)
	case spec.TypeMap:
		return in.entriesToWire(in.resolve(r.shape.Value), v)
	case spec.TypeBlob:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("blob is %T", v)
		}

		return base64.StdEncoding.EncodeToString([]byte(s)), nil
	case spec.TypeTimestamp:
		return timeToWire(r.tsfmt, v)
	default:
		return v, nil
	}
}

func (in *interp) structToWire(r resolved, v any) (any, error) {
	params, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("structure is %T", v)
	}

	out := map[string]any{}

	for _, m := range r.shape.Members {
		raw, present := params[m.Name]
		if !present || raw == nil {
			continue
		}

		mr := in.resolve(&m.Ref)

		wv, err := in.toWire(mr, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}

		out[serial(mr.loc, m.Name)] = wv
	}

	return out, nil
}

func (in *interp) itemsToWire(elem resolved, items []any) (any, error) {
	out := make([]any, 0, len(items))

	for _, item := range items {
		wv, err := in.toWire(elem, item)
		if err != nil {
			return nil, err
		}

		out = append(out, wv)
	}

	return out, nil
}

func (in *interp) entriesToWire(elem resolved, v any) (any, error) {
	entries, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("map is %T", v)
	}

	out := make(map[string]any, len(entries))

	for k, item := range entries {
		wv, err := in.toWire(elem, item)
		if err != nil {
			return nil, err
		}

		out[k] = wv
	}

	return out, nil
}

// timeToWire writes epoch seconds in the form the shape's format gives.
func timeToWire(format string, v any) (any, error) {
	num, ok := v.(json.Number)
	if !ok {
		return nil, fmt.Errorf("timestamp is %T", v)
	}

	var t protocol.Time

	err := t.UnmarshalJSON([]byte(num))
	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}

	var raw []byte

	switch strings.ToLower(format) {
	case "iso8601":
		raw, err = awsjson.Marshal((*awsjson.ISO8601)(&t))
	case "rfc822":
		raw, err = awsjson.Marshal((*awsjson.RFC822)(&t))
	default:
		raw, err = awsjson.Marshal(&t)
	}

	if err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}

	return json.RawMessage(raw), nil
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
		return 0, errors.New("float is the string " + s)
	}
}

// canonical turns a plain value into one that compares with ==: numbers are
// float64s rounded to the microsecond, and the floats json cannot hold are the
// strings the fixtures use.
func canonical(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))

		for k, e := range x {
			out[k] = canonical(e)
		}

		return out
	case []any:
		out := make([]any, 0, len(x))

		for _, e := range x {
			out = append(out, canonical(e))
		}

		return out
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}

		return canonical(f)
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
