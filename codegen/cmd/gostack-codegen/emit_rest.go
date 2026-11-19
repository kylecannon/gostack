package main

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// goNil is nil in generated code.
const goNil = "nil"

// restProtocol is the rest protocol the service speaks.
func (g *generator) restProtocol() string {
	if g.hasProtocol(protoRestXML) {
		return protoRestXML
	}

	return protoRestJSON
}

// restDispatchFile writes, for a rest service, Metadata, the router, Serve,
// and for each operation the functions that read its request and write its
// response.
func (g *generator) restDispatchFile() (string, error) {
	rules, err := g.routeRules()
	if err != nil {
		return "", err
	}

	p := &printer{}

	g.emitMetadata(p)
	paths := emitRouter(p, rules)
	g.emitRestServe(p)

	for _, name := range g.opNames {
		op := g.ops[name]

		emitRestServeOperation(p, op)

		err = g.emitRestRead(p, op, paths[g.p.Operations[name].HTTP.Path()])
		if err != nil {
			return "", fmt.Errorf("operation %s: %w", name, err)
		}

		err = g.emitRestWrite(p, op)
		if err != nil {
			return "", fmt.Errorf("operation %s: %w", name, err)
		}
	}

	g.emitRestHelpers(p)

	return p.buf.String(), nil
}

// emitRouter writes the path patterns, the rules in the order routeRules gives
// them, and Route. It returns the variable that holds each path's pattern.
func emitRouter(p *printer, rules []routeRule) map[string]string {
	paths := map[string]string{}

	p.f("// The request paths of the operations, which Route and the readers match.")
	p.f("var (")

	for _, r := range rules {
		if _, ok := paths[r.path]; !ok {
			paths[r.path] = fmt.Sprintf("path%d", len(paths)+1)
			p.f("\t%s = rest.MustParsePattern(%q)", paths[r.path], r.path)
		}
	}

	p.f(")")
	p.blank()
	p.f("// router holds the rule of every operation, the most specific first.")
	p.f("var router = rest.NewRouter(")

	for _, r := range rules {
		p.f("\trest.Rule{")
		p.f("\t\tOperation: %q,", r.op)
		p.f("\t\tMethod: %q,", r.method)
		p.f("\t\tPath: %s,", paths[r.path])

		if len(r.query) > 0 {
			literals := make([]string, 0, len(r.query))
			for _, q := range r.query {
				literals = append(literals, strings.TrimSuffix(q.Key+"="+q.Value, "="))
			}

			p.f("\t\tQuery: %s,", goStrings(literals))
		}

		if len(r.params) > 0 {
			p.f("\t\tParams: %s,", goStrings(r.params))
		}

		if len(r.headers) > 0 {
			p.f("\t\tHeaders: %s,", goStrings(r.headers))
		}

		if r.host != "" {
			p.f("\t\tHost: %q,", r.host)
		}

		p.f("\t},")
	}

	p.f(")")
	p.blank()
	p.f("// Route returns the operation r calls, found by its method, path, query,")
	p.f("// headers and host, and false when it calls none.")
	p.f("func Route(r *http.Request) (string, bool) {")
	p.f("\treturn router.Route(r)")
	p.f("}")
	p.blank()

	return paths
}

func (g *generator) emitRestServe(p *printer) {
	p.f("// Serve finds the operation the call in rc makes, unless rc names it already,")
	p.f("// decodes the call, runs it on h and writes the response. It sets")
	p.f("// rc.Operation before decoding, so the caller sees which operation ran. An")
	p.f("// error from h is written in the protocol's error form.")
	p.f("func Serve(h Handler, w http.ResponseWriter, rc *request.Context) {")
	p.f("\tif rc.Operation == \"\" {")
	p.f("\t\top, ok := Route(rc.Request)")
	p.f("\t\tif !ok {")
	p.f("\t\t\twriteError(w, rc, errUnknownOperation())")
	p.f("\t\t\treturn")
	p.f("\t\t}")
	p.f("\t\trc.Operation = op")
	p.f("\t}")
	p.blank()
	p.f("\tswitch rc.Operation {")

	for _, name := range g.opNames {
		p.f("\tcase %q:", name)
		p.f("\t\tserve%s(h, w, rc)", name)
	}

	p.f("\tdefault:")
	p.f("\t\twriteError(w, rc, errUnknownOperation())")
	p.f("\t}")
	p.f("}")
	p.blank()
}

func emitRestServeOperation(p *printer, op *operation) {
	p.f("func serve%s(h Handler, w http.ResponseWriter, rc *request.Context) {", op.name)
	p.f("\tin, err := read%s(rc)", op.name)
	p.f("\tif err != nil {")
	p.f("\t\twriteError(w, rc, err)")
	p.f("\t\treturn")
	p.f("\t}")
	p.f("\tout, herr := h.%s(request.With(rc.Request.Context(), rc), in)", op.name)
	p.f("\tif herr != nil {")
	p.f("\t\twriteHandlerError(w, rc, herr)")
	p.f("\t\treturn")
	p.f("\t}")
	p.f("\tif out == nil {")
	p.f("\t\tout = &%s{}", op.output.goName)
	p.f("\t}")
	p.f("\twrite%s(w, rc, out)", op.name)
	p.f("}")
	p.blank()
}

// hasBinding reports whether a struct has a member bound to one of the
// locations.
func hasBinding(st *structType, locations ...string) bool {
	return slices.ContainsFunc(st.fields, func(f *field) bool { return slices.Contains(locations, f.pair.p.binding) })
}

// inXMLBody reports whether a member is an element of the xml body of its
// struct: not an attribute, not the payload, and not bound outside the body. A
// response has no uri or query string, so botocore reads a member bound there
// from the body, and that is where it is written.
func inXMLBody(st *structType, f *field, dir direction) bool {
	if f.pair.p.xmlAttr || st.payload == f {
		return false
	}

	switch f.pair.p.binding {
	case "":
		return true
	case spec.LocationURI, spec.LocationQuery:
		return dir == encoding
	default:
		return false
	}
}

// hasBody reports whether a struct with no payload has members in its body.
func (g *generator) hasBody(st *structType) bool {
	for _, f := range st.fields {
		xmlBody := g.hasProtocol(protoRestXML) && inXMLBody(st, f, encoding)
		if xmlBody || g.hasProtocol(protoRestJSON) && f.pair.p.binding == "" {
			return true
		}
	}

	return false
}

func (g *generator) emitRestRead(p *printer, op *operation, path string) error {
	in := op.input

	p.f("func read%s(rc *request.Context) (*%s, *awserr.Error) {", op.name, in.goName)

	err := g.emitRestBody(p, in)
	if err != nil {
		return err
	}

	if hasBinding(in, spec.LocationURI, spec.LocationQuery, spec.LocationHeader, spec.LocationHeaders) {
		labels := goNil
		if hasBinding(in, spec.LocationURI) {
			p.f("\tlabels, _ := %s.Match(rc.Request.URL.EscapedPath())", path)

			labels = "labels"
		}

		p.f("\td := rest.NewDecoder(rc.Request, %s)", labels)
		p.f("\tin.decodeBindings(d)")
		p.f("\tif err := d.Err(); err != nil {")
		p.f("\t\treturn nil, err")
		p.f("\t}")
	}

	p.f("\treturn in, nil")
	p.f("}")
	p.blank()

	return nil
}

// emitRestBody writes the part of a reader that declares in and fills it from
// the request body.
func (g *generator) emitRestBody(p *printer, in *structType) error {
	f := in.payload
	if f == nil {
		return g.emitRestRootBody(p, in)
	}

	p.f("\tin := &%s{}", in.goName)

	switch {
	case f.typ == typeReader:
		p.f("\tin.%s = rc.Request.Body", f.goName)

		return nil
	case strings.HasPrefix(f.typ, "*") && f.pair.p.shape.Type == spec.TypeStructure &&
		g.hasProtocol(protoRestXML):
		p.f("\txd := restxml.NewDecoderFor(rest.CapBody(rc.Request.Body), rc.DecodeFactor, rc)")
		p.f("\tif _, ok := xd.Root(); ok {")
		p.f("\t\tin.%s = &%s{}", f.goName, strings.TrimPrefix(f.typ, "*"))
		p.f("\t\tin.%s.decodeXML(xd)", f.goName)
		p.f("\t}")
		p.f("\tif xd.Err() != nil {")
		p.f("\t\treturn nil, %s", g.xmlBodyError())
		p.f("\t}")

		return nil
	}

	p.f("\tbody, err := readBody(rc)")
	p.f("\tif err != nil {")
	p.f("\t\treturn nil, err")
	p.f("\t}")
	p.f("\tif len(body) > 0 {")

	switch {
	case f.typ == typeBytes:
		p.f("\t\tin.%s = body", f.goName)
	case f.typ == typeStringPtr:
		p.f("\t\tin.%s = new(string(body))", f.goName)
	case f.typ == typeDocument:
		p.f("\t\tin.%s = jsontext.Value(body)", f.goName)
	case f.pair.p.shape.Type == spec.TypeStructure:
		p.f("\t\tin.%s, err = decodeJSONBody[%s](rc, body)", f.goName, strings.TrimPrefix(f.typ, "*"))
		p.f("\t\tif err != nil {")
		p.f("\t\t\treturn nil, err")
		p.f("\t\t}")
	default:
		return fmt.Errorf("payload %s of type %s is not supported", f.member, f.typ)
	}

	p.f("\t}")

	return nil
}

// emitRestRootBody reads the body of an input with no payload member, whose
// body members are the members of in.
func (g *generator) emitRestRootBody(p *printer, in *structType) error {
	switch {
	case !g.hasBody(in):
		p.f("\tin := &%s{}", in.goName)
	case g.hasProtocol(protoRestXML):
		p.f("\tin := &%s{}", in.goName)
		p.f("\txd := restxml.NewDecoderFor(rest.CapBody(rc.Request.Body), rc.DecodeFactor, rc)")
		p.f("\tif _, ok := xd.Root(); ok {")
		p.f("\t\tin.decodeXML(xd)")
		p.f("\t}")
		p.f("\tif xd.Err() != nil {")
		p.f("\t\treturn nil, %s", g.xmlBodyError())
		p.f("\t}")
	default:
		p.f("\tbody, err := readBody(rc)")
		p.f("\tif err != nil {")
		p.f("\t\treturn nil, err")
		p.f("\t}")
		p.f("\tin := &%s{}", in.goName)
		p.f("\tif len(body) > 0 {")
		p.f("\t\tin, err = decodeJSONBody[%s](rc, body)", in.goName)
		p.f("\t\tif err != nil {")
		p.f("\t\t\treturn nil, err")
		p.f("\t\t}")
		p.f("\t}")
	}

	return nil
}

// status is the Go expression of an operation's response status.
func status(op *operation) string {
	code := op.http.code
	if code == 0 {
		code = 200
	}

	for _, f := range op.output.fields {
		if f.pair.p.binding == spec.LocationStatusCode {
			return fmt.Sprintf("rest.Status(out.%s, %d)", f.goName, code)
		}
	}

	return strconv.Itoa(code)
}

func (g *generator) emitRestWrite(p *printer, op *operation) error {
	out := op.output

	p.f("func write%s(w http.ResponseWriter, rc *request.Context, out *%s) {", op.name, out.goName)

	if g.hasProtocol(protoRestJSON) {
		err := emitJSONWrite(p, op)
		if err != nil {
			return err
		}
	} else {
		err := g.emitXMLWrite(p, op)
		if err != nil {
			return err
		}
	}

	p.f("}")
	p.blank()

	return nil
}

// emitPayloadStream writes the declaration of body, the reader of a blob or
// string payload, and returns its default Content-Type.
func emitPayloadStream(p *printer, f *field) (string, bool) {
	switch f.typ {
	case typeReadCloser:
		p.f("\tvar body io.Reader")
		p.f("\tif out.%s != nil {", f.goName)
		p.f("\t\tdefer func() { _ = out.%s.Close() }()", f.goName)
		p.f("\t\tbody = out.%s", f.goName)
		p.f("\t}")
	case typeBytes:
		p.f("\tvar body io.Reader")
		p.f("\tif out.%s != nil {", f.goName)
		p.f("\t\tbody = bytes.NewReader(out.%s)", f.goName)
		p.f("\t}")
	case typeStringPtr:
		p.f("\tvar body io.Reader")
		p.f("\tif out.%s != nil {", f.goName)
		p.f("\t\tbody = strings.NewReader(*out.%s)", f.goName)
		p.f("\t}")

		return "text/plain", true
	default:
		return "", false
	}

	return "application/octet-stream", true
}

func (g *generator) emitXMLWrite(p *printer, op *operation) error {
	out := op.output

	if hasBinding(out, spec.LocationHeader, spec.LocationHeaders) {
		p.f("\tout.encodeBindings(w.Header())")
	}

	if f := out.payload; f != nil {
		if contentType, ok := emitPayloadStream(p, f); ok {
			p.f("\tenv := &restxml.Envelope{RequestID: rc.RequestID, ContentType: %q}", contentType)
			p.f("\t_ = restxml.WriteStream(w, env, %s, body)", status(op))

			return nil
		}

		if f.pair.p.shape.Type != spec.TypeStructure {
			return fmt.Errorf("payload %s of type %s is not supported", f.member, f.typ)
		}

		root := cmp.Or(f.pair.p.loc, f.pair.p.name)

		p.f("\tenv := &restxml.Envelope{RequestID: rc.RequestID}")
		p.f("\tif out.%s == nil {", f.goName)
		p.f("\t\trestxml.WriteResponse(w, env, %s, nil)", status(op))
		p.f("\t\treturn")
		p.f("\t}")
		p.f("\trestxml.WriteResponse(w, env, %s, func(xw *restxml.Writer) {", status(op))
		openElement(p, "\t\t", root, g.responseNS(f.pair.p.xmlns))
		p.f("\t\tout.%s.encodeXML(xw)", f.goName)
		p.f("\t\txw.Close(%q)", root)
		p.f("\t})")

		return nil
	}

	p.f("\tenv := &restxml.Envelope{RequestID: rc.RequestID}")

	if !g.hasBody(out) {
		p.f("\trestxml.WriteResponse(w, env, %s, nil)", status(op))

		return nil
	}

	p.f("\trestxml.WriteResponse(w, env, %s, func(xw *restxml.Writer) {", status(op))
	openElement(p, "\t\t", op.outRoot, op.outNS)
	p.f("\t\tout.encodeXML(xw)")
	p.f("\t\txw.Close(%q)", op.outRoot)
	p.f("\t})")

	return nil
}

func emitJSONWrite(p *printer, op *operation) error {
	out := op.output
	contentType := "application/json"

	f := out.payload
	switch {
	case f == nil:
		p.f("\tdata, err := awsjson.Marshal(out)")
	case f.typ == typeDocument:
		p.f("\tdata := []byte(out.%s)", f.goName)
	case f.pair.p.shape.Type == spec.TypeStructure:
		p.f("\tvar data []byte")
		p.f("\tvar err error")
		p.f("\tif out.%s != nil {", f.goName)
		p.f("\t\tdata, err = awsjson.Marshal(out.%s)", f.goName)
		p.f("\t}")
	default:
	}

	if f == nil || f.typ != typeDocument && f.pair.p.shape.Type == spec.TypeStructure {
		p.f("\tif err != nil {")
		p.f("\t\twriteError(w, rc, awserr.Internal(err.Error()))")
		p.f("\t\treturn")
		p.f("\t}")
	}

	if hasBinding(out, spec.LocationHeader, spec.LocationHeaders) {
		p.f("\tout.encodeBindings(w.Header())")
	}

	if f != nil && f.typ != typeDocument && f.pair.p.shape.Type != spec.TypeStructure {
		ct, ok := emitPayloadStream(p, f)
		if !ok {
			return fmt.Errorf("payload %s of type %s is not supported", f.member, f.typ)
		}

		contentType = ct
	} else {
		p.f("\tvar body io.Reader")
		p.f("\tif data != nil {")
		p.f("\t\tbody = bytes.NewReader(data)")
		p.f("\t}")
	}

	p.f("\t_ = awsjson.WriteRestResponse(w, rc.RequestID, %s, %q, body)", status(op), contentType)

	return nil
}

// openElement writes the start tag of an element and the namespace it
// declares.
func openElement(p *printer, indent, name string, ns *spec.XMLNamespace) {
	p.f("%sxw.Open(%q)", indent, name)

	if ns != nil {
		p.f("%sxw.Namespace(%q, %q)", indent, ns.Prefix, ns.URI)
	}
}

// emitRestHelpers writes the error writers and the body reader the operations
// share.
func (g *generator) emitRestHelpers(p *printer) {
	p.f("// asError turns what a handler returned into the error that goes on the wire.")
	p.f("func asError(err error) *awserr.Error {")
	p.f("\tvar ae *awserr.Error")
	p.f("\tif errors.As(err, &ae) && ae != nil {")
	p.f("\t\treturn ae")
	p.f("\t}")
	p.f("\treturn awserr.Internal(err.Error())")
	p.f("}")
	p.blank()
	g.emitWriteHandlerError(p)
	g.emitErrorHelpers(p)
}

// emitWriteHandlerError writes writeHandlerError, which writes what a handler
// returned with the members its error carries.
func (g *generator) emitWriteHandlerError(p *printer) {
	p.f("// writeHandlerError writes what a handler returned: an error with members of")
	p.f("// its own, as its Err method made it, with them, and any other one as it is.")
	p.f("func writeHandlerError(w http.ResponseWriter, rc *request.Context, err error) {")

	if g.hasModeledErrors() {
		p.f("\tvar me *modeledError")
		p.f("\tif errors.As(err, &me) {")
		p.f("\t\tme.members.writeError(w, rc, me.err)")
		p.f("\t\treturn")
		p.f("\t}")
	}

	p.f("\twriteError(w, rc, asError(err))")
	p.f("}")
	p.blank()
}

// emitErrorHelpers writes writeError, the errors of the protocol and the body
// reader.
func (g *generator) emitErrorHelpers(p *printer) {
	p.f("// writeError writes err with no members of its own.")
	p.f("func writeError(w http.ResponseWriter, rc *request.Context, err *awserr.Error) {")
	p.f("\t%s", g.errorWriter(goNil))
	p.f("}")
	p.blank()

	// OPEN QUESTION: Smithy's rest protocols answer a request that matches no
	// operation with 404 UnknownOperationException. LocalStack's own answer is
	// not pinned by a parity test, so the Smithy one stands.
	p.f("// errUnknownOperation is the error for a request that calls no operation.")
	p.f("func errUnknownOperation() *awserr.Error {")
	p.f("\treturn &awserr.Error{")
	p.f("\t\tCode: \"UnknownOperationException\",")
	p.f("\t\tMessage: \"The request does not call an operation of the %s service.\",", g.name)
	p.f("\t\tStatusCode: 404,")
	p.f("\t\tFault: awserr.FaultSender,")
	p.f("\t}")
	p.f("}")
	p.blank()

	// A rest-xml service answers with AWS's MalformedXML and its message, which
	// the restxml runtime holds.
	code, message := "SerializationException", `"The request body is not well-formed or does not match the model."`
	if g.hasProtocol(protoRestXML) {
		code, message = "MalformedXML", "restxml.MalformedXMLMessage"
	}

	p.f("// errMalformedBody is the error for a request body that does not parse.")
	p.f("func errMalformedBody() *awserr.Error {")
	p.f("\treturn &awserr.Error{")
	p.f("\t\tCode: %q,", code)
	p.f("\t\tMessage: %s,", message)
	p.f("\t\tStatusCode: 400,")
	p.f("\t\tFault: awserr.FaultSender,")
	p.f("\t}")
	p.f("}")
	p.blank()
	g.emitBodyTooLarge(p)
	g.emitReadBody(p)
	g.emitMessageName(p)

	if g.hasModeledErrors() {
		emitModeledError(p)
	}
}

func (g *generator) emitReadBody(p *printer) {
	p.f("// readBody reads a request body that is not streamed, up to protocol.MaxBody bytes.")
	p.f("func readBody(rc *request.Context) ([]byte, *awserr.Error) {")
	p.f("\tbody, err := protocol.ReadBody(rc.Request.Body, rc.Request.ContentLength)")
	p.f("\tif errors.Is(err, protocol.ErrBusy) {")
	p.f("\t\treturn nil, %s", g.busyError())
	p.f("\t}")

	if g.opts.S3Errors {
		p.f("\tif protocol.OverBody(len(body)) {")
		p.f("\t\treturn nil, errBodyTooLarge()")
		p.f("\t}")
		p.f("\tif err != nil {")
	} else {
		p.f("\tif err != nil || protocol.OverBody(len(body)) {")
	}

	p.f("\t\treturn nil, errMalformedBody()")
	p.f("\t}")
	p.f("\treturn body, nil")
	p.f("}")
	p.blank()

	if g.hasProtocol(protoRestJSON) {
		p.f("// decodeJSONBody reads a json request body into a T, refusing one that would")
		p.f("// decode into more than the call's budget as a body over protocol.MaxBody.")
		p.f("func decodeJSONBody[T any](rc *request.Context, body []byte) (*T, *awserr.Error) {")
		p.f("\tin, _, err := awsjson.DecodeWithin[T](body, rc.DecodeBudget(len(body)), rc)")
		p.f("\tif err == awsjson.ErrTooLarge {")

		if g.opts.S3Errors {
			p.f("\t\treturn nil, errBodyTooLarge()")
		} else {
			p.f("\t\treturn nil, errMalformedBody()")
		}

		p.f("\t}")
		p.f("\treturn in, err")
		p.f("}")
		p.blank()
	}
}

// emitBodyTooLarge writes what an S3 request body over protocol.MaxBody is
// answered with. S3 has no such limit on a configuration document; LocalStack
// accepts any size, so this error is gostack's own, and the code and message
// are AWS's for a request that is too big.
func (g *generator) emitBodyTooLarge(p *printer) {
	if !g.opts.S3Errors {
		if g.hasProtocol(protoRestXML) {
			p.f("// errXMLBody is the error for an xml request body that failed to decode:")
			p.f("// throttled when the bodies in flight left it no room, malformed otherwise.")
			p.f("func errXMLBody(err error) *awserr.Error {")
			p.f("\tif errors.Is(err, protocol.ErrBusy) {")
			p.f("\t\treturn %s", g.busyError())
			p.f("\t}")
			p.f("\treturn errMalformedBody()")
			p.f("}")
			p.blank()
		}

		return
	}

	p.f("// errBodyTooLarge is the error for a request body over protocol.MaxBody.")
	p.f("func errBodyTooLarge() *awserr.Error {")
	p.f("\treturn &awserr.Error{")
	p.f("\t\tCode: \"MaxMessageLengthExceeded\",")
	p.f("\t\tMessage: \"Your request was too big.\",")
	p.f("\t\tStatusCode: 400,")
	p.f("\t\tFault: awserr.FaultSender,")
	p.f("\t}")
	p.f("}")
	p.blank()
	p.f("// errXMLBody is the error for an xml request body that failed to decode: too")
	p.f("// large when the read hit protocol.MaxBody, SlowDown when the bodies in flight")
	p.f("// left it no room, malformed otherwise.")
	p.f("func errXMLBody(err error) *awserr.Error {")
	p.f("\tif errors.Is(err, protocol.ErrBusy) {")
	p.f("\t\treturn protocol.SlowDown()")
	p.f("\t}")
	p.f("\tif errors.Is(err, rest.ErrBodyTooLarge) {")
	p.f("\t\treturn errBodyTooLarge()")
	p.f("\t}")
	p.f("\treturn errMalformedBody()")
	p.f("}")
	p.blank()
}

// xmlBodyError is the Go expression a reader returns for an xml decoder whose
// Err is set.
func (*generator) xmlBodyError() string {
	return "errXMLBody(xd.Err())"
}

// busyError is the Go expression for the answer to a body that failed with
// protocol.ErrBusy: S3's SlowDown, otherwise the protocol's throttling error.
func (g *generator) busyError() string {
	switch {
	case g.opts.S3Errors:
		return "protocol.SlowDown()"
	case g.hasProtocol(protoRestXML):
		return "protocol.ThrottledQuery()"
	default:
		return "protocol.ThrottledJSON()"
	}
}

func emitModeledError(p *printer) {
	{
		p.f("// errorMembers are the members of an error shape, which write the error with them.")
		p.f("type errorMembers interface {")
		p.f("\twriteError(w http.ResponseWriter, rc *request.Context, err *awserr.Error)")
		p.f("}")
		p.blank()
		p.f("// modeledError is an error that carries the members of its shape.")
		p.f("type modeledError struct {")
		p.f("\terr *awserr.Error")
		p.f("\tmembers errorMembers")
		p.f("}")
		p.blank()
		p.f("func (e *modeledError) Error() string { return e.err.Error() }")
		p.blank()
		p.f("func (e *modeledError) Unwrap() error { return e.err }")
		p.blank()
	}
}

// errorWriter is the statement that writes err, with extra writing the error's
// own body members in rest-xml and members holding them in rest-json.
func (g *generator) errorWriter(extra string) string {
	if g.hasProtocol(protoRestJSON) {
		members := ""
		if extra != goNil {
			members = ", Members: v"
		}

		if len(g.messageNames()) > 0 {
			members += ", MessageName: messageName(err.Code)"
		}

		return "awsjson.WriteError(w, rc.Request, rc.RequestID, err, " +
			"awsjson.ErrorOptions{ContentType: \"application/json\"" + members + "})"
	}

	writer := "WriteError"
	if g.opts.S3Errors {
		writer = "WriteS3Error"
	}

	return fmt.Sprintf("restxml.%s(w, &restxml.Envelope{RequestID: rc.RequestID}, err, %s)", writer, extra)
}

// messageNames maps the code of each json or rest-json error whose shape names
// its message member other than "message" to that name, which botocore reads
// the member from and LocalStack writes it under.
func (g *generator) messageNames() map[string]string {
	names := map[string]string{}

	if !g.hasProtocol(protoRestJSON) && !g.hasProtocol(protoJSON) {
		return names
	}

	for _, ex := range g.exceptions {
		for _, m := range g.p.Shapes[ex.shape].Members {
			if strings.EqualFold(m.Name, "message") && m.Name != "message" {
				names[ex.code] = m.Name
			}
		}
	}

	return names
}

func (g *generator) emitMessageName(p *printer) {
	names := g.messageNames()
	if len(names) == 0 {
		return
	}

	p.f("// messageName is the json name of the message of an error, by its code: the")
	p.f("// name of its shape's message member, where that is not \"message\".")
	p.f("func messageName(code string) string {")
	p.f("\tswitch code {")

	for _, code := range sortedKeys(names) {
		p.f("\tcase %q:", code)
		p.f("\t\treturn %q", names[code])
	}

	p.f("\tdefault:")
	p.f("\t\treturn \"\"")
	p.f("\t}")
	p.f("}")
	p.blank()
}

func (g *generator) hasModeledErrors() bool {
	for _, ex := range g.exceptions {
		if ex.members != nil {
			return true
		}
	}

	return false
}
