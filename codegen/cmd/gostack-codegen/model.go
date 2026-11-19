package main

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kylecannon/gostack/codegen/spec"
)

// ref is a reference to a shape with the traits it carries once the
// referencing member's own traits are applied over the shape's.
type ref struct {
	name  string
	shape *spec.Shape
	loc   string // the locationName trait
	flat  bool
	tsfmt string

	// The rest protocol traits. binding is the location trait, spec.Location*,
	// and "" for a body member.
	binding      string
	xmlns        *spec.XMLNamespace
	xmlAttr      bool
	streaming    bool
	hostLabel    bool
	eventPayload bool
	eventHeader  bool
	// jsonvalue marks a string header that carries json text in base64.
	jsonvalue bool
}

// The Go types of the values the rest emitters treat apart.
const (
	typeBytes      = "[]byte"
	typeStringPtr  = "*string"
	typeDocument   = "jsontext.Value"
	typeReader     = "io.Reader"
	typeReadCloser = "io.ReadCloser"
)

// site says where a value sits. A struct member holds a scalar by pointer, so
// absence shows as nil. A list element or map value holds it by value.
type site int

const (
	siteMember site = iota + 1
	siteElement
)

// direction says whether a codec decodes a struct or encodes it.
type direction int

const (
	decoding direction = iota + 1
	encoding
)

// pair is the same position in the primary model and the query model. The Go
// types come from the primary side and the query wire names from the other.
type pair struct {
	p ref
	q ref
}

// field is one member of a generated struct.
type field struct {
	member    string
	goName    string
	jsonName  string
	required  bool
	typ       string
	pair      pair
	hasQuery  bool
	queryName string
}

// structType is one generated struct.
type structType struct {
	goName     string
	shape      *spec.Shape
	queryShape *spec.Shape
	fields     []*field
	queryOrder []*field
	// decodes and encodes mark a struct below an operation input, which a
	// codec decodes, and one below an output or an error, which it encodes.
	decodes    bool
	encodes    bool
	needsCheck bool
	// payload is the field the shape's payload trait names, sent as the whole
	// body by a rest protocol, or nil.
	payload *field
}

// operation is one operation of the service.
type operation struct {
	name        string
	input       *structType
	output      *structType
	queryResult string
	queryOutput bool
	http        httpBinding
	deprecated  bool
	// outRoot is the root element of a rest-xml response body that has no
	// payload member: the locationName of the operation's output reference, or
	// of its shape, or else the shape's name, renamed by the service's
	// ResponseRoots. outNS is the namespace that root declares. A request
	// body's root is read under any name, so the input reference's traits are
	// not kept.
	outRoot string
	outNS   *spec.XMLNamespace
}

// httpBinding is where a rest protocol puts an operation: its method, request
// URI path and literal query, and the status of a successful response, 0 when
// the model leaves it to the protocol.
type httpBinding struct {
	method string
	path   []pathSegment
	query  []spec.QueryLiteral
	code   int
}

// pathSegment is one part of a request URI path between slashes: a literal, or
// a label that binds the uri member of the input it names. A greedy label,
// {Key+}, takes the rest of the path, slashes included.
type pathSegment struct {
	literal string
	label   *field
	greedy  bool
}

// exception is one error shape.
type exception struct {
	shape     string
	code      string
	status    int
	sender    bool
	queryCode string
	// members are the members a rest error carries besides its message, nil
	// when it has none.
	members *structType
}

// generator holds one service's models and what is derived from them.
type generator struct {
	name       string
	opts       serviceOptions
	p          *spec.Service
	q          *spec.Service
	protocols  []string
	opNames    []string
	ops        map[string]*operation
	structs    map[string]*structType
	rootNames  map[string]string
	exceptions []exception
}

const (
	protoJSON     = "json"
	protoQuery    = "query"
	protoRestJSON = "rest-json"
	protoRestXML  = "rest-xml"
)

func (g *generator) hasProtocol(name string) bool {
	return slices.Contains(g.protocols, name)
}

// isRest reports whether the service speaks rest-xml or rest-json.
func (g *generator) isRest() bool {
	return g.hasProtocol(protoRestXML) || g.hasProtocol(protoRestJSON)
}

// responseNS is the namespace the root element of a rest-xml response body
// declares: the service's ResponseNamespace, or else ns, the model's.
func (g *generator) responseNS(ns *spec.XMLNamespace) *spec.XMLNamespace {
	if g.opts.ResponseNamespace != "" {
		return &spec.XMLNamespace{URI: g.opts.ResponseNamespace}
	}

	return ns
}

// newGenerator reads the models of one service and works out its structs,
// operations and exceptions.
func newGenerator(name string, loader *spec.Loader) (*generator, error) {
	opts, ok := serviceTable[name]
	if !ok {
		return nil, fmt.Errorf("service %q has no entry in the service table", name)
	}

	p, err := loader.Botocore(name)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", name, err)
	}

	var q *spec.Service

	if opts.QueryModel != "" {
		q, err = loader.File(opts.QueryModel)
		if err != nil {
			return nil, fmt.Errorf("load %s query model: %w", name, err)
		}
	}

	return newModel(name, opts, p, q)
}

// newModel works out the structs, operations and exceptions of a service from
// its primary model p and, for a service whose query protocol another model
// describes, that query model q.
func newModel(name string, opts serviceOptions, p, q *spec.Service) (*generator, error) {
	g := &generator{
		name:      name,
		opts:      opts,
		p:         p,
		q:         q,
		ops:       map[string]*operation{},
		structs:   map[string]*structType{},
		rootNames: map[string]string{},
	}

	g.protocols = declaredProtocols(p)

	if q != nil {
		if !g.hasProtocol(protoQuery) {
			g.protocols = append(g.protocols, protoQuery)
		}
	} else if g.hasProtocol(protoQuery) {
		g.q = p
	}

	for _, proto := range g.protocols {
		switch proto {
		case protoJSON, protoQuery, protoRestJSON, protoRestXML:
		default:
			return nil, fmt.Errorf("%s: protocol %q is not supported yet", name, proto)
		}
	}

	// A rest service routes, reads and writes errors its own way, so it
	// speaks no other protocol beside its one rest protocol.
	if g.isRest() && len(g.protocols) > 1 {
		return nil, fmt.Errorf("%s: a rest protocol with another one, %v, is not supported", name, g.protocols)
	}

	err := g.build()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	return g, nil
}

func declaredProtocols(svc *spec.Service) []string {
	protocols := slices.Clone(svc.Metadata.Protocols)
	if len(protocols) == 0 && svc.Metadata.Protocol != "" {
		protocols = []string{svc.Metadata.Protocol}
	}

	return protocols
}

func (g *generator) build() error {
	g.opNames = sortedKeys(g.p.Operations)
	g.nameRoots()

	for _, name := range g.opNames {
		err := g.buildOperation(name)
		if err != nil {
			return fmt.Errorf("operation %s: %w", name, err)
		}
	}

	g.streamPayloads()

	err := g.buildExceptions()
	if err != nil {
		return err
	}

	g.markReachable()
	g.markChecks()

	return nil
}

// streamPayloads types a streaming blob or string payload as a stream, so a
// body is never buffered: an io.Reader in a request, an io.ReadCloser in a
// response, which Serve closes once written. A struct that is the root of both
// takes the io.ReadCloser, which is an io.Reader too.
func (g *generator) streamPayloads() {
	for _, name := range g.opNames {
		op := g.ops[name]

		for _, root := range []struct {
			st  *structType
			typ string
		}{{op.input, typeReader}, {op.output, typeReadCloser}} {
			f := root.st.payload
			if f == nil || !f.pair.p.streaming || f.typ == typeReadCloser {
				continue
			}

			if t := f.pair.p.shape.Type; t == spec.TypeBlob || t == spec.TypeString {
				f.typ = root.typ
			}
		}
	}
}

// nameRoots names the Go type of each operation's input and output shape. The
// root of one operation is <Operation>Input or <Operation>Output. A shape that
// several operations share keeps its own name, and they all use that one type:
// S3's GetBucketNotification and GetBucketNotificationConfiguration both take
// a GetBucketNotificationConfigurationRequest, and five lambda operations
// answer with a FunctionConfiguration.
func (g *generator) nameRoots() {
	uses := map[string]int{}

	for _, name := range g.opNames {
		op := g.p.Operations[name]

		for _, r := range []*spec.Ref{op.Input, op.Output} {
			if r != nil {
				uses[r.Shape]++
			}
		}
	}

	for _, name := range g.opNames {
		op := g.p.Operations[name]

		for _, root := range []struct {
			ref    *spec.Ref
			suffix string
		}{{op.Input, "Input"}, {op.Output, "Output"}} {
			switch {
			case root.ref == nil:
			case uses[root.ref.Shape] > 1:
				g.rootNames[root.ref.Shape] = shapeName(root.ref.Shape)
			default:
				g.rootNames[root.ref.Shape] = name + root.suffix
			}
		}
	}
}

func (g *generator) buildOperation(name string) error {
	op := g.p.Operations[name]
	out := &operation{name: name, deprecated: op.Deprecated}
	g.ops[name] = out

	if op.Output != nil {
		out.outRoot, out.outNS = op.Output.LocationName, op.Output.XMLNamespace

		if shape := g.p.Shapes[op.Output.Shape]; shape != nil {
			out.outRoot = cmp.Or(out.outRoot, shape.LocationName, op.Output.Shape)
			out.outNS = cmp.Or(out.outNS, shape.XMLNamespace)
		}

		out.outRoot = cmp.Or(g.opts.ResponseRoots[out.outRoot], out.outRoot)
		out.outNS = g.responseNS(out.outNS)
	}

	var err error

	// qop is the operation in the query model. It is nil when there is none, and
	// the nil operation has no input or output either.
	var qop *spec.Operation
	if g.q != nil {
		qop = g.q.Operations[name]
	}

	var qin, qout *spec.Ref
	if qop != nil {
		qin, qout = qop.Input, qop.Output
	}

	out.input, err = g.rootStruct(name+"Input", op.Input, qin)
	if err != nil {
		return err
	}

	out.output, err = g.rootStruct(name+"Output", op.Output, qout)
	if err != nil {
		return err
	}

	if qout != nil {
		out.queryOutput = true
		out.queryResult = qout.ResultWrapper

		if out.queryResult == "" {
			out.queryResult = qout.Shape
		}
	}

	if op.HTTP != nil {
		out.http, err = bindHTTP(op.HTTP, out.input)
	}

	return err
}

// bindHTTP reads an operation's http trait and binds each label of its path to
// the uri member of the input that the label names.
func bindHTTP(h *spec.HTTP, input *structType) (httpBinding, error) {
	b := httpBinding{method: h.Method, query: h.Query(), code: h.ResponseCode}

	path := strings.TrimPrefix(h.Path(), "/")
	if path == "" {
		return b, nil
	}

	for part := range strings.SplitSeq(path, "/") {
		inner, isLabel := strings.CutPrefix(part, "{")
		if !isLabel {
			if strings.ContainsAny(part, "{}") {
				return b, fmt.Errorf("request URI %s: a label must be a whole path segment", h.RequestURI)
			}

			b.path = append(b.path, pathSegment{literal: part})

			continue
		}

		label, ok := strings.CutSuffix(inner, "}")
		if !ok || label == "" || strings.ContainsAny(label, "{}") {
			return b, fmt.Errorf("request URI %s: malformed label %s", h.RequestURI, part)
		}

		seg := pathSegment{}
		label, seg.greedy = strings.CutSuffix(label, "+")

		for _, f := range input.fields {
			if f.pair.p.binding == spec.LocationURI && wireName(f) == label {
				seg.label = f
			}
		}

		if seg.label == nil {
			return b, fmt.Errorf("request URI %s: no uri member of %s is named %s", h.RequestURI, input.goName, label)
		}

		b.path = append(b.path, seg)
	}

	return b, nil
}

// wireName is the name a member has on the wire: its locationName, or else its
// member name.
func wireName(f *field) string {
	if f.pair.p.loc != "" {
		return f.pair.p.loc
	}

	return f.member
}

// rootStruct builds the struct of an operation's input or output. A missing
// shape gives an empty struct, so every operation has both.
func (g *generator) rootStruct(goName string, pr, qr *spec.Ref) (*structType, error) {
	if pr == nil {
		if _, taken := g.structs[goName]; taken {
			return nil, fmt.Errorf("the Go type %s of an absent root is a shape's as well", goName)
		}

		st := &structType{goName: goName}
		g.structs[goName] = st

		return st, nil
	}

	st, err := g.structFor(pr.Shape)
	if err != nil {
		return nil, err
	}

	if qr != nil {
		err = g.attachQuery(st, qr.Shape)
		if err != nil {
			return nil, err
		}
	}

	return st, nil
}

// structFor builds the struct of a primary-model structure shape, and of every
// structure below it, once.
func (g *generator) structFor(shapeName string) (*structType, error) {
	goName := g.goTypeName(shapeName)
	shape := g.p.Shapes[shapeName]

	if st, ok := g.structs[goName]; ok {
		if st.shape != shape {
			return nil, fmt.Errorf("shape %s and another shape would both be the Go type %s", shapeName, goName)
		}

		return st, nil
	}

	if shape == nil || shape.Type != spec.TypeStructure {
		return nil, fmt.Errorf("shape %s is not a structure", shapeName)
	}

	st := &structType{goName: goName, shape: shape}
	g.structs[goName] = st

	for _, m := range shape.Members {
		f, err := g.newField(shape, m)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", shapeName, m.Name, err)
		}

		st.fields = append(st.fields, f)

		if m.Name == shape.Payload {
			st.payload = f
		}

		err = g.buildNested(f.pair.p)
		if err != nil {
			return nil, err
		}
	}

	if shape.Payload != "" && st.payload == nil {
		return nil, fmt.Errorf("shape %s has no payload member %s", shapeName, shape.Payload)
	}

	// A root shape is matched to the query model's through its operation, in
	// rootStruct. Any other shape is matched by name.
	_, isRoot := g.rootNames[shapeName]
	if !isRoot && g.q != nil && g.q.Shapes[shapeName] != nil {
		err := g.attachQuery(st, shapeName)
		if err != nil {
			return nil, err
		}
	}

	return st, nil
}

// buildNested builds the structs a member's type refers to.
func (g *generator) buildNested(r ref) error {
	if r.shape.Document {
		return nil
	}

	switch r.shape.Type {
	case spec.TypeStructure:
		_, err := g.structFor(r.name)

		return err
	case spec.TypeList:
		elem, err := resolve(g.p, r.shape.Member)
		if err != nil {
			return err
		}

		return g.buildNested(elem)
	case spec.TypeMap:
		elem, err := resolve(g.p, r.shape.Value)
		if err != nil {
			return err
		}

		return g.buildNested(elem)
	default:
		return nil
	}
}

func (g *generator) newField(shape *spec.Shape, m spec.Member) (*field, error) {
	r, err := resolve(g.p, &m.Ref)
	if err != nil {
		return nil, err
	}

	typ, err := g.goType(r, siteMember)
	if err != nil {
		return nil, err
	}

	// A member bound outside the body holds its timestamps in the formats of
	// its location, which package rest reads and writes.
	if r.binding == "" {
		err = g.checkJSONTimestamp(r)
		if err != nil {
			return nil, err
		}
	}

	jsonName := m.Name
	if r.loc != "" {
		jsonName = r.loc
	}

	return &field{
		member:   m.Name,
		goName:   exportName(m.Name),
		jsonName: jsonName,
		required: shape.IsRequired(m.Name),
		typ:      typ,
		pair:     pair{p: r},
	}, nil
}

// attachQuery matches the members of a query-model structure to the fields of
// st, so the query codec reads the query wire names while the Go types stay
// those of the primary model.
func (g *generator) attachQuery(st *structType, queryShape string) error {
	qs := g.q.Shapes[queryShape]
	if qs == nil || qs.Type != spec.TypeStructure {
		return fmt.Errorf("query shape %s of %s is not a structure", queryShape, st.goName)
	}

	if st.queryShape != nil {
		return nil
	}

	st.queryShape = qs

	for _, qm := range qs.Members {
		var f *field

		// The models may differ in the case of a member name, as botocore's SQS
		// "tags" and the query model's "Tags" do. The Go field is the same.
		for _, cand := range st.fields {
			if cand.goName == exportName(qm.Name) {
				f = cand
			}
		}

		if f == nil {
			return fmt.Errorf("query member %s.%s has no counterpart in the primary model", queryShape, qm.Name)
		}

		qr, err := resolve(g.q, &qm.Ref)
		if err != nil {
			return err
		}

		f.pair.q = qr
		f.hasQuery = true
		f.queryName = qm.Name

		st.queryOrder = append(st.queryOrder, f)
	}

	return nil
}

// goTypeName is the Go name of a structure shape.
func (g *generator) goTypeName(shape string) string {
	if root, ok := g.rootNames[shape]; ok {
		return root
	}

	return shapeName(shape)
}

// goType is the Go type of a shape reference at the given site.
func (g *generator) goType(r ref, at site) (string, error) {
	// A document is any json value, kept as its text. nil is absent.
	if r.shape.Document {
		return typeDocument, nil
	}

	scalar, ok := scalarTypes[r.shape.Type]
	if ok {
		if at == siteMember && scalar != typeBytes && scalar != "*protocol.Time" {
			return "*" + scalar, nil
		}

		return scalar, nil
	}

	switch r.shape.Type {
	case spec.TypeStructure:
		if at == siteMember {
			return "*" + g.goTypeName(r.name), nil
		}

		return g.goTypeName(r.name), nil
	case spec.TypeList:
		return g.collectionType("[]", r.shape.Member)
	case spec.TypeMap:
		key, err := resolve(g.p, r.shape.Key)
		if err != nil {
			return "", err
		}

		if key.shape.Type != spec.TypeString {
			return "", fmt.Errorf("map %s has a key of type %s", r.name, key.shape.Type)
		}

		return g.collectionType("map[string]", r.shape.Value)
	default:
		return "", fmt.Errorf("shape %s has unsupported type %q", r.name, r.shape.Type)
	}
}

func (g *generator) collectionType(prefix string, elem *spec.Ref) (string, error) {
	er, err := resolve(g.p, elem)
	if err != nil {
		return "", err
	}

	et, err := g.goType(er, siteElement)
	if err != nil {
		return "", err
	}

	return prefix + et, nil
}

var scalarTypes = map[string]string{
	spec.TypeString:    "string",
	spec.TypeInteger:   "int32",
	spec.TypeLong:      "int64",
	spec.TypeFloat:     "float64",
	spec.TypeDouble:    "float64",
	spec.TypeBoolean:   "bool",
	spec.TypeTimestamp: "*protocol.Time",
	spec.TypeBlob:      "[]byte",
}

// resolve applies a reference's own traits over the ones on its shape.
func resolve(svc *spec.Service, r *spec.Ref) (ref, error) {
	if r == nil {
		return ref{}, errors.New("missing shape reference")
	}

	shape, ok := svc.Shapes[r.Shape]
	if !ok {
		return ref{}, fmt.Errorf("unknown shape %q", r.Shape)
	}

	out := ref{
		name:         r.Shape,
		shape:        shape,
		loc:          shape.LocationName,
		flat:         shape.Flattened || r.Flattened,
		tsfmt:        shape.TimestampFormat,
		binding:      r.Location,
		xmlns:        shape.XMLNamespace,
		xmlAttr:      r.XMLAttribute,
		streaming:    shape.Streaming || r.Streaming,
		hostLabel:    r.HostLabel,
		eventPayload: r.EventPayload,
		eventHeader:  r.EventHeader,
		jsonvalue:    shape.JSONValue || r.JSONValue,
	}

	if r.LocationName != "" {
		out.loc = r.LocationName
	}

	if r.TimestampFormat != "" {
		out.tsfmt = r.TimestampFormat
	}

	if r.XMLNamespace != nil {
		out.xmlns = r.XMLNamespace
	}

	return out, nil
}

// markReachable flags the structs the query or rest codec has to decode, those
// below an operation input, and to encode, those below an output or the
// members of a rest error.
func (g *generator) markReachable() {
	if g.q == nil && !g.isRest() {
		return
	}

	for _, name := range g.opNames {
		g.markStruct(g.ops[name].input, decoding)
		g.markStruct(g.ops[name].output, encoding)
	}

	for _, ex := range g.exceptions {
		if ex.members != nil {
			g.markStruct(ex.members, encoding)
		}
	}
}

func (g *generator) markStruct(st *structType, dir direction) {
	flag := &st.encodes
	if dir == decoding {
		flag = &st.decodes
	}

	if *flag {
		return
	}

	*flag = true

	for _, f := range st.fields {
		for _, nested := range g.nestedStructs(f.pair.p) {
			g.markStruct(g.structs[g.goTypeName(nested)], dir)
		}
	}
}

// nestedStructs lists the structure shapes directly below a type: the type
// itself, or the element of a list or map.
func (g *generator) nestedStructs(r ref) []string {
	if r.shape.Document {
		return nil
	}

	switch r.shape.Type {
	case spec.TypeStructure:
		return []string{r.name}
	case spec.TypeList:
		elem, _ := resolve(g.p, r.shape.Member)

		return g.nestedStructs(elem)
	case spec.TypeMap:
		elem, _ := resolve(g.p, r.shape.Value)

		return g.nestedStructs(elem)
	default:
		return nil
	}
}

// markChecks flags the structs that hold a required member somewhere below
// them, which are the ones ServeValidated has to look through.
func (g *generator) markChecks() {
	memo := map[string]bool{}

	var needs func(st *structType) bool

	needs = func(st *structType) bool {
		if v, ok := memo[st.goName]; ok {
			return v
		}

		memo[st.goName] = false // a cycle adds nothing by itself

		result := false

		for _, f := range st.fields {
			if f.required {
				result = true
			}

			for _, nested := range g.nestedStructs(f.pair.p) {
				if needs(g.structs[g.goTypeName(nested)]) {
					result = true
				}
			}
		}

		memo[st.goName] = result

		return result
	}

	for _, st := range g.structs {
		st.needsCheck = needs(st)
	}
}

func (g *generator) sortedStructs() []*structType {
	out := make([]*structType, 0, len(g.structs))
	for _, st := range g.structs {
		out = append(out, st)
	}

	slices.SortFunc(out, func(a, b *structType) int { return strings.Compare(a.goName, b.goName) })

	return out
}

// exportName turns a member or shape name into an exported Go identifier.
func exportName(name string) string {
	r, size := utf8.DecodeRuneInString(name)

	return string(unicode.ToUpper(r)) + name[size:]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
