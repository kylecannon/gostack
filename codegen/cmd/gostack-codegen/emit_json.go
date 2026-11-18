package main

import (
	"github.com/kylecannon/gostack/codegen/spec"
)

// jsonFile writes the json codec: the protocol constants, one decode function
// for each operation, and the marshalers of the structs that hold a timestamp
// in a form other than epoch seconds. The types carry json tags, so
// encoding/json/v2 does the rest.
func (g *generator) jsonFile() (string, error) {
	if !g.hasProtocol(protoJSON) {
		return "", nil
	}

	p := &printer{}

	p.f("const (")
	p.f("\t// jsonVersion is the version in the Content-Type of a json response.")
	p.f("\tjsonVersion = %q", g.p.Metadata.JSONVersion)
	p.f("\t// awsQueryCompatible marks a service whose json errors also speak the query protocol.")
	p.f("\tawsQueryCompatible = %t", g.p.Metadata.IsQueryCompatible())
	p.f(")")
	p.blank()

	for _, name := range g.opNames {
		in := g.ops[name].input.goName

		p.f("// decode%sJSON reads the json body of a %s request.", name, name)
		p.f("func decode%sJSON(body []byte) (*%s, *awserr.Error) {", name, in)
		p.f("\treturn awsjson.Decode[%s](body)", in)
		p.f("}")
		p.blank()
	}

	for _, st := range g.sortedStructs() {
		emitTimeMarshalers(p, st)
	}

	return p.buf.String(), nil
}

// timeField is a struct member that json writes in a timestamp format.
type timeField struct {
	goName   string
	jsonName string
	typ      string
}

// timeFields lists the members of st whose timestamps are not epoch seconds.
func timeFields(st *structType) []timeField {
	var out []timeField

	for _, f := range st.fields {
		if f.pair.p.shape.Type != spec.TypeTimestamp || !inJSONBody(st, f) {
			continue
		}

		typ, err := jsonTimeType(f.pair.p.tsfmt)
		if err != nil || typ == "" {
			continue // newField has already rejected what is unsupported
		}

		out = append(out, timeField{goName: f.goName, jsonName: f.jsonName, typ: typ})
	}

	return out
}

// emitTimeMarshalers writes MarshalJSONTo and UnmarshalJSONFrom, the
// encoding/json/v2 interfaces that stream into the caller's encoder and decoder,
// for a struct with timestamp members in another format. Each swaps those
// members for the awsjson type of the format through an embedded alias of the
// struct, which has no methods of its own, and a shadowing field of the same
// json name.
func emitTimeMarshalers(p *printer, st *structType) {
	tf := timeFields(st)
	if len(tf) == 0 {
		return
	}

	shadow := func() {
		p.f("\t\t*plain")

		for _, f := range tf {
			p.f("\t\t%s *%s `json:\"%s,omitzero\"`", f.goName, f.typ, f.jsonName)
		}
	}

	p.f("// MarshalJSONTo writes the timestamps of %s in the format of their shapes.", st.goName)
	p.f("func (s %s) MarshalJSONTo(enc *jsontext.Encoder) error {", st.goName)
	p.f("\ttype plain %s", st.goName)
	p.f("\treturn awsjson.MarshalTo(enc, struct {")
	shadow()
	p.f("\t}{")
	p.f("\t\tplain: (*plain)(&s),")

	for _, f := range tf {
		p.f("\t\t%s: (*%s)(s.%s),", f.goName, f.typ, f.goName)
	}

	p.f("\t})")
	p.f("}")
	p.blank()
	p.f("// UnmarshalJSONFrom reads the timestamps of %s in the format of their shapes.", st.goName)
	p.f("func (s *%s) UnmarshalJSONFrom(dec *jsontext.Decoder) error {", st.goName)
	p.f("\ttype plain %s", st.goName)
	p.f("\taux := struct {")
	shadow()
	p.f("\t}{plain: (*plain)(s)}")
	p.f("\tif err := awsjson.UnmarshalFrom(dec, &aux); err != nil {")
	p.f("\t\treturn err")
	p.f("\t}")

	for _, f := range tf {
		p.f("\tif aux.%s != nil {", f.goName)
		p.f("\t\ts.%s = (*protocol.Time)(aux.%s)", f.goName, f.goName)
		p.f("\t}")
	}

	p.f("\treturn nil")
	p.f("}")
	p.blank()
}
