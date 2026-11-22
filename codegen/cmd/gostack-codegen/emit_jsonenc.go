package main

import (
	"fmt"
	"slices"

	"github.com/kylecannon/gostack/codegen/spec"
)

// The json encoders write each operation output without reflection: an
// AppendJSON method on the output type, which awsjson.WriteResponse calls, and
// an appendJSON<Struct> function for each struct below an output. Their bytes
// are those encoding/json/v2 writes for the same value with awsjson's options:
// members in declaration order, a nil member left out (omitzero), a nil list
// or map inside a list or map written as null, map members in Go's map order.
// Strings go through awsjson's run quoting.
//
// Two shapes stay on v2, through awsjson.Writer.Reflect: a document member,
// and a struct whose timestamps take another format than epoch seconds, which
// its generated MarshalJSONTo writes.
//
// Each struct's function is the one place its members are written, so a member
// with an encoded sibling (markEncoded) is written there from the sibling when
// that is set, its encoded values appended as they are, and from the member
// otherwise. v2 skips the sibling, so the generated test moves each value into
// it and compares with the typed form instead.

// jsonEncodedStructs lists the structs below the operation outputs, sorted by
// name.
func (g *generator) jsonEncodedStructs() []*structType {
	seen := map[string]*structType{}

	var walk func(st *structType)
	walk = func(st *structType) {
		if st == nil || seen[st.goName] != nil {
			return
		}

		seen[st.goName] = st

		for _, f := range st.fields {
			for _, nested := range g.nestedStructs(f.pair.p) {
				walk(g.structs[g.goTypeName(nested)])
			}
		}
	}

	for _, name := range g.opNames {
		walk(g.ops[name].output)
	}

	out := make([]*structType, 0, len(seen))
	for _, name := range sortedKeys(seen) {
		out = append(out, seen[name])
	}

	return out
}

// jsonOutputs lists the distinct output types of the operations, sorted.
func (g *generator) jsonOutputs() []*structType {
	var out []*structType

	for _, name := range g.opNames {
		if st := g.ops[name].output; st != nil && !slices.Contains(out, st) {
			out = append(out, st)
		}
	}

	slices.SortFunc(out, func(a, b *structType) int {
		switch {
		case a.goName < b.goName:
			return -1
		case a.goName > b.goName:
			return 1
		default:
			return 0
		}
	})

	return out
}

// jsonEncodeRounds is how many random values of each output type the
// generated test compares with v2 on every test run; FuzzJSONEncode goes on
// from there.
const jsonEncodeRounds = 100

// jsonTestFile writes the encode tests: every operation's output has a
// generated encoder, and that encoder writes what v2 writes.
func (g *generator) jsonTestFile() (string, error) {
	p := &printer{}

	p.f("// jsonOutputs are the output types of the operations, by operation.")
	p.f("var jsonOutputs = map[string]any{")

	for _, name := range g.opNames {
		if st := g.ops[name].output; st != nil {
			p.f("\t%q: (*%s)(nil),", name, st.goName)
		}
	}

	p.f("}")
	p.blank()
	p.f("func TestJSONEncodersCover(t *testing.T) {")
	p.f("\tt.Parallel()")
	p.f("\tawsjsontest.CheckCoverage(t, jsonOutputs)")
	p.f("}")
	p.blank()
	p.f("func TestJSONEncodeMatchesV2(t *testing.T) {")
	p.f("\tt.Parallel()")
	p.f("\tawsjsontest.CheckRandom(t, jsonOutputs, %d)", jsonEncodeRounds)
	p.f("}")
	p.blank()

	if len(g.opts.EncodedShapes) > 0 {
		p.f("func TestJSONEncodedMatchesTyped(t *testing.T) {")
		p.f("\tt.Parallel()")
		p.f("\tawsjsontest.CheckEncoded(t, jsonOutputs, %d)", jsonEncodeRounds)
		p.f("}")
		p.blank()
	}

	p.f("func FuzzJSONEncode(f *testing.F) {")
	p.f("\tawsjsontest.Fuzz(f, jsonOutputs)")
	p.f("}")

	return p.buf.String(), nil
}

// emitJSONEncoders writes the AppendJSON methods and the struct functions.
func (g *generator) emitJSONEncoders(p *printer) error {
	for _, st := range g.jsonOutputs() {
		p.f("// AppendJSON appends v as json, as encoding/json/v2 writes it.")
		p.f("func (v *%s) AppendJSON(b []byte) ([]byte, error) {", st.goName)
		p.f("\tw := awsjson.NewWriter(b)")
		p.f("\tif v == nil {")
		p.f("\t\tw.Null()")
		p.f("\t} else {")
		p.f("\t\tappendJSON%s(&w, v)", st.goName)
		p.f("\t}")
		p.f("\treturn w.Result()")
		p.f("}")
		p.blank()
	}

	for _, st := range g.jsonEncodedStructs() {
		err := g.emitStructEncoder(p, st)
		if err != nil {
			return fmt.Errorf("json encoder of %s: %w", st.goName, err)
		}
	}

	return nil
}

func (g *generator) emitStructEncoder(p *printer, st *structType) error {
	p.f("// appendJSON%s appends v as a json object.", st.goName)
	p.f("func appendJSON%s(w *awsjson.Writer, v *%s) {", st.goName, st.goName)

	if len(timeFields(st)) > 0 {
		p.f("\t// Its timestamps take a format its MarshalJSONTo writes.")
		p.f("\tw.Reflect(v)")
		p.f("}")
		p.blank()

		return nil
	}

	p.f("\tstart := w.ObjectStart()")

	for _, f := range st.fields {
		if !inJSONBody(st, f) {
			continue
		}

		err := g.emitMemberEncoder(p, f)
		if err != nil {
			return fmt.Errorf("member %s: %w", f.member, err)
		}
	}

	p.f("\tw.ObjectEnd(start)")
	p.f("}")
	p.blank()

	return nil
}

// emitMemberEncoder writes the code that appends one member when it is set,
// from its encoded sibling when the member has one and that is set.
func (g *generator) emitMemberEncoder(p *printer, f *field) error {
	name, err := jsonNameLiteral(f.jsonName)
	if err != nil {
		return err
	}

	r := f.pair.p
	expr := "v." + f.goName

	cond := expr + " != nil"
	if r.shape.Type == spec.TypeTimestamp && !r.shape.Document {
		cond += " && !" + expr + ".IsZero()"
	}

	if e := f.encoded; e != nil {
		encExpr := "v." + e.goName

		encCond := encExpr + " != nil"
		if e.leaf {
			encCond = "len(" + encExpr + ") != 0"
		}

		p.f("\tif %s {", encCond)
		p.f("\t\tw.EncodedMember(`,%s:`)", name)

		err = g.emitJSONValue(p, r, encExpr, siteMember, 0, "\t\t", g.opts.EncodedShapes)
		if err != nil {
			return err
		}

		p.f("\t} else if %s {", cond)
	} else {
		p.f("\tif %s {", cond)
	}

	p.f("\t\tw.Raw(`,%s:`)", name)

	err = g.emitJSONValue(p, r, expr, siteMember, 0, "\t\t", nil)
	if err != nil {
		return err
	}

	p.f("\t}")

	return nil
}

// jsonNameLiteral is name quoted as v2 quotes it, for a member name that needs
// no escape, which every name in the specs is.
func jsonNameLiteral(name string) (string, error) {
	for i := range len(name) {
		if c := name[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '`' {
			return "", fmt.Errorf("json name %q needs an escape", name)
		}
	}

	return `"` + name + `"`, nil
}

// emitJSONValue writes the code that appends the value of r held in expr at
// the given site. depth names the loop variables of nested collections. A
// value of a shape in encoded is an awsjson.Encoded, appended as it is: expr
// is an encoded sibling when encoded is not empty.
func (g *generator) emitJSONValue(
	p *printer, r ref, expr string, at site, depth int, indent string, encoded []string,
) error {
	if slices.Contains(encoded, r.name) {
		p.f("%sw.Encoded(%s)", indent, expr)

		return nil
	}

	if r.shape.Document {
		p.f("%sw.Reflect(%s)", indent, expr)

		return nil
	}

	deref := expr
	if at == siteMember {
		deref = "*" + expr
	}

	switch r.shape.Type {
	case spec.TypeString:
		p.f("%sw.String(%s)", indent, deref)
	case spec.TypeInteger:
		p.f("%sw.Int32(%s)", indent, deref)
	case spec.TypeLong:
		p.f("%sw.Int64(%s)", indent, deref)
	case spec.TypeFloat, spec.TypeDouble:
		p.f("%sw.Float64(%s)", indent, deref)
	case spec.TypeBoolean:
		p.f("%sw.Bool(%s)", indent, deref)
	case spec.TypeTimestamp:
		p.f("%sw.Time(%s)", indent, expr)
	case spec.TypeBlob:
		p.f("%sw.Blob(%s)", indent, expr)
	case spec.TypeStructure:
		if at == siteMember {
			p.f("%sappendJSON%s(w, %s)", indent, g.goTypeName(r.name), expr)
		} else {
			p.f("%sappendJSON%s(w, &%s)", indent, g.goTypeName(r.name), expr)
		}
	case spec.TypeList:
		return g.emitJSONList(p, r, expr, at, depth, indent, encoded)
	case spec.TypeMap:
		return g.emitJSONMap(p, r, expr, at, depth, indent, encoded)
	default:
		return fmt.Errorf("shape %s has unsupported type %q", r.name, r.shape.Type)
	}

	return nil
}

// nilAsNull opens the branch that writes a nil list or map inside a list or
// map as null, and returns the indent of the other branch and its closer. A nil
// member never gets here: its struct leaves it out.
func nilAsNull(p *printer, at site, expr, indent string) (string, func()) {
	if at == siteMember {
		return indent, func() {}
	}

	p.f("%sif %s == nil {", indent, expr)
	p.f("%s\tw.Null()", indent)
	p.f("%s} else {", indent)

	return indent + "\t", func() { p.f("%s}", indent) }
}

func (g *generator) emitJSONList(
	p *printer, r ref, expr string, at site, depth int, indent string, encoded []string,
) error {
	elem, err := resolve(g.p, r.shape.Member)
	if err != nil {
		return err
	}

	i := fmt.Sprintf("i%d", depth)
	in, done := nilAsNull(p, at, expr, indent)

	p.f("%sw.Byte('[')", in)
	p.f("%sfor %s := range %s {", in, i, expr)
	p.f("%s\tif %s > 0 {", in, i)
	p.f("%s\t\tw.Byte(',')", in)
	p.f("%s\t}", in)

	err = g.emitJSONValue(p, elem, fmt.Sprintf("%s[%s]", expr, i), siteElement, depth+1, in+"\t", encoded)
	if err != nil {
		return err
	}

	p.f("%s}", in)
	p.f("%sw.Byte(']')", in)
	done()

	return nil
}

func (g *generator) emitJSONMap(
	p *printer, r ref, expr string, at site, depth int, indent string, encoded []string,
) error {
	value, err := resolve(g.p, r.shape.Value)
	if err != nil {
		return err
	}

	k, e, first := fmt.Sprintf("k%d", depth), fmt.Sprintf("e%d", depth), fmt.Sprintf("first%d", depth)
	in, done := nilAsNull(p, at, expr, indent)

	p.f("%sw.Byte('{')", in)
	p.f("%s%s := true", in, first)
	p.f("%sfor %s, %s := range %s {", in, k, e, expr)
	p.f("%s\tif !%s {", in, first)
	p.f("%s\t\tw.Byte(',')", in)
	p.f("%s\t}", in)
	p.f("%s\t%s = false", in, first)
	p.f("%s\tw.String(%s)", in, k)
	p.f("%s\tw.Byte(':')", in)

	err = g.emitJSONValue(p, value, e, siteElement, depth+1, in+"\t", encoded)
	if err != nil {
		return err
	}

	p.f("%s}", in)
	p.f("%sw.Byte('}')", in)
	done()

	return nil
}
