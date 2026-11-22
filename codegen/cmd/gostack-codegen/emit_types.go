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
			if !inJSONBody(st, f) {
				p.f("\t%s %s `json:\"-\"`", f.goName, f.typ)

				continue
			}

			p.f("\t%s %s `json:\"%s,omitzero\"`", f.goName, f.typ, f.jsonName)

			// The json encoder writes an encoded sibling in place of its member
			// when it is set: awsjson.Encoded.
			if e := f.encoded; e != nil {
				p.f("\t%s %s `json:\"-\"`", e.goName, e.typ)
			}
		}

		p.f("}")
		p.blank()

		if st.union {
			emitMembersSet(p, st)
		}
	}

	return p.buf.String(), nil
}

// emitMembersSet writes the method that counts the members of a union that
// are set. A member is set when it is present at all: an empty string, list
// or map, a false boolean and an empty blob each count.
func emitMembersSet(p *printer, st *structType) {
	p.f("// %s counts the members of v that are set, present even when empty. A", unionMethod)
	p.f("// valid %s has exactly one.", st.goName)
	p.f("func (v *%s) %s() int {", st.goName, unionMethod)
	p.f("\tn := 0")

	for _, f := range st.fields {
		p.f("\tif v.%s != nil {", f.goName)
		p.f("\t\tn++")
		p.f("\t}")
	}

	p.f("\treturn n")
	p.f("}")
	p.blank()
}

// operationsFile writes the operation list, the Handler interface,
// Unimplemented and OperationMethod.
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
		p.f("\t%s", handlerSignature(g.ops[name]))
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
		op := g.ops[name]

		p.f("// %s returns awserr.NotImplemented.", name)
		p.f("func (Unimplemented) %s(context.Context, *%s) (*%s, error) {", name, op.input.goName, op.output.goName)
		p.f("\treturn nil, awserr.NotImplemented(%q, %q)", g.name, name)
		p.f("}")
		p.blank()
	}

	g.operationMethod(p)

	return p.buf.String(), nil
}

// operationMethod writes OperationMethod: the method of a type for an
// operation, looked up with the operation's name as a constant. The linker
// keeps only the methods whose names a constant MethodByName mentions; one
// MethodByName call with a variable name would make it keep every exported
// method in the binary.
func (g *generator) operationMethod(p *printer) {
	p.f("// OperationMethod returns the method of t that implements operation, and false when")
	p.f("// t has none or operation is not one of the %s API. Every lookup names its", g.name)
	p.f("// method with a constant, so the linker keeps only the methods of those names.")
	p.f("func OperationMethod(t reflect.Type, operation string) (reflect.Method, bool) {")
	p.f("\tswitch operation {")

	for _, name := range g.opNames {
		p.f("\tcase %q:", name)
		p.f("\t\treturn t.MethodByName(%q)", name)
	}

	p.f("\tdefault:")
	p.f("\t\treturn reflect.Method{}, false")
	p.f("\t}")
	p.f("}")
}

// inJSONBody reports whether a member is one of the json object a rest-json
// body holds: not one bound to a header or the status, and not the payload,
// which is the whole body by itself. A querystring or uri member stays in, as
// botocore reads a response's from the body. json and query services have
// neither kind, so every member of theirs is in.
func inJSONBody(st *structType, f *field) bool {
	switch f.pair.p.binding {
	case spec.LocationHeader, spec.LocationHeaders, spec.LocationStatusCode:
		return false
	default:
		return st.payload != f
	}
}

// handlerSignature is the Handler method of an operation. Operations that
// share a root shape share its type.
func handlerSignature(op *operation) string {
	return fmt.Sprintf("%s(ctx context.Context, in *%s) (*%s, error)", op.name, op.input.goName, op.output.goName)
}

// checkJSONTimestamp rejects the timestamp forms the json codec cannot write:
// a timestampFormat other than epoch seconds on a list element or a map value.
// On a struct member the generated struct swaps the field while it is encoded.
func (g *generator) checkJSONTimestamp(r ref) error {
	if !g.hasProtocol(protoJSON) && !g.hasProtocol(protoRestJSON) {
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
