package main

import (
	"fmt"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// typesFile writes the struct of every operation input and output and of the
// structures below them.
func (g *generator) typesFile() (string, error) {
	p := &printer{}

	for _, st := range g.sortedStructs() {
		if len(st.fields) == 0 {
			p.f("// %s has no members.", st.goName)
			p.f("type %s struct{}", st.goName)
			p.blank()

			continue
		}

		p.f("// %s is a structure of the %s API.", st.goName, g.name)
		p.f("type %s struct {", st.goName)

		for _, f := range st.fields {
			p.f("\t%s %s `json:\"%s,omitzero\"`", f.goName, f.typ, f.jsonName)
		}

		p.f("}")
		p.blank()
	}

	return p.buf.String(), nil
}

// operationsFile writes the operation list, the Handler interface and
// Unimplemented.
func (g *generator) operationsFile() (string, error) {
	p := &printer{}

	p.f("// Operations lists the operations of the %s API in name order.", g.name)
	p.f("var Operations = []string{")

	for _, name := range g.opNames {
		p.f("\t%q,", name)
	}

	p.f("}")
	p.blank()
	p.f("// Handler is what a %s provider implements: one method for each operation. A method", g.name)
	p.f("// returns the output, or an error. An *awserr.Error is written to the wire as it is,")
	p.f("// and any other error becomes an InternalError.")
	p.f("type Handler interface {")

	for _, name := range g.opNames {
		p.f("\t%s", handlerSignature(name))
	}

	p.f("}")
	p.blank()
	p.f("// Unimplemented implements every operation by returning awserr.NotImplemented.")
	p.f("// Embed it in a provider so that the operations it lacks answer with a 501.")
	p.f("type Unimplemented struct{}")
	p.blank()
	p.f("var _ Handler = Unimplemented{}")
	p.blank()

	for _, name := range g.opNames {
		p.f("// %s returns awserr.NotImplemented.", name)
		p.f("func (Unimplemented) %s(context.Context, *%sInput) (*%sOutput, error) {", name, name, name)
		p.f("\treturn nil, awserr.NotImplemented(%q, %q)", g.name, name)
		p.f("}")
		p.blank()
	}

	return p.buf.String(), nil
}

func handlerSignature(op string) string {
	return fmt.Sprintf("%s(ctx context.Context, in *%sInput) (*%sOutput, error)", op, op, op)
}

// checkJSONTimestamp rejects the timestamp forms the json codec cannot write:
// a timestampFormat other than epoch seconds on a list element or a map value.
// On a struct member the generated struct swaps the field while it is encoded.
func (g *generator) checkJSONTimestamp(r ref) error {
	if !g.hasProtocol(protoJSON) {
		return nil
	}

	if r.shape.Type == spec.TypeTimestamp {
		_, err := jsonTimeType(r.tsfmt)

		return err
	}

	var elem *spec.Ref

	switch r.shape.Type {
	case spec.TypeList:
		elem = r.shape.Member
	case spec.TypeMap:
		elem = r.shape.Value
	default:
		return nil
	}

	er, err := resolve(g.p, elem)
	if err != nil {
		return err
	}

	if er.shape.Type == spec.TypeTimestamp && jsonDefaultTime(er.tsfmt) {
		return nil
	}

	if er.shape.Type == spec.TypeTimestamp {
		return fmt.Errorf("timestampFormat %q on a collection element is not supported", er.tsfmt)
	}

	return g.checkJSONTimestamp(er)
}

// jsonDefaultTime reports whether a timestampFormat is epoch seconds, the json
// protocol's default.
func jsonDefaultTime(format string) bool {
	f := strings.ToLower(format)

	return f == "" || f == "unixtimestamp"
}

// jsonTimeType returns the awsjson type that writes a timestamp in the given
// format, or "" for the default.
func jsonTimeType(format string) (string, error) {
	switch strings.ToLower(format) {
	case "", "unixtimestamp":
		return "", nil
	case "iso8601":
		return "awsjson.ISO8601", nil
	case "rfc822":
		return "awsjson.RFC822", nil
	default:
		return "", fmt.Errorf("timestampFormat %q is not supported", format)
	}
}
