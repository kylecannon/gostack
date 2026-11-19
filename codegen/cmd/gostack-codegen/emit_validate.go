package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kylecannon/gostack/codegen/spec"
)

// The rule texts of AWS's constraint messages.
const (
	ruleMinLength = "Member must have length greater than or equal to "
	ruleMaxLength = "Member must have length less than or equal to "
	ruleMinValue  = "Member must have value greater than or equal to "
	ruleMaxValue  = "Member must have value less than or equal to "
	rulePattern   = "Member must satisfy regular expression pattern: "
	ruleEnum      = "Member must satisfy enum value set: "
)

// javaUnicode is a \uXXXX escape of a Java pattern, which Go writes \x{XXXX}.
var javaUnicode = regexp.MustCompile(`\\u([0-9A-Fa-f]{4})`)

// validator holds the pattern variables the validate.go file of a service
// collects while it is written.
type validator struct {
	g        *generator
	patterns map[string]string // Go expression to variable name
	order    []string          // Go expressions in the order they were named
}

// validateFile writes the Validate method of every operation input, the
// validate method of every struct below one that holds a member the service
// table lists, and the patterns they use.
func (g *generator) validateFile() (string, error) {
	v := &validator{g: g, patterns: map[string]string{}}
	body := &printer{}

	g.emitInputValidate(body)

	for _, st := range g.sortedStructs() {
		if !st.validates {
			continue
		}

		err := v.emitStruct(body, st)
		if err != nil {
			return "", fmt.Errorf("%s: %w", st.goName, err)
		}
	}

	p := &printer{}

	p.f("// Only the members listed in the generator's service table are checked.")
	p.blank()

	if len(v.order) > 0 {
		p.f("// The patterns of the string shapes, compiled when first used.")
		p.f("var (")

		for _, expr := range v.order {
			p.f("\t%s = constraint.NewPattern(%s)", v.patterns[expr], strconv.Quote(expr))
		}

		p.f(")")
		p.blank()
	}

	p.buf.Write(body.buf.Bytes())

	return p.buf.String(), nil
}

// emitInputValidate writes the exported Validate method of each operation
// input, which a handler calls to run the checks at its own point.
func (g *generator) emitInputValidate(p *printer) {
	seen := map[string]bool{}

	var inputs []*structType

	for _, name := range g.opNames {
		in := g.ops[name].input
		if !seen[in.goName] {
			seen[in.goName] = true
			inputs = append(inputs, in)
		}
	}

	slices.SortFunc(inputs, func(a, b *structType) int { return strings.Compare(a.goName, b.goName) })

	for _, in := range inputs {
		p.f("// Validate returns the error for the listed constraints v breaks, or nil.")
		p.f("// The dispatch never calls it: the handler does, where LocalStack makes the")
		p.f("// check, after its own earlier checks and any filtering of the input.")

		if !in.validates {
			p.f("func (*%s) Validate() error {", in.goName)
			p.f("\treturn nil")
			p.f("}")
			p.blank()

			continue
		}

		p.f("func (v *%s) Validate() error {", in.goName)
		p.f("\tvar r constraint.Report")
		p.f("\tv.validate(&r, nil)")
		p.f("\tif r.Failed() {")
		p.f("\t\treturn newValidationError(&r)")
		p.f("\t}")
		p.f("\treturn nil")
		p.f("}")
		p.blank()
	}
}

func (v *validator) emitStruct(p *printer, st *structType) error {
	p.f("// validate records in r every listed constraint v breaks, at the path at.")
	p.f("func (v *%s) validate(r *constraint.Report, at *constraint.Path) {", st.goName)

	for _, f := range st.fields {
		err := v.emitField(p, f)
		if err != nil {
			return fmt.Errorf("%s: %w", f.member, err)
		}
	}

	p.f("}")
	p.blank()

	return nil
}

// emitField writes the checks of one member: absence when it is listed as
// required, then, when it is set, its own listed constraints and the structs
// below it.
func (v *validator) emitField(p *printer, f *field) error {
	r := f.pair.p
	name := lowerFirst(f.member)
	x := "v." + f.goName
	required := f.checks&checkRequired != 0

	if r.shape.Document {
		if required {
			p.f("\tif %s == nil {", x)
			p.f("\t\tr.Null(at, %q)", name)
			p.f("\t}")
		}

		return nil
	}

	// A struct member holds a scalar by pointer; the checks read the value.
	val := x
	if strings.HasPrefix(f.typ, "*") && r.shape.Type != spec.TypeStructure && r.shape.Type != spec.TypeTimestamp {
		val = "*" + x
	}

	inner := &printer{}

	err := v.emitValue(inner, r, f.checks, val, name)
	if err != nil {
		return err
	}

	v.emitNested(inner, r, x, name)

	switch {
	case required:
		p.f("\tif %s == nil {", x)
		p.f("\t\tr.Null(at, %q)", name)

		if inner.buf.Len() > 0 {
			p.f("\t} else {")
			p.buf.Write(inner.buf.Bytes())
		}

		p.f("\t}")
	case inner.buf.Len() > 0:
		p.f("\tif %s != nil {", x)
		p.buf.Write(inner.buf.Bytes())
		p.f("\t}")
	default: // nothing to check
	}

	return nil
}

// emitValue writes the listed checks of a set member's own value, val:
// pattern, length or value bounds, and enum, in that order. AWS reports a
// pattern failure before a length failure of the same member.
func (v *validator) emitValue(p *printer, r ref, checks check, val, name string) error {
	sh := r.shape
	report := v.reporter(r, val, name)

	switch sh.Type {
	case spec.TypeString:
		if checks&checkPattern != 0 {
			err := v.emitPattern(p, r, val, report)
			if err != nil {
				return err
			}
		}

		err := emitBounds(p, sh, checks, "constraint.Length("+val+")", lengthBounds, report)
		if err != nil {
			return err
		}

		if checks&checkEnum != 0 {
			v.emitEnum(p, r, val, report)
		}

		return nil
	case spec.TypeBlob, spec.TypeList, spec.TypeMap:
		return emitBounds(p, sh, checks, "len("+val+")", lengthBounds, report)
	case spec.TypeInteger, spec.TypeLong, spec.TypeFloat, spec.TypeDouble:
		return emitBounds(p, sh, checks, val, valueBounds, report)
	default:
		return nil
	}
}

// bounds is what a min and max trait bound: a length, which is whole and of
// which a min of zero needs no check, or a value.
type bounds struct {
	minRule, maxRule string
	length           bool
}

var (
	lengthBounds = bounds{minRule: ruleMinLength, maxRule: ruleMaxLength, length: true}
	valueBounds  = bounds{minRule: ruleMinValue, maxRule: ruleMaxValue}
)

// reporter returns what writes one failure of the value val, shown as AWS
// shows it: the text of a string or number, "[a, b]" for a list of scalars,
// and "[]" or "{}" for an empty list or map. A sensitive value, a blob, and a
// non-empty list or map of structures are not shown; no snapshot pins how AWS
// prints the last two.
func (v *validator) reporter(r ref, val, name string) func(p *printer, rule string) {
	hidden := func(p *printer, rule string) {
		p.f("\t\tr.Hidden(at, %q, %q)", name, rule)
	}

	shown := func(text string) func(p *printer, rule string) {
		return func(p *printer, rule string) {
			p.f("\t\tr.Value(at, %q, %s, %q)", name, text, rule)
		}
	}

	empty := func(text string) func(p *printer, rule string) {
		return func(p *printer, rule string) {
			p.f("\t\tif len(%s) == 0 {", val)
			p.f("\t\t\tr.Value(at, %q, %q, %q)", name, text, rule)
			p.f("\t\t} else {")
			p.f("\t\t\tr.Hidden(at, %q, %q)", name, rule)
			p.f("\t\t}")
		}
	}

	if r.shape.Sensitive {
		return hidden
	}

	switch r.shape.Type {
	case spec.TypeString:
		return shown(val)
	case spec.TypeInteger, spec.TypeLong, spec.TypeFloat, spec.TypeDouble:
		return shown("constraint.Number(" + val + ")")
	case spec.TypeList:
		elem, err := resolve(v.g.p, r.shape.Member)
		if err == nil && isScalar(elem.shape) {
			return shown("constraint.Items(" + val + ")")
		}

		return empty("[]")
	case spec.TypeMap:
		return empty("{}")
	default:
		return hidden
	}
}

// isScalar reports whether a shape prints as itself inside a list.
func isScalar(sh *spec.Shape) bool {
	switch sh.Type {
	case spec.TypeString, spec.TypeInteger, spec.TypeLong, spec.TypeFloat, spec.TypeDouble, spec.TypeBoolean:
		return !sh.Sensitive
	default:
		return false
	}
}

// emitBounds writes the listed min and max checks of measure.
func emitBounds(p *printer, sh *spec.Shape, checks check, measure string, b bounds,
	report func(p *printer, rule string),
) error {
	if checks&checkMin != 0 {
		n, err := sh.Min.Int64()
		if err != nil && b.length {
			return fmt.Errorf("min %s is not a whole number", sh.Min)
		}

		if !b.length || n > 0 {
			p.f("\t\tif %s < %s {", measure, sh.Min)
			report(p, b.minRule+sh.Min.String())
			p.f("\t\t}")
		}
	}

	if checks&checkMax != 0 {
		_, err := sh.Max.Int64()
		if err != nil && b.length {
			return fmt.Errorf("max %s is not a whole number", sh.Max)
		}

		p.f("\t\tif %s > %s {", measure, sh.Max)
		report(p, b.maxRule+sh.Max.String())
		p.f("\t\t}")
	}

	return nil
}

// goPattern is the expression Go's regexp runs for a model pattern, which
// must match whole: a \uXXXX escape is written the way Go writes it.
func goPattern(pat string) string {
	return "^(?:" + javaUnicode.ReplaceAllString(pat, `\x{$1}`) + ")$"
}

// emitPattern writes the pattern check of a string.
func (v *validator) emitPattern(p *printer, r ref, val string, report func(p *printer, rule string)) error {
	pat := r.shape.Pattern
	expr := goPattern(pat)

	_, err := regexp.Compile(expr)
	if err != nil {
		return fmt.Errorf("pattern %s, which Go's regexp cannot compile: %w", pat, err)
	}

	varName, ok := v.patterns[expr]
	if !ok {
		varName = fmt.Sprintf("pattern%d", len(v.order)+1)
		v.patterns[expr] = varName
		v.order = append(v.order, expr)
	}

	p.f("\t\tif !%s.MatchString(%s) {", varName, val)
	report(p, rulePattern+pat)
	p.f("\t\t}")

	return nil
}

// emitEnum writes the enum check of a string, with the value list the
// service's Enums option gives in place of the model's.
func (v *validator) emitEnum(p *printer, r ref, val string, report func(p *printer, rule string)) {
	values := r.shape.Enum
	text := values

	if o, ok := v.g.opts.Validation.Enums[r.name]; ok {
		text = o.Values
		if o.Narrow {
			values = o.Values
		}
	}

	quoted := make([]string, len(values))
	for i, s := range values {
		quoted[i] = strconv.Quote(s)
	}

	p.f("\t\tswitch %s {", val)
	p.f("\t\tcase %s:", strings.Join(quoted, ", "))
	p.f("\t\tdefault:")
	report(p, ruleEnum+"["+strings.Join(text, ", ")+"]")
	p.f("\t\t}")
}

// emitNested writes the calls into the structs below a set member that
// validate: the member itself, the elements of a list, or the values of a
// map, at the path AWS gives them.
func (v *validator) emitNested(p *printer, r ref, x, name string) {
	switch r.shape.Type {
	case spec.TypeStructure:
		if v.g.structs[v.g.goTypeName(r.name)].validates {
			p.f("\t\t{")
			p.f("\t\t\tp := at.Field(%q)", name)
			p.f("\t\t\t%s.validate(r, &p)", x)
			p.f("\t\t}")
		}
	case spec.TypeList:
		elem, _ := resolve(v.g.p, r.shape.Member)
		if v.structValidates(elem) {
			p.f("\t\tfor i := range %s {", x)
			p.f("\t\t\tp := at.Item(%q, i)", name)
			p.f("\t\t\t%s[i].validate(r, &p)", x)
			p.f("\t\t}")
		}
	case spec.TypeMap:
		elem, _ := resolve(v.g.p, r.shape.Value)
		if v.structValidates(elem) {
			p.f("\t\tfor _, k := range constraint.Keys(%s) {", x)
			p.f("\t\t\te := %s[k]", x)
			p.f("\t\t\tp := at.Entry(%q, k)", name)
			p.f("\t\t\te.validate(r, &p)")
			p.f("\t\t}")
		}
	default: // a scalar has nothing below it
	}
}

// structValidates reports whether r is a structure with a validate method.
func (v *validator) structValidates(r ref) bool {
	return r.shape.Type == spec.TypeStructure && !r.shape.Document &&
		v.g.structs[v.g.goTypeName(r.name)].validates
}

// checkedStructs lists the structure a member's checks call into: the member
// itself, or the element of a list or the value of a map. A structure inside
// a list or map that is itself inside a list or map is not followed; no
// snapshot pins the path AWS gives it.
func (g *generator) checkedStructs(r ref) []string {
	if r.shape.Document {
		return nil
	}

	inner := r

	switch r.shape.Type {
	case spec.TypeStructure:
	case spec.TypeList:
		inner, _ = resolve(g.p, r.shape.Member)
	case spec.TypeMap:
		inner, _ = resolve(g.p, r.shape.Value)
	default:
		return nil
	}

	if inner.shape == nil || inner.shape.Type != spec.TypeStructure || inner.shape.Document {
		return nil
	}

	return []string{inner.name}
}

// markValidation sets the checks the service table lists on their fields,
// and flags the structs below an operation input that get a validate method:
// those that hold a listed member, on themselves or on a struct below them.
func (g *generator) markValidation() error {
	val := g.opts.Validation
	if val == nil {
		return nil
	}

	enumsUsed := map[string]bool{}

	for _, key := range sortedKeys(val.Checks) {
		f, err := g.listCheck(key, val.Checks[key])
		if err != nil {
			return fmt.Errorf("Validation.Checks %s: %w", key, err)
		}

		if f.checks&checkEnum != 0 {
			enumsUsed[f.pair.p.name] = true
		}
	}

	for _, name := range sortedKeys(val.Enums) {
		switch {
		case g.p.Shapes[name] == nil:
			return fmt.Errorf("Validation.Enums %s: no such shape in the model", name)
		case !enumsUsed[name]:
			return fmt.Errorf("Validation.Enums %s: no listed enum check uses it", name)
		}
	}

	reach := g.structsReaching(func(f *field) bool { return f.checks != 0 }, g.checkedStructs)

	var walk func(st *structType)

	walk = func(st *structType) {
		if st.validates || !reach[st.goName] {
			return
		}

		st.validates = true

		for _, f := range st.fields {
			for _, nested := range g.checkedStructs(f.pair.p) {
				walk(g.structs[g.goTypeName(nested)])
			}
		}
	}

	for _, name := range g.opNames {
		walk(g.ops[name].input)
	}

	for _, key := range sortedKeys(val.Checks) {
		shape, _, _ := strings.Cut(key, ".")
		if !g.structs[g.goTypeName(shape)].validates {
			return fmt.Errorf("Validation.Checks %s: no operation input reaches it along a path the checks follow", key)
		}
	}

	return nil
}

// listCheck sets the checks of the member key names, "Shape.Member" in the
// model's own names, after it makes sure the member has every trait they
// enforce. A key is exact, never a Go name or a differently cased one, so two
// keys never reach one field. It returns the member.
func (g *generator) listCheck(key string, c check) (*field, error) {
	shape, member, ok := strings.Cut(key, ".")
	if !ok || c == 0 {
		return nil, errors.New("want a Shape.Member key and at least one check")
	}

	if unknown := c &^ checkAll; unknown != 0 {
		return nil, fmt.Errorf("check bits %#b are not ones the generator knows", unknown)
	}

	// Only a model shape name is a key: shapeName and goTypeName also accept
	// a Go name, and a name in another case, for the same structure.
	sh := g.p.Shapes[shape]
	if sh == nil || sh.Type != spec.TypeStructure {
		return nil, fmt.Errorf("no structure %s in the model", shape)
	}

	st := g.structs[g.goTypeName(shape)]
	if st == nil {
		return nil, fmt.Errorf("no operation uses the structure %s", shape)
	}

	i := slices.IndexFunc(st.fields, func(f *field) bool { return f.member == member })
	if i < 0 {
		return nil, fmt.Errorf("%s has no member %s", shape, member)
	}

	f := st.fields[i]

	missing := traitsMissing(f, c)
	if len(missing) > 0 {
		return nil, fmt.Errorf("the member has no %s to check", strings.Join(missing, ", "))
	}

	f.checks = c

	return f, nil
}

// traitsMissing names the checks in c the member f has no trait for.
func traitsMissing(f *field, c check) []string {
	sh := f.pair.p.shape
	length := sh.Type == spec.TypeString || sh.Type == spec.TypeBlob || sh.Type == spec.TypeList || sh.Type == spec.TypeMap
	number := sh.Type == spec.TypeInteger || sh.Type == spec.TypeLong || sh.Type == spec.TypeFloat ||
		sh.Type == spec.TypeDouble

	_, patternErr := regexp.Compile(goPattern(sh.Pattern))

	has := map[check]bool{
		checkRequired: f.required,
		checkPattern:  sh.Type == spec.TypeString && sh.Pattern != "" && patternErr == nil,
		checkMin:      (number && sh.Min != "") || (length && sh.Min != "" && sh.Min != "0"),
		checkMax:      (number || length) && sh.Max != "",
		checkEnum:     sh.Type == spec.TypeString && len(sh.Enum) > 0,
	}

	names := []struct {
		c    check
		name string
	}{
		{checkRequired, "required trait"},
		{checkPattern, "pattern Go's regexp can compile"},
		{checkMin, "min"},
		{checkMax, "max"},
		{checkEnum, "enum"},
	}

	var missing []string

	for _, n := range names {
		if c&n.c != 0 && !has[n.c] {
			missing = append(missing, n.name)
		}
	}

	return missing
}

// lowerFirst writes a member name as AWS's messages do, its first letter in
// lower case: KeySpec is keySpec.
func lowerFirst(name string) string {
	r, size := utf8.DecodeRuneInString(name)

	return string(unicode.ToLower(r)) + name[size:]
}

// validationErrorFunc writes the error a request that breaks a constraint
// gets.
func (g *generator) validationErrorFunc(p *printer) {
	code := g.opts.Validation.Code
	if code == "" {
		code = "ValidationException"
	}

	p.f("// newValidationError returns the error for a request that breaks the constraints r lists.")
	p.f("func newValidationError(r *constraint.Report) *awserr.Error {")
	p.f("\treturn &awserr.Error{")
	p.f("\t\tCode: %q,", code)
	p.f("\t\tMessage: r.Message(),")
	p.f("\t\tStatusCode: 400,")
	p.f("\t\tFault: awserr.FaultSender,")
	p.f("\t}")
	p.f("}")
}
