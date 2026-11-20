package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// queryEmitter writes the query codec of one service. The package-level
// ListSpec and MapSpec variables the decode methods refer to collect in specs.
type queryEmitter struct {
	g     *generator
	p     *printer
	specs *printer
	seq   int
}

// queryFile writes the query codec: decode methods for the structs below an
// input, encode methods for the structs below an output, and a decode function
// for each operation.
func (g *generator) queryFile() (string, error) {
	if !g.hasProtocol(protoQuery) {
		return "", nil
	}

	if looped := g.queryDecodeCycles(); len(looped) > 0 {
		return "", fmt.Errorf("the query decoder has no depth bound, and %s holds itself below",
			strings.Join(looped, ", "))
	}

	e := &queryEmitter{g: g, p: &printer{}, specs: &printer{}}

	e.p.f("const (")
	e.p.f("\t// queryNamespace is the xmlns of query responses.")
	e.p.f("\tqueryNamespace = %q", g.q.Metadata.XMLNamespace)
	e.p.f("\t// queryEscapeQuotes writes a double quote in response text as &quot;.")
	e.p.f("\tqueryEscapeQuotes = %t", g.opts.EscapeQuotes)
	e.p.f(")")
	e.p.blank()

	for _, name := range g.opNames {
		e.emitOperationDecode(g.ops[name])
	}

	for _, st := range g.sortedStructs() {
		err := e.emitStruct(st)
		if err != nil {
			return "", fmt.Errorf("%s: %w", st.goName, err)
		}
	}

	return e.p.buf.String() + e.specs.buf.String(), nil
}

// queryDecodeCycles names, sorted, the structs the query codec decodes that
// hold themselves somewhere below. Generated query decoding recurses once per
// structure level and builds each level's key from its parent's, so through
// such a struct the length of a request key sets the depth, and the time and
// memory grow with its square: 100,000 levels took 96 s and allocated 290 GB.
// No query service on the request path has one (SQS, STS and SNS); a service
// that does needs a depth bound in the decoder before it generates.
func (g *generator) queryDecodeCycles() []string {
	var looped []string

	for _, st := range g.sortedStructs() {
		if st.decodes && g.reachesItself(st) {
			looped = append(looped, st.goName)
		}
	}

	return looped
}

// reachesItself reports whether start holds a struct of its own type at some
// depth below it, through members, list elements and map values.
func (g *generator) reachesItself(start *structType) bool {
	seen := map[string]bool{}
	stack := []*structType{start}

	for len(stack) > 0 {
		st := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for _, f := range st.fields {
			for _, shape := range g.nestedStructs(f.pair.p) {
				name := g.goTypeName(shape)
				if name == start.goName {
					return true
				}

				if next, ok := g.structs[name]; ok && !seen[name] {
					seen[name] = true
					stack = append(stack, next)
				}
			}
		}
	}

	return false
}

func (e *queryEmitter) emitOperationDecode(op *operation) {
	p := e.p
	p.f("// decode%sQuery reads the form values of a %s request.", op.name, op.name)
	p.f("func decode%sQuery(vals url.Values) (*%s, *awserr.Error) {", op.name, op.input.goName)
	p.f("\td := query.NewDecoder(vals)")
	p.f("\tin := &%s{}", op.input.goName)
	p.f("\tin.decodeQuery(d, \"\")")
	p.f("\tif err := d.Err(); err != nil {")
	p.f("\t\treturn nil, err")
	p.f("\t}")
	p.f("\treturn in, nil")
	p.f("}")
	p.blank()
}

func (e *queryEmitter) emitStruct(st *structType) error {
	if (st.decodes || st.encodes) && st.queryShape == nil && len(st.fields) > 0 {
		return errors.New("no query shape for a structure the query codec needs")
	}

	if st.decodes {
		e.p.f("func (v *%s) decodeQuery(d *query.Decoder, p string) {", st.goName)

		for _, f := range st.queryOrder {
			err := e.decodeMember(f)
			if err != nil {
				return fmt.Errorf("decode %s: %w", f.member, err)
			}
		}

		e.p.f("}")
		e.p.blank()
	}

	if st.encodes {
		e.p.f("func (v *%s) encodeQuery(w *query.Writer) {", st.goName)

		for _, f := range st.queryOrder {
			err := e.encodeMember(f)
			if err != nil {
				return fmt.Errorf("encode %s: %w", f.member, err)
			}
		}

		e.p.f("}")
		e.p.blank()
	}

	return nil
}

// scalarKind names the Decoder method, and the Writer method, for a scalar
// shape type.
var scalarDecoders = map[string]string{
	spec.TypeString:  "String",
	spec.TypeInteger: "Int32",
	spec.TypeLong:    "Int64",
	spec.TypeFloat:   "Float64",
	spec.TypeDouble:  "Float64",
	spec.TypeBoolean: "Bool",
	spec.TypeBlob:    "Blob",
}

func isComplex(typ string) bool {
	return typ == spec.TypeStructure || typ == spec.TypeList || typ == spec.TypeMap
}

// checkKinds fails when the two models disagree about the kind of a member.
func checkKinds(pr pair) error {
	if pr.p.shape.Type != pr.q.shape.Type {
		return fmt.Errorf("the models disagree: %s is a %s, and %s is a %s",
			pr.p.name, pr.p.shape.Type, pr.q.name, pr.q.shape.Type)
	}

	return nil
}

// decodeMember writes the statements that read one member of a struct.
func (e *queryEmitter) decodeMember(f *field) error {
	err := checkKinds(f.pair)
	if err != nil {
		return err
	}

	pr := f.pair
	dst := "v." + f.goName
	name := serialName(pr.q.loc, f.queryName)

	switch pr.p.shape.Type {
	case spec.TypeStructure:
		e.p.f("\tif k := query.Key(p, %q); d.Has(k) {", name)
		e.p.f("\t\tn := &%s{}", e.g.goTypeName(pr.p.name))
		e.p.f("\t\tn.decodeQuery(d, k)")
		e.p.f("\t\t%s = n", dst)
		e.p.f("\t}")

		return nil
	case spec.TypeList:
		return e.decodeList(pr, "p", e.listNames(pr, f.queryName), dst, 1)
	case spec.TypeMap:
		return e.decodeMap(pr, "p", e.mapNames(pr, f.queryName), dst, 1)
	default:
		call, err := scalarRead(pr, fmt.Sprintf("query.Key(p, %q)", name))
		if err != nil {
			return err
		}

		e.p.f("\t%s = %s", dst, call)

		return nil
	}
}

// serialName is the wire name of a member: its locationName, or its name.
func serialName(loc, member string) string {
	if loc != "" {
		return loc
	}

	return member
}

// scalarRead is the Decoder call that reads a scalar by pointer.
func scalarRead(pr pair, key string) (string, error) {
	if pr.p.shape.Type == spec.TypeTimestamp {
		return fmt.Sprintf("d.Time(%s, %q)", key, pr.q.tsfmt), nil
	}

	method, ok := scalarDecoders[pr.p.shape.Type]
	if !ok {
		return "", fmt.Errorf("unsupported scalar type %q", pr.p.shape.Type)
	}

	return fmt.Sprintf("d.%s(%s)", method, key), nil
}

// listNames lists the names a list member may carry in a request: the one
// the spec gives, then, for a service that accepts them, the member's own name.
func (e *queryEmitter) listNames(pr pair, member string) []string {
	primary := serialName(pr.q.loc, member)

	if pr.q.flat {
		elem, err := resolve(e.g.q, pr.q.shape.Member)
		if err == nil {
			primary = serialName(elem.loc, member)
		}
	}

	return e.withAlias(primary, member)
}

func (e *queryEmitter) mapNames(pr pair, member string) []string {
	return e.withAlias(serialName(pr.q.loc, member), member)
}

func (e *queryEmitter) withAlias(primary, alias string) []string {
	if e.g.opts.QueryAliases && primary != alias {
		return []string{primary, alias}
	}

	return []string{primary}
}

func goStrings(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}

	return "[]string{" + strings.Join(quoted, ", ") + "}"
}

// decodeList writes the loop that reads a list into dst. prefix is the Go
// expression of the key the list's name hangs under.
func (e *queryEmitter) decodeList(pr pair, prefix string, names []string, dst string, depth int) error {
	pelem, err := resolve(e.g.p, pr.p.shape.Member)
	if err != nil {
		return err
	}

	qelem, err := resolve(e.g.q, pr.q.shape.Member)
	if err != nil {
		return err
	}

	elem := pair{p: pelem, q: qelem}

	err = checkKinds(elem)
	if err != nil {
		return err
	}

	typ, err := e.g.goType(pelem, siteElement)
	if err != nil {
		return err
	}

	e.seq++

	specVar := fmt.Sprintf("qList%d", e.seq)
	e.specs.f("var %s = query.ListSpec{Names: %s, Member: %q, Flattened: %t, Complex: %t}",
		specVar, goStrings(names), qelem.loc, pr.q.flat, isComplex(pelem.shape.Type))

	k, v := fmt.Sprintf("k%d", depth), fmt.Sprintf("e%d", depth)

	e.p.f("\tfor _, %s := range d.List(%s, &%s) {", k, prefix, specVar)
	e.p.f("\t\tvar %s %s", v, typ)

	err = e.decodeElem(elem, k, v, depth+1)
	if err != nil {
		return err
	}

	e.p.f("\t\t%s = append(%s, %s)", dst, dst, v)
	e.p.f("\t}")

	return nil
}

// decodeMap writes the loop that reads a map into dst.
func (e *queryEmitter) decodeMap(pr pair, prefix string, names []string, dst string, depth int) error {
	pval, err := resolve(e.g.p, pr.p.shape.Value)
	if err != nil {
		return err
	}

	qkey, err := resolve(e.g.q, pr.q.shape.Key)
	if err != nil {
		return err
	}

	qval, err := resolve(e.g.q, pr.q.shape.Value)
	if err != nil {
		return err
	}

	elem := pair{p: pval, q: qval}

	err = checkKinds(elem)
	if err != nil {
		return err
	}

	typ, err := e.g.goType(pval, siteElement)
	if err != nil {
		return err
	}

	e.seq++

	specVar := fmt.Sprintf("qMap%d", e.seq)
	e.specs.f("var %s = query.MapSpec{Names: %s, Key: %q, Value: %q, Flattened: %t, ComplexValue: %t}",
		specVar, goStrings(names), qkey.loc, qval.loc, pr.q.flat, isComplex(pval.shape.Type))

	ent, k, v := fmt.Sprintf("ent%d", depth), fmt.Sprintf("k%d", depth), fmt.Sprintf("e%d", depth)

	e.p.f("\tfor _, %s := range d.Map(%s, &%s) {", ent, prefix, specVar)
	e.p.f("\t\t%s := query.Value(d.String(%s.Key))", k, ent)
	e.p.f("\t\tvar %s %s", v, typ)

	err = e.decodeElem(elem, ent+".Value", v, depth+1)
	if err != nil {
		return err
	}

	e.p.f("\t\tif %s == nil {", dst)
	e.p.f("\t\t\t%s = map[string]%s{}", dst, typ)
	e.p.f("\t\t}")
	e.p.f("\t\t%s[%s] = %s", dst, k, v)
	e.p.f("\t}")

	return nil
}

// decodeElem writes the statements that read a list element or map value, whose
// key is the Go expression key, into the variable dst.
func (e *queryEmitter) decodeElem(pr pair, key, dst string, depth int) error {
	switch pr.p.shape.Type {
	case spec.TypeStructure:
		e.p.f("\t\t%s.decodeQuery(d, %s)", dst, key)

		return nil
	case spec.TypeList:
		return e.decodeList(pr, key, []string{""}, dst, depth)
	case spec.TypeMap:
		return e.decodeMap(pr, key, []string{""}, dst, depth)
	default:
		call, err := scalarRead(pr, key)
		if err != nil {
			return err
		}

		if pr.p.shape.Type == spec.TypeBlob || pr.p.shape.Type == spec.TypeTimestamp {
			e.p.f("\t\t%s = %s", dst, call)
		} else {
			e.p.f("\t\t%s = query.Value(%s)", dst, call)
		}

		return nil
	}
}

// encodeMember writes the statements that write one member of a struct.
func (e *queryEmitter) encodeMember(f *field) error {
	err := checkKinds(f.pair)
	if err != nil {
		return err
	}

	return e.encodeValue(f.pair, "v."+f.goName, serialName(f.pair.q.loc, f.queryName), siteMember, 1)
}

// encodeValue writes the statements that write the value in the Go expression
// x as an element named name. at says whether x is a struct member, where a
// scalar is held by pointer, or a list element or map value.
func (e *queryEmitter) encodeValue(pr pair, x, name string, at site, depth int) error {
	switch pr.p.shape.Type {
	case spec.TypeStructure:
		e.encodeStruct(x, name, at)

		return nil
	case spec.TypeList:
		return e.encodeList(pr, x, name, depth)
	case spec.TypeMap:
		return e.encodeMap(pr, x, name, depth)
	default:
		return e.encodeScalar(pr, x, name, at)
	}
}

func (e *queryEmitter) encodeStruct(x, name string, at site) {
	if at == siteMember {
		e.p.f("\tif %s != nil {", x)
		e.p.f("\t\tw.Open(%q)", name)
		e.p.f("\t\t%s.encodeQuery(w)", x)
		e.p.f("\t\tw.Close(%q)", name)
		e.p.f("\t}")

		return
	}

	e.p.f("\tw.Open(%q)", name)
	e.p.f("\t%s.encodeQuery(w)", x)
	e.p.f("\tw.Close(%q)", name)
}

func (e *queryEmitter) encodeScalar(pr pair, x, name string, at site) error {
	deref, nilCheck := x, ""

	switch pr.p.shape.Type {
	case spec.TypeBlob, spec.TypeTimestamp:
		nilCheck = x + " != nil"
	default:
		if at == siteMember {
			deref, nilCheck = "*"+x, x+" != nil"
		}
	}

	var call string

	switch pr.p.shape.Type {
	case spec.TypeString:
		call = fmt.Sprintf("w.String(%q, %s)", name, deref)
	case spec.TypeInteger, spec.TypeLong:
		call = fmt.Sprintf("w.Int(%q, int64(%s))", name, deref)
	case spec.TypeFloat, spec.TypeDouble:
		call = fmt.Sprintf("w.Float(%q, %s)", name, deref)
	case spec.TypeBoolean:
		call = fmt.Sprintf("w.Bool(%q, %s)", name, deref)
	case spec.TypeBlob:
		call = fmt.Sprintf("w.Blob(%q, %s)", name, deref)
	case spec.TypeTimestamp:
		call = fmt.Sprintf("w.Time(%q, %s, %q)", name, deref, pr.q.tsfmt)
	default:
		return fmt.Errorf("unsupported scalar type %q", pr.p.shape.Type)
	}

	if nilCheck == "" {
		e.p.f("\t%s", call)
	} else {
		e.p.f("\tif %s {", nilCheck)
		e.p.f("\t\t%s", call)
		e.p.f("\t}")
	}

	return nil
}

func (e *queryEmitter) encodeList(pr pair, x, name string, depth int) error {
	pelem, err := resolve(e.g.p, pr.p.shape.Member)
	if err != nil {
		return err
	}

	qelem, err := resolve(e.g.q, pr.q.shape.Member)
	if err != nil {
		return err
	}

	elem := pair{p: pelem, q: qelem}

	err = checkKinds(elem)
	if err != nil {
		return err
	}

	v := fmt.Sprintf("e%d", depth)

	e.p.f("\tif %s != nil {", x)

	if pr.q.flat {
		e.p.f("\t\tfor _, %s := range %s {", v, x)

		err = e.encodeValue(elem, v, serialName(qelem.loc, name), siteElement, depth+1)
		if err != nil {
			return err
		}

		e.p.f("\t\t}")
	} else {
		e.p.f("\t\tw.Open(%q)", name)
		e.p.f("\t\tfor _, %s := range %s {", v, x)

		err = e.encodeValue(elem, v, serialName(qelem.loc, "member"), siteElement, depth+1)
		if err != nil {
			return err
		}

		e.p.f("\t\t}")
		e.p.f("\t\tw.Close(%q)", name)
	}

	e.p.f("\t}")

	return nil
}

func (e *queryEmitter) encodeMap(pr pair, x, name string, depth int) error {
	pval, err := resolve(e.g.p, pr.p.shape.Value)
	if err != nil {
		return err
	}

	qkey, err := resolve(e.g.q, pr.q.shape.Key)
	if err != nil {
		return err
	}

	qval, err := resolve(e.g.q, pr.q.shape.Value)
	if err != nil {
		return err
	}

	elem := pair{p: pval, q: qval}

	err = checkKinds(elem)
	if err != nil {
		return err
	}

	k, v := fmt.Sprintf("k%d", depth), fmt.Sprintf("e%d", depth)
	entry := "entry"

	e.p.f("\tif %s != nil {", x)

	if pr.q.flat {
		entry = name
	} else {
		e.p.f("\t\tw.Open(%q)", name)
	}

	e.p.f("\t\tfor _, %s := range slices.Sorted(maps.Keys(%s)) {", k, x)
	e.p.f("\t\t\t%s := %s[%s]", v, x, k)
	e.p.f("\t\t\tw.Open(%q)", entry)
	e.p.f("\t\t\tw.String(%q, %s)", serialName(qkey.loc, "key"), k)

	err = e.encodeValue(elem, v, serialName(qval.loc, "value"), siteElement, depth+1)
	if err != nil {
		return err
	}

	e.p.f("\t\t\tw.Close(%q)", entry)
	e.p.f("\t\t}")

	if !pr.q.flat {
		e.p.f("\t\tw.Close(%q)", name)
	}

	e.p.f("\t}")

	return nil
}
