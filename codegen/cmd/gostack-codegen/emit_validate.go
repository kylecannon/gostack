package main

import (
	"fmt"
	"regexp"
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

// validator holds what the validate.go file of a service collects while it is
// written: the pattern variables, and the constraints it leaves unchecked.
type validator struct {
	g         *generator
	patterns  map[string]string // Go expression to variable name
	order     []string          // Go expressions in the order they were named
	unchecked []string
	seen      map[string]bool // unchecked lines already listed
}

// validateFile writes the validate method of every struct below an operation
// input that has a constraint somewhere below it, and the patterns they use.
func (g *generator) validateFile() (string, error) {
	v := &validator{g: g, patterns: map[string]string{}, seen: map[string]bool{}}
	body := &printer{}

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

	if len(v.unchecked) > 0 {
		p.f("// The checks below leave these constraints of the model unchecked; a")
		p.f("// handler that needs them checks them itself:")
		p.f("//")

		for _, line := range v.unchecked {
			p.f("//   - %s", line)
		}

		p.blank()
		p.f("// validateUnchecked counts the constraints listed above.")
		p.f("const validateUnchecked = %d", len(v.unchecked))
		p.blank()
	}

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

// skip lists a constraint the checks leave out.
func (v *validator) skip(line string) {
	if !v.seen[line] {
		v.seen[line] = true
		v.unchecked = append(v.unchecked, line)
	}
}

func (v *validator) emitStruct(p *printer, st *structType) error {
	p.f("// validate records in r every constraint of the model v breaks, at the path at.")
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

// emitField writes the checks of one member: absence when it is required,
// then, when it is set, its own constraints and the structs below it.
func (v *validator) emitField(p *printer, f *field) error {
	r := f.pair.p
	name := lowerFirst(f.member)
	x := "v." + f.goName

	if r.shape.Document {
		if f.required {
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

	err := v.emitValue(inner, r, val, name)
	if err != nil {
		return err
	}

	v.emitNested(inner, r, x, name)

	switch {
	case f.required:
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

// emitValue writes the checks of a set member's own value, val: pattern,
// length or value bounds, and enum, in that order. AWS reports a pattern
// failure before a length failure of the same member.
func (v *validator) emitValue(p *printer, r ref, val, name string) error {
	sh := r.shape
	report := v.reporter(r, val, name)

	switch sh.Type {
	case spec.TypeString:
		v.emitPattern(p, r, val, report)

		err := emitBounds(p, sh, "constraint.Length("+val+")", lengthBounds, report)
		if err != nil {
			return err
		}

		v.emitEnum(p, r, val, report)

		return nil
	case spec.TypeBlob, spec.TypeList, spec.TypeMap:
		return emitBounds(p, sh, "len("+val+")", lengthBounds, report)
	case spec.TypeInteger, spec.TypeLong, spec.TypeFloat, spec.TypeDouble:
		return emitBounds(p, sh, val, valueBounds, report)
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

// emitBounds writes the min and max checks of measure.
func emitBounds(p *printer, sh *spec.Shape, measure string, b bounds, report func(p *printer, rule string)) error {
	if sh.Min != "" {
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

	if sh.Max != "" {
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

// emitPattern writes the pattern check of a string, which must match whole.
// A pattern Go's regexp cannot compile, a lookahead for one, is left out and
// listed, not approximated; a \uXXXX escape is written the way Go writes it.
func (v *validator) emitPattern(p *printer, r ref, val string, report func(p *printer, rule string)) {
	pat := r.shape.Pattern
	if pat == "" {
		return
	}

	expr := "^(?:" + javaUnicode.ReplaceAllString(pat, `\x{$1}`) + ")$"

	_, err := regexp.Compile(expr)
	if err != nil {
		v.skip(fmt.Sprintf("%s: pattern %s, which Go's regexp cannot compile", r.name, pat))

		return
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
}

// emitEnum writes the enum check of a string, with the value list the
// service's Enums option gives in place of the model's.
func (v *validator) emitEnum(p *printer, r ref, val string, report func(p *printer, rule string)) {
	values := r.shape.Enum
	if len(values) == 0 {
		return
	}

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

// emitNested writes the calls into the structs below a set member: the member
// itself, the elements of a list, or the values of a map, at the path AWS
// gives them. The constraints of scalar list elements and of map keys and
// values are reported by AWS in a form no snapshot pins, so they are listed as
// unchecked instead.
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
		} else if hasConstraint(elem.shape) {
			v.skip(fmt.Sprintf("%s: the constraints of its elements, %s", r.name, elem.name))
		}
	case spec.TypeMap:
		key, _ := resolve(v.g.p, r.shape.Key)
		if hasConstraint(key.shape) {
			v.skip(fmt.Sprintf("%s: the constraints of its keys, %s", r.name, key.name))
		}

		elem, _ := resolve(v.g.p, r.shape.Value)
		if v.structValidates(elem) {
			p.f("\t\tfor _, k := range constraint.Keys(%s) {", x)
			p.f("\t\t\te := %s[k]", x)
			p.f("\t\t\tp := at.Entry(%q, k)", name)
			p.f("\t\t\te.validate(r, &p)")
			p.f("\t\t}")
		} else if hasConstraint(elem.shape) {
			v.skip(fmt.Sprintf("%s: the constraints of its values, %s", r.name, elem.name))
		}
	default: // a scalar has nothing below it
	}
}

// structValidates reports whether r is a structure with a validate method.
func (v *validator) structValidates(r ref) bool {
	return r.shape.Type == spec.TypeStructure && !r.shape.Document &&
		v.g.structs[v.g.goTypeName(r.name)].validates
}

// hasConstraint reports whether a shape has a trait the checks act on, which
// for a list element or map value would go unchecked.
func hasConstraint(sh *spec.Shape) bool {
	if sh.Type == spec.TypeStructure || sh.Type == spec.TypeList || sh.Type == spec.TypeMap {
		return false
	}

	minSet := sh.Min != "" && sh.Min != "0"

	return minSet || sh.Max != "" || sh.Pattern != "" || len(sh.Enum) > 0
}

// markValidation flags the structs below an operation input that get a
// validate method: those with a required member, a constrained member, or
// such a struct below them.
func (g *generator) markValidation() {
	if g.opts.Validation == nil {
		return
	}

	memo := map[string]bool{}

	var needs func(st *structType) bool

	needs = func(st *structType) bool {
		if res, ok := memo[st.goName]; ok {
			return res
		}

		memo[st.goName] = false // a cycle adds nothing by itself

		result := false

		for _, f := range st.fields {
			r := f.pair.p
			if f.required || (!r.shape.Document && ownConstraint(r.shape)) {
				result = true
			}

			for _, nested := range g.nestedStructs(r) {
				if needs(g.structs[g.goTypeName(nested)]) {
					result = true
				}
			}
		}

		memo[st.goName] = result

		return result
	}

	var walk func(st *structType)

	walk = func(st *structType) {
		if st.validates || !needs(st) {
			return
		}

		st.validates = true

		for _, f := range st.fields {
			for _, nested := range g.nestedStructs(f.pair.p) {
				walk(g.structs[g.goTypeName(nested)])
			}
		}
	}

	for _, name := range g.opNames {
		walk(g.ops[name].input)
	}
}

// ownConstraint reports whether a member's own value has a trait the checks
// act on.
func ownConstraint(sh *spec.Shape) bool {
	switch sh.Type {
	case spec.TypeString:
		return (sh.Min != "" && sh.Min != "0") || sh.Max != "" || sh.Pattern != "" || len(sh.Enum) > 0
	case spec.TypeBlob, spec.TypeList, spec.TypeMap:
		return (sh.Min != "" && sh.Min != "0") || sh.Max != ""
	case spec.TypeInteger, spec.TypeLong, spec.TypeFloat, spec.TypeDouble:
		return sh.Min != "" || sh.Max != ""
	default:
		return false
	}
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
