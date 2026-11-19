package main

import (
	"fmt"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// protocolKinds maps a spec protocol name to the protocol.Kind constant.
var protocolKinds = map[string]string{
	protoJSON:     "protocol.KindJSON",
	protoQuery:    "protocol.KindQuery",
	protoRestJSON: "protocol.KindRestJSON",
	protoRestXML:  "protocol.KindRestXML",
}

// dispatchFile writes Metadata, Serve and ServeValidated, the per-operation
// functions behind them, and the checks for required members.
func (g *generator) dispatchFile() (string, error) {
	if g.isRest() {
		return g.restDispatchFile()
	}

	p := &printer{}

	g.emitMetadata(p)
	g.emitServe(p)

	for _, name := range g.opNames {
		g.emitServeOperation(p, g.ops[name])
	}

	g.emitHelpers(p)

	// The validate methods of a service with the Validation option replace
	// missingRequired.
	for _, st := range g.sortedStructs() {
		if g.opts.Validation != nil {
			break
		}

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
	if g.opts.Validation != nil {
		p.f("// ServeValidated is Serve. The generated checks never reject a request on their")
		p.f("// own for this service: h calls Validate on the input where LocalStack makes the")
		p.f("// check, and a required member can reach h as nil.")
	} else {
		p.f("// ServeValidated is Serve for an endpoint that rejects a request with a required")
		p.f("// member missing before it reaches h, as SQS's queue-URL paths do.")
	}

	p.f("func ServeValidated(h Handler, w http.ResponseWriter, rc *request.Context) {")
	p.f("\tserve(h, w, rc, true)")
	p.f("}")
	p.blank()
	p.f("func serve(h Handler, w http.ResponseWriter, rc *request.Context, validate bool) {")

	if g.hasProtocol(protoQuery) {
		// AWS Common Errors: "MissingAction ... HTTP Status Code: 400". LocalStack
		// answers 500 for a query call with no Action, so this one differs from it.
		p.f("\tif rc.Operation == \"\" && rc.Protocol == protocol.KindQuery {")
		p.f("\t\twriteError(w, rc, &awserr.Error{")
		p.f("\t\t\tCode: \"MissingAction\",")
		p.f("\t\t\tMessage: \"The request is missing an action or a required parameter.\",")
		p.f("\t\t\tStatusCode: 400,")
		p.f("\t\t\tFault: awserr.FaultSender,")
		p.f("\t\t})")
		p.f("\t\treturn")
		p.f("\t}")
		p.blank()
	}

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

	// A Validation service's handler runs in.Validate itself, where LocalStack
	// makes the check, so nothing runs here for it.
	if g.opts.Validation == nil && in.needsCheck {
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

	if g.hasModeledErrors() {
		p.f("\t\twriteHandlerError(w, rc, herr)")
	} else {
		p.f("\t\twriteError(w, rc, asError(herr))")
	}

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
		p.f("\t\treturn decodeJSON[%s](rc, body)", op.input.goName)
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

	if g.hasModeledErrors() {
		g.emitWriteHandlerError(p)
		emitModeledError(p)
	}

	g.emitWriteError(p)
	g.emitMessageName(p)
	g.emitReaders(p)

	for _, ex := range g.exceptions {
		if ex.members != nil {
			g.emitMembersErrorWriter(p, ex.members)
		}
	}
}

// emitMembersErrorWriter writes the method that writes an error of a json or
// query service with the members of its shape: beside __type in json, and in
// query after the RequestId of the ErrorResponse, where LocalStack writes them
// (serializer.py, _add_additional_error_tags) and botocore reads them. Smithy's
// awsQuery puts them inside <Error>, and botocore's ignore list skips that
// case (QueryComplexError), so LocalStack's place stands.
func (g *generator) emitMembersErrorWriter(p *printer, st *structType) {
	trim := g.hasProtocol(protoJSON) && emitOmitEmpty(p, st)

	p.f("// writeError writes err with the members in v.")
	p.f("func (v *%s) writeError(w http.ResponseWriter, rc *request.Context, err *awserr.Error) {", st.goName)
	p.f("\tswitch rc.Protocol {")

	if g.hasProtocol(protoJSON) {
		members := "v"
		if trim {
			members = "v.omitEmpty()"
		}

		p.f("\tcase protocol.KindJSON:")
		p.f("\t\topts := %s", g.jsonErrorOptions("Members: "+members))
		p.f("\t\tawsjson.WriteError(w, rc.Request, rc.RequestID, err, opts)")
	}

	if g.hasProtocol(protoQuery) {
		p.f("\tcase protocol.KindQuery:")
		p.f("\t\tenv := &query.Envelope{Namespace: queryNamespace, RequestID: rc.RequestID, EscapeQuotes: queryEscapeQuotes}")
		p.f("\t\tquery.WriteModeledError(w, env, err, v.encodeQuery)")
	}

	p.f("\tdefault:")
	p.f("\t\thttp.Error(w, err.Message, err.StatusCode)")
	p.f("\t}")
	p.f("}")
	p.blank()
}

// emitOmitEmpty writes the omitEmpty method of an error's members, which
// clears every member a json error leaves out: one that is not required and
// is empty, a zero number, false, or an empty string, list, map or blob. That
// is LocalStack's rule (serializer.py, JSONResponseSerializer._serialize_error:
// "if value or member in shape.required_members"). A structure or timestamp
// member is left as it is: none of the json services has one. It reports
// whether it wrote the method, which an error with nothing to clear does not.
func emitOmitEmpty(p *printer, st *structType) bool {
	body := &printer{}

	for _, f := range st.fields {
		if f.required {
			continue
		}

		x := "c." + f.goName

		switch f.pair.p.shape.Type {
		case spec.TypeList, spec.TypeMap, spec.TypeBlob:
			body.f("\tif len(%s) == 0 {", x)
		case spec.TypeString:
			body.f("\tif %s != nil && *%s == \"\" {", x, x)
		case spec.TypeInteger, spec.TypeLong, spec.TypeFloat, spec.TypeDouble:
			body.f("\tif %s != nil && *%s == 0 {", x, x)
		case spec.TypeBoolean:
			body.f("\tif %s != nil && !*%s {", x, x)
		default:
			continue
		}

		body.f("\t\t%s = nil", x)
		body.f("\t}")
	}

	if body.buf.Len() == 0 {
		return false
	}

	p.f("// omitEmpty returns v without the members a json error leaves out: one that")
	p.f("// is empty and not required.")
	p.f("func (v *%s) omitEmpty() *%s {", st.goName, st.goName)
	p.f("\tc := *v")
	p.buf.Write(body.buf.Bytes())
	p.f("\treturn &c")
	p.f("}")
	p.blank()

	return true
}

// jsonErrorOptions is the awsjson.ErrorOptions literal of a json error, with
// extra fields after the ones every json error sets.
func (g *generator) jsonErrorOptions(extra ...string) string {
	fields := []string{
		"QueryCompatible: awsQueryCompatible",
		"ContentType: \"application/x-amz-json-\" + jsonVersion",
	}

	if len(g.messageNames()) > 0 {
		fields = append(fields, "MessageName: messageName(err.Code)")
	}

	fields = append(fields, extra...)

	return "awsjson.ErrorOptions{\n\t\t\t" + strings.Join(fields, ",\n\t\t\t") + ",\n\t\t}"
}

// emitReaders writes the functions that read a request body in each protocol.
func (g *generator) emitReaders(p *printer) {
	if g.hasProtocol(protoJSON) {
		p.f("func readBody(rc *request.Context) ([]byte, *awserr.Error) {")
		p.f("\tbody, err := protocol.ReadBody(rc.Request.Body, rc.Request.ContentLength)")
		p.f("\tif errors.Is(err, protocol.ErrBusy) {")
		p.f("\t\treturn nil, protocol.ThrottledJSON()")
		p.f("\t}")
		p.f("\tif err != nil {")
		p.f("\t\treturn nil, &awserr.Error{")
		p.f("\t\t\tCode: \"SerializationException\",")
		p.f("\t\t\tMessage: \"The request body could not be read.\",")
		p.f("\t\t\tStatusCode: 400,")
		p.f("\t\t\tFault: awserr.FaultSender,")
		p.f("\t\t}")
		p.f("\t}")
		p.f("\tif protocol.OverBody(len(body)) {")
		p.f("\t\treturn nil, query.MalformedQueryString()")
		p.f("\t}")
		p.f("\tif !utf8.Valid(body) {")
		p.f("\t\treturn nil, errBodyNotUTF8")
		p.f("\t}")
		p.f("\treturn body, nil")
		p.f("}")
		p.blank()
		emitDecodeJSON(p)
		p.f("// errBodyNotUTF8 is the answer to a json body that is not valid UTF-8: the same as")
		p.f("// broken json and a lone surrogate escape (awsjson.Decode). LocalStack answers 500")
		p.f("// here, which is a crash in its parser and not an answer to copy.")
		p.f("var errBodyNotUTF8 = &awserr.Error{")
		p.f("\tCode: \"SerializationException\",")
		p.f("\tMessage: \"Start of structure or map found where not expected.\",")
		p.f("\tStatusCode: 400,")
		p.f("\tFault: awserr.FaultSender,")
		p.f("}")
		p.blank()
	}

	g.emitFormReader(p)
}

// emitDecodeJSON writes the function every operation's reader decodes a json
// body with. It keeps the body on the request only when the lenient pass was
// needed, the one case a handler reads it again (sentNumber in sqs).
func emitDecodeJSON(p *printer) {
	p.f("// decodeJSON reads the json body of a call into a T. A body that only the lenient")
	p.f("// reading takes stays on the request: only that reading clamps an integer past the")
	p.f("// int32 range, and a handler that refuses one echoes the text sent, which only the")
	p.f("// body holds. Any other body is dropped once decoded. A body that would decode into")
	p.f("// more than the call's budget is answered as a body over protocol.MaxBody.")
	p.f("func decodeJSON[T any](rc *request.Context, body []byte) (*T, *awserr.Error) {")
	p.f("\tin, lenient, err := awsjson.DecodeWithin[T](body, rc.DecodeBudget(len(body)), rc)")
	p.f("\tif err == awsjson.ErrTooLarge {")
	p.f("\t\treturn nil, query.MalformedQueryString()")
	p.f("\t}")
	p.f("\tif lenient {")
	p.f("\t\trc.Request.Body = io.NopCloser(bytes.NewReader(body))")
	p.f("\t}")
	p.f("\treturn in, err")
	p.f("}")
	p.blank()
}

// emitFormReader writes the function that reads the parameters of a query call.
func (g *generator) emitFormReader(p *printer) {
	if g.hasProtocol(protoQuery) {
		p.f("// readForm gives the parameters of a query call as LocalStack reads them: the URL's")
		p.f("// before the body's, with no \";\" separator and a malformed escape kept as typed.")
		p.f("func readForm(rc *request.Context) (url.Values, *awserr.Error) {")
		p.f("\tvals, err := query.RequestFormHeld(rc.Request, rc)")
		p.f("\tif errors.Is(err, protocol.ErrBusy) {")
		p.f("\t\treturn nil, protocol.ThrottledQuery()")
		p.f("\t}")
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
		p.f("\t\topts := %s", g.jsonErrorOptions())
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
	if r.shape.Document {
		return nil
	}

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
