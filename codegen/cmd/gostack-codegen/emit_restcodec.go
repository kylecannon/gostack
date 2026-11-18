package main

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/kylecannon/gostack/internal/protocol/rest"
)

// restCodecFile writes the methods that carry each struct's members between
// Go and the wire: decodeBindings and encodeBindings for the members bound
// outside the body, the xml codec of a rest-xml body, the timestamp
// marshalers of a rest-json body, and the writer of each error with members.
func (g *generator) restCodecFile() (string, error) {
	p := &printer{}
	inputs, outputs := map[*structType]bool{}, map[*structType]bool{}

	for _, name := range g.opNames {
		inputs[g.ops[name].input] = true
		outputs[g.ops[name].output] = true
	}

	for _, ex := range g.exceptions {
		if ex.members != nil {
			outputs[ex.members] = true
		}
	}

	x := &xmlEmitter{g: g, p: p}

	for _, st := range g.sortedStructs() {
		var err error
		if inputs[st] {
			err = g.emitDecodeBindings(p, st)
		}

		if err == nil && outputs[st] {
			err = g.emitEncodeBindings(p, st)
		}

		if err != nil {
			return "", fmt.Errorf("%s: %w", st.goName, err)
		}

		if g.hasProtocol(protoRestJSON) {
			emitTimeMarshalers(p, st)

			continue
		}

		if st.decodes {
			err = x.decodeStruct(st)
		}

		if err == nil && st.encodes {
			err = x.encodeStruct(st)
		}

		if err != nil {
			return "", fmt.Errorf("%s: %w", st.goName, err)
		}
	}

	for _, ex := range g.exceptions {
		if ex.members != nil {
			g.emitErrorWriter(p, ex.members)
		}
	}

	return p.buf.String(), nil
}

func (g *generator) emitDecodeBindings(p *printer, st *structType) error {
	if hasBinding(st, spec.LocationURI, spec.LocationQuery, spec.LocationHeader, spec.LocationHeaders) {
		p.f("// decodeBindings reads the members of v bound to the uri, the query string and headers.")
		p.f("func (v *%s) decodeBindings(d *rest.Decoder) {", st.goName)

		for _, f := range st.fields {
			read, err := g.bindingRead(f)
			if err != nil {
				return fmt.Errorf("%s: %w", f.member, err)
			}

			if read != "" {
				p.f("\tv.%s = %s", f.goName, read)
			}
		}

		p.f("}")
		p.blank()
	}

	return nil
}

func (g *generator) emitEncodeBindings(p *printer, st *structType) error {
	if hasBinding(st, spec.LocationHeader, spec.LocationHeaders) {
		p.f("// encodeBindings sets the members of v bound to headers.")
		p.f("func (v *%s) encodeBindings(h http.Header) {", st.goName)

		for _, f := range st.fields {
			set, err := g.bindingWrite(f)
			if err != nil {
				return fmt.Errorf("%s: %w", f.member, err)
			}

			if set != "" {
				p.f("\t%s", set)
			}
		}

		p.f("}")
		p.blank()
	}

	return nil
}

// scalarSuffixes name the typed getter and setter of package rest for a
// scalar shape type, and listSuffixes the ones for a list of it.
var (
	scalarSuffixes = map[string]string{
		spec.TypeString: "", spec.TypeInteger: "Int32", spec.TypeLong: "Int64", spec.TypeFloat: suffixFloat,
		spec.TypeDouble: suffixFloat, spec.TypeBoolean: "Bool", spec.TypeTimestamp: "Time", spec.TypeBlob: suffixBlob,
	}
	listSuffixes = map[string]string{
		spec.TypeString: "Strings", spec.TypeInteger: "Int32s", spec.TypeLong: "Int64s", spec.TypeFloat: "Float64s",
		spec.TypeDouble: "Float64s", spec.TypeBoolean: "Bools", spec.TypeTimestamp: "Times",
	}
)

// The suffixes of the rest getters and setters of a float and a blob.
const (
	suffixFloat = "Float64"
	suffixBlob  = "Blob"
)

// elemDepth is the depth of the variables of a member's first element.
const elemDepth = 2

// timeArg is the format argument a timestamp getter or setter takes.
func timeArg(r ref) string {
	if r.shape.Type != spec.TypeTimestamp {
		return ""
	}

	return fmt.Sprintf(", %q", r.tsfmt)
}

// bindingRead is the expression that reads a member bound outside the body
// from the rest.Decoder d, or "" for a body member.
func (g *generator) bindingRead(f *field) (string, error) {
	r, name := f.pair.p, wireName(f)

	switch r.binding {
	case spec.LocationURI:
		suffix, ok := scalarSuffixes[r.shape.Type]
		if !ok || suffix == suffixBlob {
			return "", fmt.Errorf("a uri label of type %s is not supported", r.shape.Type)
		}

		return fmt.Sprintf("d.Label%s(%q%s)", suffix, name, timeArg(r)), nil
	case spec.LocationQuery:
		return g.queryRead(r, name)
	case spec.LocationHeader:
		return g.headerCall("d.Header", r, name, "")
	case spec.LocationHeaders:
		// The prefix is the locationName alone: none matches every header.
		return fmt.Sprintf("d.HeaderMap(%q)", r.loc), g.checkStringMap(r)
	default:
		return "", nil
	}
}

func (g *generator) queryRead(r ref, name string) (string, error) {
	switch r.shape.Type {
	case spec.TypeList:
		elem, err := resolve(g.p, r.shape.Member)
		if err != nil {
			return "", err
		}

		suffix, ok := listSuffixes[elem.shape.Type]
		if !ok {
			return "", fmt.Errorf("a query list of %s is not supported", elem.shape.Type)
		}

		return fmt.Sprintf("d.Query%s(%q%s)", suffix, name, timeArg(elem)), nil
	case spec.TypeMap:
		val, err := resolve(g.p, r.shape.Value)
		if err != nil {
			return "", err
		}

		if val.shape.Type == spec.TypeString {
			return "d.QueryMap()", nil
		}

		if val.shape.Type == spec.TypeList {
			inner, err := resolve(g.p, val.shape.Member)
			if err == nil && inner.shape.Type == spec.TypeString {
				return "d.QueryMapStrings()", nil
			}
		}

		return "", fmt.Errorf("a query map of %s is not supported", val.shape.Type)
	default:
		suffix, ok := scalarSuffixes[r.shape.Type]
		if !ok || suffix == suffixBlob {
			return "", fmt.Errorf("a query parameter of type %s is not supported", r.shape.Type)
		}

		return fmt.Sprintf("d.Query%s(%q%s)", suffix, name, timeArg(r)), nil
	}
}

// headerCall is the call of the getter (prefix "d.Header") or the setter
// (prefix "rest.Set") of a header member; value is the setter's argument.
func (g *generator) headerCall(prefix string, r ref, name, value string) (string, error) {
	args := fmt.Sprintf("%q", name)
	if value != "" {
		args = "h, " + args + ", " + value
	}

	if r.jsonvalue {
		return fmt.Sprintf("%sJSONValue(%s)", prefix, args), nil
	}

	if r.shape.Type == spec.TypeList {
		elem, err := resolve(g.p, r.shape.Member)
		if err != nil {
			return "", err
		}

		suffix, ok := listSuffixes[elem.shape.Type]
		if !ok {
			return "", fmt.Errorf("a header list of %s is not supported", elem.shape.Type)
		}

		return fmt.Sprintf("%s%s(%s%s)", prefix, suffix, args, g.headerTimeArg(prefix, elem)), nil
	}

	suffix, ok := scalarSuffixes[r.shape.Type]
	if !ok {
		return "", fmt.Errorf("a header of type %s is not supported", r.shape.Type)
	}

	if prefix == "rest.Set" && suffix == "" {
		suffix = "String"
	}

	return fmt.Sprintf("%s%s(%s%s)", prefix, suffix, args, g.headerTimeArg(prefix, r)), nil
}

// headerTimeArg is timeArg for a header. A header the service writes in the
// explicit iso8601 form carries the service's BodyTime too, as S3's
// x-amz-object-lock-retain-until-date does (serializer _timestamp_iso8601).
// The default of a header, rfc822, stays, and so does reading.
func (g *generator) headerTimeArg(prefix string, r ref) string {
	if prefix == "rest.Set" && g.opts.BodyTime != "" && r.shape.Type == spec.TypeTimestamp &&
		strings.EqualFold(r.tsfmt, rest.TimeISO8601) {
		return fmt.Sprintf(", %q", g.opts.BodyTime)
	}

	return timeArg(r)
}

func (g *generator) checkStringMap(r ref) error {
	val, err := resolve(g.p, r.shape.Value)
	if err != nil {
		return err
	}

	if r.shape.Type != spec.TypeMap || val.shape.Type != spec.TypeString {
		return fmt.Errorf("a headers member of type %s is not a map of strings", r.shape.Type)
	}

	return nil
}

// bindingWrite is the statement that sets a member bound to a header on h, or
// "" for another member.
func (g *generator) bindingWrite(f *field) (string, error) {
	r, name := f.pair.p, wireName(f)

	switch r.binding {
	case spec.LocationHeader:
		return g.headerCall("rest.Set", r, name, "v."+f.goName)
	case spec.LocationHeaders:
		return fmt.Sprintf("rest.SetMap(h, %q, v.%s)", r.loc, f.goName), g.checkStringMap(r)
	default:
		return "", nil
	}
}

// emitErrorWriter writes the method that writes an error with the members of
// its shape.
func (g *generator) emitErrorWriter(p *printer, st *structType) {
	extra := goNil
	if g.hasBody(st) {
		extra = "v.encodeXML"
		if g.hasProtocol(protoRestJSON) {
			extra = "v"
		}
	}

	p.f("// writeError writes err with the members in v.")
	p.f("func (v *%s) writeError(w http.ResponseWriter, rc *request.Context, err *awserr.Error) {", st.goName)

	if hasBinding(st, spec.LocationHeader, spec.LocationHeaders) {
		p.f("\tv.encodeBindings(w.Header())")
	}

	p.f("\t%s", g.errorWriter(extra))
	p.f("}")
	p.blank()
}

// xmlEmitter writes the decodeXML and encodeXML methods of the structs of a
// rest-xml service. Both read or write what is inside the struct's element:
// its attributes and its child elements. The caller enters or opens the
// element itself, as only it knows the element's name.
type xmlEmitter struct {
	g *generator
	p *printer
}

func (x *xmlEmitter) decodeStruct(st *structType) error {
	p := x.p

	p.f("// decodeXML reads the attributes and child elements of the element d entered last.")
	p.f("func (v *%s) decodeXML(d *restxml.Decoder) {", st.goName)

	for _, f := range st.fields {
		if !f.pair.p.xmlAttr || f.pair.p.binding != "" {
			continue
		}

		if f.typ != typeStringPtr {
			return fmt.Errorf("the xml attribute %s of type %s is not supported", f.member, f.typ)
		}

		p.f("\tif s, ok := d.Attr(%q); ok {", wireName(f))
		p.f("\t\tv.%s = &s", f.goName)
		p.f("\t}")
	}

	p.f("\tfor {")
	p.f("\t\tname, ok := d.Next()")
	p.f("\t\tif !ok {")
	p.f("\t\t\treturn")
	p.f("\t\t}")
	p.f("\t\tswitch name {")

	seen := map[string]bool{}

	for _, f := range st.fields {
		if !inXMLBody(st, f, decoding) {
			continue
		}

		err := x.decodeMember(f, seen)
		if err != nil {
			return fmt.Errorf("%s: %w", f.member, err)
		}
	}

	p.f("\t\tdefault:")
	p.f("\t\t\td.Skip()")
	p.f("\t\t}")
	p.f("\t}")
	p.f("}")
	p.blank()

	return nil
}

// xmlScalarRead is the restxml.Decoder call that reads a scalar element.
func xmlScalarRead(r ref) (string, bool) {
	switch r.shape.Type {
	case spec.TypeString:
		return "d.String()", true
	case spec.TypeInteger:
		return "d.Int32()", true
	case spec.TypeLong:
		return "d.Int64()", true
	case spec.TypeFloat, spec.TypeDouble:
		return "d.Float64()", true
	case spec.TypeBoolean:
		return "d.Bool()", true
	case spec.TypeTimestamp:
		return fmt.Sprintf("d.Time(%q)", r.tsfmt), true
	case spec.TypeBlob:
		return "d.Blob()", true
	default:
		return "", false
	}
}

func (x *xmlEmitter) decodeMember(f *field, seen map[string]bool) error {
	p, r, target := x.p, f.pair.p, "v."+f.goName
	if r.shape.Document {
		return errors.New("a document in a rest-xml body is not supported")
	}

	elem, labels, err := x.caseLabels(f, seen)
	if err != nil || len(labels) == 0 {
		return err
	}

	p.f("\t\tcase %s:", strings.Join(labels, ", "))

	if read, ok := xmlScalarRead(r); ok {
		p.f("\t\t\t%s = %s", target, read)

		return nil
	}

	switch {
	case r.shape.Type == spec.TypeStructure:
		p.f("\t\t\t%s = &%s{}", target, x.g.goTypeName(r.name))
		p.f("\t\t\t%s.decodeXML(d)", target)

		return nil
	case r.flat && r.shape.Type == spec.TypeList:
		var et string

		et, err = x.g.goType(elem, siteElement)
		if err != nil {
			return err
		}

		p.f("\t\t\tvar e1 %s", et)

		err = x.decodeInto(elem, "e1", elemDepth)
		p.f("\t\t\t%s = append(%s, e1)", target, target)

		return err
	case r.flat && r.shape.Type == spec.TypeMap:
		p.f("\t\t\tif %s == nil {", target)
		p.f("\t\t\t\t%s = %s{}", target, f.typ)
		p.f("\t\t\t}")

		return x.entry(r, target, 1)
	default:
		if r.shape.Type == spec.TypeList {
			p.f("\t\t\tif %s == nil {", target)
			p.f("\t\t\t\t%s = %s{}", target, f.typ)
			p.f("\t\t\t}")
		}

		return x.decodeInto(r, target, 1)
	}
}

// caseLabels returns the element names a member is read under, quoted, that
// no member before it in its struct took, and for a flattened list the
// reference of its elements.
func (x *xmlEmitter) caseLabels(f *field, seen map[string]bool) (ref, []string, error) {
	r := f.pair.p
	names := []string{wireName(f)}

	var elem ref

	if r.flat && r.shape.Type == spec.TypeList {
		var err error

		elem, err = resolve(x.g.p, r.shape.Member)
		if err != nil {
			return ref{}, nil, err
		}

		// botocore writes a flattened list under the member's name and reads
		// it under its element's, so both are read.
		if elem.loc != "" {
			names = append(names, elem.loc)
		}
	}

	// S3's deprecated Event and its Events list share the element name; the
	// member listed first takes it.
	labels := make([]string, 0, len(names))

	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			labels = append(labels, fmt.Sprintf("%q", name))
		}
	}

	return elem, labels, nil
}

// decodeInto writes the code that reads the element entered last into target,
// a variable of r's element type. A list or map adds to what target holds.
func (x *xmlEmitter) decodeInto(r ref, target string, depth int) error {
	p := x.p

	if r.shape.Document {
		return errors.New("a document in a rest-xml body is not supported")
	}

	switch r.shape.Type {
	case spec.TypeStructure:
		p.f("%s.decodeXML(d)", target)
	case spec.TypeList:
		return x.decodeList(r, target, depth)
	case spec.TypeMap:
		mt, err := x.g.goType(r, siteElement)
		if err != nil {
			return err
		}

		p.f("if %s == nil {", target)
		p.f("%s = %s{}", target, mt)
		p.f("}")
		p.f("for {")
		p.f("if _, ok := d.Next(); !ok {")
		p.f("break")
		p.f("}")

		err = x.entry(r, target, depth)
		if err != nil {
			return err
		}

		p.f("}")
	case spec.TypeTimestamp, spec.TypeBlob:
		read, _ := xmlScalarRead(r)
		p.f("%s = %s", target, read)
	default:
		read, ok := xmlScalarRead(r)
		if !ok {
			return fmt.Errorf("shape %s of type %s is not supported", r.name, r.shape.Type)
		}

		p.f("if x := %s; x != nil {", read)
		p.f("%s = *x", target)
		p.f("}")
	}

	return nil
}

// decodeList writes the loop that reads each child of the element entered
// last as an element of the list target.
func (x *xmlEmitter) decodeList(r ref, target string, depth int) error {
	p := x.p

	elem, err := resolve(x.g.p, r.shape.Member)
	if err != nil {
		return err
	}

	et, err := x.g.goType(elem, siteElement)
	if err != nil {
		return err
	}

	p.f("for {")
	p.f("if _, ok := d.Next(); !ok {")
	p.f("break")
	p.f("}")
	p.f("var e%d %s", depth, et)

	err = x.decodeInto(elem, fmt.Sprintf("e%d", depth), depth+1)
	if err != nil {
		return err
	}

	p.f("%s = append(%s, e%d)", target, target, depth)
	p.f("}")

	return nil
}

// entry writes the code that reads one map entry, the element entered last,
// into the map target.
func (x *xmlEmitter) entry(r ref, target string, depth int) error {
	p := x.p

	key, err := resolve(x.g.p, r.shape.Key)
	if err != nil {
		return err
	}

	val, err := resolve(x.g.p, r.shape.Value)
	if err != nil {
		return err
	}

	vt, err := x.g.goType(val, siteElement)
	if err != nil {
		return err
	}

	p.f("var k%d *string", depth)
	p.f("var e%d %s", depth, vt)
	p.f("for {")
	p.f("n%d, ok := d.Next()", depth)
	p.f("if !ok {")
	p.f("break")
	p.f("}")
	p.f("switch n%d {", depth)
	p.f("case %q:", cmp.Or(key.loc, "key"))
	p.f("k%d = d.String()", depth)
	p.f("case %q:", cmp.Or(val.loc, "value"))

	err = x.decodeInto(val, fmt.Sprintf("e%d", depth), depth+1)
	if err != nil {
		return err
	}

	p.f("default:")
	p.f("d.Skip()")
	p.f("}")
	p.f("}")
	p.f("if k%d != nil {", depth)
	p.f("%s[*k%d] = e%d", target, depth, depth)
	p.f("}")

	return nil
}

func (x *xmlEmitter) encodeStruct(st *structType) error {
	p := x.p

	p.f("// encodeXML writes the attributes and child elements of v into the element")
	p.f("// xw opened last.")
	p.f("func (v *%s) encodeXML(xw *restxml.Writer) {", st.goName)

	for _, f := range st.fields {
		if !f.pair.p.xmlAttr || f.pair.p.binding != "" {
			continue
		}

		if f.typ != typeStringPtr {
			return fmt.Errorf("the xml attribute %s of type %s is not supported", f.member, f.typ)
		}

		p.f("\tif v.%s != nil {", f.goName)
		p.f("\t\txw.Attr(%q, *v.%s)", wireName(f), f.goName)
		p.f("\t}")
	}

	for _, f := range st.fields {
		if !inXMLBody(st, f, encoding) {
			continue
		}

		err := x.encodeMember(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f.member, err)
		}
	}

	p.f("}")
	p.blank()

	return nil
}

func (x *xmlEmitter) encodeMember(f *field) error {
	p, r, name, v := x.p, f.pair.p, wireName(f), "v."+f.goName

	if r.flat && (r.shape.Type == spec.TypeList || r.shape.Type == spec.TypeMap) {
		return x.encodeFlat(r, v, name)
	}

	value := v
	if _, ok := xmlScalarRead(r); ok && r.shape.Type != spec.TypeTimestamp && r.shape.Type != spec.TypeBlob {
		value = "*" + v
	}

	p.f("\tif %s != nil {", v)

	err := x.encodeValue(r, value, name, 1)

	p.f("\t}")

	return err
}

// encodeFlat writes a flattened list or map: each element, or each entry, is
// an element of the member's own name.
func (x *xmlEmitter) encodeFlat(r ref, v, name string) error {
	p := x.p

	if r.shape.Type == spec.TypeList {
		elem, err := resolve(x.g.p, r.shape.Member)
		if err != nil {
			return err
		}

		p.f("\tfor _, e1 := range %s {", v)

		err = x.encodeValue(elem, "e1", cmp.Or(elem.loc, name), elemDepth)

		p.f("\t}")

		return err
	}

	p.f("\tfor _, k1 := range slices.Sorted(maps.Keys(%s)) {", v)
	p.f("\t\te1 := %s[k1]", v)
	p.f("\t\txw.Open(%q)", name)

	err := x.encodeEntry(r, 1)

	p.f("\t\txw.Close(%q)", name)
	p.f("\t}")

	return err
}

// encodeEntry writes the key k<depth> and the value e<depth> of a map entry.
func (x *xmlEmitter) encodeEntry(r ref, depth int) error {
	key, err := resolve(x.g.p, r.shape.Key)
	if err != nil {
		return err
	}

	val, err := resolve(x.g.p, r.shape.Value)
	if err != nil {
		return err
	}

	x.p.f("xw.String(%q, k%d)", cmp.Or(key.loc, "key"), depth)

	return x.encodeValue(val, fmt.Sprintf("e%d", depth), cmp.Or(val.loc, "value"), depth+1)
}

// encodeValue writes the value v of shape r as the element name. A struct v
// is addressable, or a pointer.
func (x *xmlEmitter) encodeValue(r ref, v, name string, depth int) error {
	p := x.p

	switch r.shape.Type {
	case spec.TypeString:
		// A namespace on a scalar is written only on a string; clients match
		// elements by local name, so leaving it off another scalar loses nothing.
		if r.xmlns == nil {
			p.f("xw.String(%q, %s)", name, v)

			return nil
		}

		openElement(p, "", name, r.xmlns)
		p.f("xw.Text(%s)", v)
		p.f("xw.Close(%q)", name)
	case spec.TypeInteger, spec.TypeLong:
		p.f("xw.Int(%q, int64(%s))", name, v)
	case spec.TypeFloat, spec.TypeDouble:
		p.f("xw.Float(%q, %s)", name, v)
	case spec.TypeBoolean:
		p.f("xw.Bool(%q, %s)", name, v)
	case spec.TypeTimestamp:
		p.f("xw.Time(%q, %s, %q)", name, v, x.g.bodyTime(r))
	case spec.TypeBlob:
		p.f("xw.Blob(%q, %s)", name, v)
	case spec.TypeStructure:
		openElement(p, "", name, r.xmlns)
		p.f("%s.encodeXML(xw)", v)
		p.f("xw.Close(%q)", name)
	case spec.TypeList:
		return x.encodeList(r, v, name, depth)
	case spec.TypeMap:
		openElement(p, "", name, r.xmlns)
		p.f("for _, k%d := range slices.Sorted(maps.Keys(%s)) {", depth, v)
		p.f("e%d := %s[k%d]", depth, v, depth)
		p.f("xw.Open(\"entry\")")

		err := x.encodeEntry(r, depth)
		if err != nil {
			return err
		}

		p.f("xw.Close(\"entry\")")
		p.f("}")
		p.f("xw.Close(%q)", name)
	default:
		return fmt.Errorf("shape %s of type %s is not supported", r.name, r.shape.Type)
	}

	return nil
}

// bodyTime is the format of a timestamp in a rest-xml body: the service's
// BodyTime in place of iso8601, the default, and else the model's.
func (g *generator) bodyTime(r ref) string {
	if g.opts.BodyTime != "" && (r.tsfmt == "" || strings.EqualFold(r.tsfmt, rest.TimeISO8601)) {
		return g.opts.BodyTime
	}

	return r.tsfmt
}

func (x *xmlEmitter) encodeList(r ref, v, name string, depth int) error {
	elem, err := resolve(x.g.p, r.shape.Member)
	if err != nil {
		return err
	}

	openElement(x.p, "", name, r.xmlns)
	x.p.f("for _, e%d := range %s {", depth, v)

	err = x.encodeValue(elem, fmt.Sprintf("e%d", depth), cmp.Or(elem.loc, "member"), depth+1)
	if err != nil {
		return err
	}

	x.p.f("}")
	x.p.f("xw.Close(%q)", name)

	return nil
}
