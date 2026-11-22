package main

import (
	"fmt"
	"slices"

	"github.com/kylecannon/gostack/codegen/spec"
)

// encodedField is the sibling of a member that holds its value already encoded
// as json: awsjson.Encoded in place of each value of a shape the service's
// EncodedShapes names, inside the same lists and maps as the member's own type.
type encodedField struct {
	goName string
	typ    string
	// leaf marks a member whose own shape is the encoded one, so the sibling
	// is a single awsjson.Encoded rather than a list or map of them.
	leaf bool
}

// encodedSuffix ends the Go name of a member's encoded sibling.
const encodedSuffix = "JSON"

// encodedGoType is the Go type of one pre-encoded value.
const encodedGoType = "awsjson.Encoded"

// markEncoded gives an encoded sibling to each member of a struct below an
// operation output whose shape is one of the service's EncodedShapes, or a
// list or map that holds one. Only the generated json encoder writes it;
// v2 and every decoder skip it.
func (g *generator) markEncoded() error {
	if len(g.opts.EncodedShapes) == 0 {
		return nil
	}

	// Only the json encoder writes a sibling: a query response written from
	// the same output would leave the member out.
	if !slices.Equal(g.protocols, []string{protoJSON}) {
		return fmt.Errorf("the EncodedShapes option needs json as the only protocol, not %v", g.protocols)
	}

	reached := map[string]bool{}
	structs := g.jsonEncodedStructs()

	for _, st := range structs {
		err := g.markEncodedStruct(st, reached)
		if err != nil {
			return fmt.Errorf("%s: %w", st.goName, err)
		}
	}

	// v2 writes a struct with timestamps whole, the structs below it too, and
	// never reads a sibling.
	for _, st := range structs {
		if len(timeFields(st)) == 0 {
			continue
		}

		switch holder := g.encodedHolder(st, map[string]bool{}); holder {
		case "":
		case st.goName:
			return fmt.Errorf("%s: v2 writes the struct for its timestamps, and it has an encoded member", st.goName)
		default:
			return fmt.Errorf("%s: v2 writes the struct for its timestamps, and %s below it has an encoded member",
				st.goName, holder)
		}
	}

	for _, name := range g.opts.EncodedShapes {
		if g.p.Shapes[name] == nil {
			return fmt.Errorf("encoded shape %s is not a shape of the model", name)
		}

		if !reached[name] {
			return fmt.Errorf("encoded shape %s is no member of an operation output", name)
		}
	}

	return nil
}

func (g *generator) markEncodedStruct(st *structType, reached map[string]bool) error {
	for _, f := range st.fields {
		if !inJSONBody(st, f) {
			continue
		}

		typ, shape, err := g.encodedType(f.pair.p)
		if err != nil {
			return fmt.Errorf("member %s: %w", f.member, err)
		}

		if shape == "" {
			continue
		}

		reached[shape] = true

		name := f.goName + encodedSuffix
		if name == unionMethod || slices.ContainsFunc(st.fields, func(o *field) bool { return o.goName == name }) {
			return fmt.Errorf("the encoded sibling %s of member %s is taken", name, f.member)
		}

		f.encoded = &encodedField{goName: name, typ: typ, leaf: typ == encodedGoType}
	}

	return nil
}

// encodedHolder returns the name of st, or of a struct below it, that has a
// member with an encoded sibling, or "" when none has. seen holds the structs
// already looked at.
func (g *generator) encodedHolder(st *structType, seen map[string]bool) string {
	if st == nil || seen[st.goName] {
		return ""
	}

	seen[st.goName] = true

	for _, f := range st.fields {
		if f.encoded != nil {
			return st.goName
		}
	}

	for _, f := range st.fields {
		for _, nested := range g.nestedStructs(f.pair.p) {
			if holder := g.encodedHolder(g.structs[g.goTypeName(nested)], seen); holder != "" {
				return holder
			}
		}
	}

	return ""
}

// encodedType returns the Go type of the encoded sibling of a value of r, and
// the encoded shape it holds; an empty shape means r holds none. It follows
// lists and maps, not structures: a structure below has siblings of its own.
func (g *generator) encodedType(r ref) (string, string, error) {
	if slices.Contains(g.opts.EncodedShapes, r.name) {
		return encodedGoType, r.name, nil
	}

	var (
		elem   *spec.Ref
		prefix string
	)

	switch {
	case r.shape.Document:
		return "", "", nil
	case r.shape.Type == spec.TypeList:
		elem, prefix = r.shape.Member, "[]"
	case r.shape.Type == spec.TypeMap:
		elem, prefix = r.shape.Value, "map[string]"
	default:
		return "", "", nil
	}

	er, err := resolve(g.p, elem)
	if err != nil {
		return "", "", err
	}

	typ, shape, err := g.encodedType(er)
	if shape == "" || err != nil {
		return "", "", err
	}

	return prefix + typ, shape, nil
}
