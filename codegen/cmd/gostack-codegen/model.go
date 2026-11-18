package main

import (
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
	loc   string
	flat  bool
	tsfmt string
}

// site says where a value sits. A struct member holds a scalar by pointer, so
// absence shows as nil. A list element or map value holds it by value.
type site int

const (
	siteMember site = iota + 1
	siteElement
)

// direction says whether the query codec decodes a struct or encodes it.
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
	decodeQ    bool
	encodeQ    bool
	needsCheck bool
}

// operation is one operation of the service.
type operation struct {
	name        string
	input       *structType
	output      *structType
	queryResult string
	queryOutput bool
}

// exception is one error shape.
type exception struct {
	shape     string
	code      string
	status    int
	sender    bool
	queryCode string
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
	protoJSON  = "json"
	protoQuery = "query"
)

func (g *generator) hasProtocol(name string) bool {
	return slices.Contains(g.protocols, name)
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

	g := &generator{
		name:      name,
		opts:      opts,
		p:         p,
		ops:       map[string]*operation{},
		structs:   map[string]*structType{},
		rootNames: map[string]string{},
	}

	g.protocols = declaredProtocols(p)

	if opts.QueryModel != "" {
		g.q, err = loader.File(opts.QueryModel)
		if err != nil {
			return nil, fmt.Errorf("load %s query model: %w", name, err)
		}

		if !g.hasProtocol(protoQuery) {
			g.protocols = append(g.protocols, protoQuery)
		}
	} else if g.hasProtocol(protoQuery) {
		g.q = p
	}

	for _, proto := range g.protocols {
		if proto != protoJSON && proto != protoQuery {
			return nil, fmt.Errorf("%s: protocol %q is not supported yet", name, proto)
		}
	}

	err = g.build()
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

	err := g.nameRoots()
	if err != nil {
		return err
	}

	for _, name := range g.opNames {
		err = g.buildOperation(name)
		if err != nil {
			return fmt.Errorf("operation %s: %w", name, err)
		}
	}

	g.markReachable()
	g.markChecks()

	return g.buildExceptions()
}

// nameRoots gives the input and output shape of each operation the Go name
// <Operation>Input and <Operation>Output.
func (g *generator) nameRoots() error {
	for _, name := range g.opNames {
		op := g.p.Operations[name]

		for _, root := range []struct {
			ref    *spec.Ref
			suffix string
		}{{op.Input, "Input"}, {op.Output, "Output"}} {
			if root.ref == nil {
				continue
			}

			if prev, dup := g.rootNames[root.ref.Shape]; dup {
				return fmt.Errorf("shape %s is the input or output of both %s and %s", root.ref.Shape, prev, name+root.suffix)
			}

			g.rootNames[root.ref.Shape] = name + root.suffix
		}
	}

	return nil
}

func (g *generator) buildOperation(name string) error {
	op := g.p.Operations[name]
	out := &operation{name: name}
	g.ops[name] = out

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

	return nil
}

// rootStruct builds the struct of an operation's input or output. A missing
// shape gives an empty struct, so every operation has both.
func (g *generator) rootStruct(goName string, pr, qr *spec.Ref) (*structType, error) {
	if pr == nil {
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
	if st, ok := g.structs[goName]; ok {
		return st, nil
	}

	shape := g.p.Shapes[shapeName]
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

		err = g.buildNested(f.pair.p)
		if err != nil {
			return nil, err
		}
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

	err = g.checkJSONTimestamp(r)
	if err != nil {
		return nil, err
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

	return exportName(shape)
}

// goType is the Go type of a shape reference at the given site.
func (g *generator) goType(r ref, at site) (string, error) {
	scalar, ok := scalarTypes[r.shape.Type]
	if ok {
		if at == siteMember && scalar != "[]byte" && scalar != "*protocol.Time" {
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

	out := ref{name: r.Shape, shape: shape, loc: shape.LocationName, flat: shape.Flattened, tsfmt: shape.TimestampFormat}
	if r.LocationName != "" {
		out.loc = r.LocationName
	}

	if r.Flattened {
		out.flat = true
	}

	if r.TimestampFormat != "" {
		out.tsfmt = r.TimestampFormat
	}

	return out, nil
}

// markReachable flags the structs the query codec has to decode, those below an
// operation input, and to encode, those below an output.
func (g *generator) markReachable() {
	if g.q == nil {
		return
	}

	for _, name := range g.opNames {
		g.markStruct(g.ops[name].input, decoding)
		g.markStruct(g.ops[name].output, encoding)
	}
}

func (g *generator) markStruct(st *structType, dir direction) {
	flag := &st.encodeQ
	if dir == decoding {
		flag = &st.decodeQ
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
