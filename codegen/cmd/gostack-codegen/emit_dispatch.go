package main

import (
	"fmt"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// protocolKinds maps a spec protocol name to the protocol.Kind constant.
var protocolKinds = map[string]string{
	protoJSON:  "protocol.KindJSON",
	protoQuery: "protocol.KindQuery",
}

// dispatchFile writes Metadata, Serve and ServeValidated, the per-operation
// functions behind them, and the checks for required members.
func (g *generator) dispatchFile() (string, error) {
	p := &printer{}

	g.emitMetadata(p)
	g.emitServe(p)

	for _, name := range g.opNames {
		g.emitServeOperation(p, g.ops[name])
	}

	g.emitHelpers(p)

	for _, st := range g.sortedStructs() {
		err := g.emitMissingRequired(p, st)
		if err != nil {
			return "", fmt.Errorf("%s: %w", st.goName, err)
		}
	}

	return p.buf.String(), nil
}

func (g *generator) emitMetadata(p *printer) {
	md := g.p.Metadata

	signing := md.SigningName
	if signing == "" {
		signing = md.EndpointPrefix
	}

	kinds := make([]string, 0, len(g.protocols))
	for _, proto := range g.protocols {
		kinds = append(kinds, protocolKinds[proto])
	}

	p.f("// Metadata describes the %s service.", g.name)
	p.f("var Metadata = service.Metadata{")
	p.f("\tName: %q,", g.name)
	p.f("\tSigningName: %q,", signing)
	p.f("\tTargetPrefix: %q,", md.TargetPrefix)
	p.f("\tEndpointPrefix: %q,", md.EndpointPrefix)
	p.f("\tAPIVersion: %q,", md.APIVersion)
	p.f("\tProtocols: []protocol.Kind{%s},", strings.Join(kinds, ", "))
	p.f("\tOperations: Operations,")
	p.f("}")
	p.blank()
}

func (g *generator) emitServe(p *printer) {
	p.f("// Serve decodes the call in rc by its protocol, runs it on h and writes the")
	p.f("// response. An error from h is written in the protocol's error form.")
	p.f("func Serve(h Handler, w http.ResponseWriter, rc *request.Context) {")
	p.f("\tserve(h, w, rc, false)")
	p.f("}")
	p.blank()
	p.f("// ServeValidated is Serve for an endpoint that rejects a request with a required")
	p.f("// member missing before it reaches h, as SQS's queue-URL paths do.")
	p.f("func ServeValidated(h Handler, w http.ResponseWriter, rc *request.Context) {")
	p.f("\tserve(h, w, rc, true)")
	p.f("}")
	p.blank()
	p.f("func serve(h Handler, w http.ResponseWriter, rc *request.Context, validate bool) {")
	p.f("\tswitch rc.Operation {")

	for _, name := range g.opNames {
		p.f("\tcase %q:", name)
		p.f("\t\tserve%s(h, w, rc, validate)", name)
	}

	p.f("\tdefault:")
	p.f("\t\twriteError(w, rc, &awserr.Error{")
	p.f("\t\t\tCode: \"InvalidAction\",")
	p.f("\t\t\tMessage: \"Could not find operation \" + rc.Operation + \" for service %s.\",", g.name)
	p.f("\t\t\tStatusCode: 400,")
	p.f("\t\t\tFault: awserr.FaultSender,")
	p.f("\t\t})")
	p.f("\t}")
	p.f("}")
	p.blank()
}

func (g *generator) emitServeOperation(p *printer, op *operation) {
	in, out := op.input, op.output

	p.f("func serve%s(h Handler, w http.ResponseWriter, rc *request.Context, validate bool) {", op.name)
	p.f("\tin, err := read%s(rc)", op.name)

	if in.needsCheck {
		p.f("\tif err == nil && validate {")
		p.f("\t\tif member := in.missingRequired(); member != \"\" {")
		p.f("\t\t\terr = newMissingRequired(member)")
		p.f("\t\t}")
		p.f("\t}")
	}

	p.f("\tif err != nil {")
	p.f("\t\twriteError(w, rc, err)")
	p.f("\t\treturn")
	p.f("\t}")
	p.f("\tout, herr := h.%s(request.With(rc.Request.Context(), rc), in)", op.name)
	p.f("\tif herr != nil {")
	p.f("\t\twriteError(w, rc, asError(herr))")
	p.f("\t\treturn")
	p.f("\t}")
	p.f("\tif out == nil {")
	p.f("\t\tout = &%s{}", out.goName)
	p.f("\t}")
	p.f("\twrite%s(w, rc, out)", op.name)
	p.f("}")
	p.blank()

	g.emitRead(p, op)
	g.emitWrite(p, op)
}

// emitRead writes the function that decodes the request of an operation by
// the protocol the caller used.
func (g *generator) emitRead(p *printer, op *operation) {
	p.f("func read%s(rc *request.Context) (*%s, *awserr.Error) {", op.name, op.input.goName)
	p.f("\tswitch rc.Protocol {")

	if g.hasProtocol(protoJSON) {
		p.f("\tcase protocol.KindJSON:")
		p.f("\t\tbody, err := readBody(rc)")
		p.f("\t\tif err != nil {")
		p.f("\t\t\treturn nil, err")
		p.f("\t\t}")
		p.f("\t\treturn decode%sJSON(body)", op.name)
	}

	if g.hasProtocol(protoQuery) {
		p.f("\tcase protocol.KindQuery:")
		p.f("\t\tvals, err := readForm(rc)")
		p.f("\t\tif err != nil {")
		p.f("\t\t\treturn nil, err")
		p.f("\t\t}")
		p.f("\t\treturn decode%sQuery(vals)", op.name)
	}

	p.f("\tdefault:")
	p.f("\t\treturn nil, errUnsupportedProtocol(rc)")
	p.f("\t}")
	p.f("}")
	p.blank()
}

// emitWrite writes the function that writes the response of an operation in
// the protocol the caller used.
func (g *generator) emitWrite(p *printer, op *operation) {
	p.f("func write%s(w http.ResponseWriter, rc *request.Context, out *%s) {", op.name, op.output.goName)
	p.f("\tswitch rc.Protocol {")

	if g.hasProtocol(protoJSON) {
		p.f("\tcase protocol.KindJSON:")
		p.f("\t\tawsjson.WriteResponse(w, rc.RequestID, jsonVersion, out)")
	}

	if g.hasProtocol(protoQuery) {
		encode := "nil"
		if op.queryOutput {
			encode = "out.encodeQuery"
		}

		p.f("\tcase protocol.KindQuery:")
		p.f("\t\tquery.WriteResponse(w, &query.Envelope{")
		p.f("\t\t\tAction: %q,", op.name)
		p.f("\t\t\tNamespace: queryNamespace,")
		p.f("\t\t\tResult: %q,", op.queryResult)
		p.f("\t\t\tRequestID: rc.RequestID,")
		p.f("\t\t\tEscapeQuotes: queryEscapeQuotes,")
		p.f("\t\t}, %s)", encode)
	}

	p.f("\tdefault:")
	p.f("\t\twriteError(w, rc, errUnsupportedProtocol(rc))")
	p.f("\t}")
	p.f("}")
	p.blank()
}

// emitHelpers writes the functions every operation shares.
func (g *generator) emitHelpers(p *printer) {
	p.f("// errUnsupportedProtocol is the error for a call in a protocol the service lacks.")
	p.f("func errUnsupportedProtocol(rc *request.Context) *awserr.Error {")
	p.f("\treturn &awserr.Error{")
	p.f("\t\tCode: \"InvalidAction\",")
	p.f("\t\tMessage: \"The %s service does not accept this protocol.\",", g.name)
	p.f("\t\tStatusCode: 400,")
	p.f("\t\tFault: awserr.FaultSender,")
	p.f("\t}")
	p.f("}")
	p.blank()
	p.f("// asError turns what a handler returned into the error that goes on the wire.")
	p.f("func asError(err error) *awserr.Error {")
	p.f("\tvar ae *awserr.Error")
	p.f("\tif errors.As(err, &ae) && ae != nil {")
	p.f("\t\treturn ae")
	p.f("\t}")
	p.f("\treturn awserr.Internal(err.Error())")
	p.f("}")
	p.blank()

	g.emitWriteError(p)
	g.emitReaders(p)
}

// emitReaders writes the functions that read a request body in each protocol.
func (g *generator) emitReaders(p *printer) {
	if g.hasProtocol(protoJSON) {
		p.f("func readBody(rc *request.Context) ([]byte, *awserr.Error) {")
		p.f("\tbody, err := io.ReadAll(rc.Request.Body)")
		p.f("\tif err != nil {")
		p.f("\t\treturn nil, &awserr.Error{")
		p.f("\t\t\tCode: \"SerializationException\",")
		p.f("\t\t\tMessage: \"The request body could not be read.\",")
		p.f("\t\t\tStatusCode: 400,")
		p.f("\t\t\tFault: awserr.FaultSender,")
		p.f("\t\t}")
		p.f("\t}")
		p.f("\tif !utf8.Valid(body) {")
		p.f("\t\treturn nil, errBodyNotUTF8")
		p.f("\t}")
		p.f("\treturn body, nil")
		p.f("}")
		p.blank()
		p.f("// errBodyNotUTF8 is the answer to a json body that is not valid UTF-8. LocalStack's")
		p.f("// parser raises, and its exception handler turns that into a 500 of this form.")
		p.f("var errBodyNotUTF8 = &awserr.Error{")
		p.f("\tCode: \"CommonServiceException\",")
		p.f("\tMessage: \"exception while calling %s with unknown operation: \"+", g.name)
		p.f("\t\t\"HTTP body could not be parsed as JSON.\",")
		p.f("\tStatusCode: 500,")
		p.f("\tFault: awserr.FaultSender,")
		p.f("\tQueryCode: \"InternalError\",")
		p.f("}")
		p.blank()
	}

	if g.hasProtocol(protoQuery) {
		p.f("// readForm gives the parameters of a query call as LocalStack reads them: the URL's")
		p.f("// before the body's, with no \";\" separator and a malformed escape kept as typed.")
		p.f("func readForm(rc *request.Context) (url.Values, *awserr.Error) {")
		p.f("\tvals, err := query.RequestForm(rc.Request)")
		p.f("\tif err != nil {")
		p.f("\t\treturn nil, query.MalformedQueryString()")
		p.f("\t}")
		p.f("\treturn vals, nil")
		p.f("}")
		p.blank()
	}
}

func (g *generator) emitWriteError(p *printer) {
	p.f("// writeError writes err in the error form of the caller's protocol.")
	p.f("func writeError(w http.ResponseWriter, rc *request.Context, err *awserr.Error) {")
	p.f("\tswitch rc.Protocol {")

	if g.hasProtocol(protoJSON) {
		p.f("\tcase protocol.KindJSON:")
		p.f("\t\topts := awsjson.ErrorOptions{QueryCompatible: awsQueryCompatible}")
		p.f("\t\tawsjson.WriteError(w, rc.Request, rc.RequestID, err, opts)")
	}

	if g.hasProtocol(protoQuery) {
		p.f("\tcase protocol.KindQuery:")
		p.f("\t\tenv := &query.Envelope{Namespace: queryNamespace, RequestID: rc.RequestID, EscapeQuotes: queryEscapeQuotes}")
		p.f("\t\tquery.WriteError(w, env, err)")
	}

	p.f("\tdefault:")
	p.f("\t\thttp.Error(w, err.Message, err.StatusCode)")
	p.f("\t}")
	p.f("}")
	p.blank()
}

// emitMissingRequired writes the method that finds the first required member a
// struct lacks, looking through the structs below it.
func (g *generator) emitMissingRequired(p *printer, st *structType) error {
	if !st.needsCheck {
		return nil
	}

	p.f("// missingRequired returns the name of the first required member v lacks, or \"\".")
	p.f("func (v *%s) missingRequired() string {", st.goName)

	for _, f := range st.fields {
		if f.required {
			p.f("\tif v.%s == nil {", f.goName)
			p.f("\t\treturn %q", f.member)
			p.f("\t}")
		}
	}

	for _, f := range st.fields {
		err := g.checkNested(p, f.pair.p, "v."+f.goName, siteMember, 1)
		if err != nil {
			return fmt.Errorf("%s: %w", f.member, err)
		}
	}

	p.f("\treturn \"\"")
	p.f("}")
	p.blank()

	return nil
}

// checkNested writes the statements that look through the value in x for a
// structure with a missing member. at says whether x is a struct member, which
// may be nil, or a list element or map value.
func (g *generator) checkNested(p *printer, r ref, x string, at site, depth int) error {
	switch r.shape.Type {
	case spec.TypeStructure:
		if !g.structs[g.goTypeName(r.name)].needsCheck {
			return nil
		}

		if at == siteMember {
			p.f("\tif %s != nil {", x)
		}

		p.f("\t\tif member := %s.missingRequired(); member != \"\" {", x)
		p.f("\t\t\treturn member")
		p.f("\t\t}")

		if at == siteMember {
			p.f("\t}")
		}

		return nil
	case spec.TypeList:
		return g.checkElements(p, r.shape.Member, x, depth)
	case spec.TypeMap:
		return g.checkElements(p, r.shape.Value, x, depth)
	default:
		return nil
	}
}

// checkElements writes the loop over the elements of a list or the values of a
// map, when what they hold has a required member somewhere below it.
func (g *generator) checkElements(p *printer, elemRef *spec.Ref, x string, depth int) error {
	elem, err := resolve(g.p, elemRef)
	if err != nil {
		return err
	}

	needed := false

	for _, name := range g.nestedStructs(elem) {
		needed = needed || g.structs[g.goTypeName(name)].needsCheck
	}

	if !needed {
		return nil
	}

	v := fmt.Sprintf("e%d", depth)
	p.f("\tfor _, %s := range %s {", v, x)

	err = g.checkNested(p, elem, v, siteElement, depth+1)
	if err != nil {
		return err
	}

	p.f("\t}")

	return nil
}
