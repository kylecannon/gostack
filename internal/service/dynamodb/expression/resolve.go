package expression

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The functions of the condition grammar, by name, with the number of
// operands each takes. Names are case-sensitive.
var functions = map[string]int{
	fnAttributeExists: oneOperand, fnAttributeNotExists: oneOperand, fnAttributeType: twoOperands,
	fnBeginsWith: twoOperands, fnContains: twoOperands, fnSize: oneOperand,
}

const (
	oneOperand  = 1
	twoOperands = 2
)

const (
	fnAttributeExists    = "attribute_exists"
	fnAttributeNotExists = "attribute_not_exists"
	fnAttributeType      = "attribute_type"
	fnBeginsWith         = "begins_with"
	fnContains           = "contains"
	fnSize               = "size"
)

// typeNames are the type names attribute_type accepts, in the order DynamoDB
// Local lists them.
const typeNames = "{N,BS,L,B,NULL,M,S,SS,NS,BOOL}"

// pathTypes is the operand type DynamoDB Local names when attribute_type's
// type operand is a document path.
const pathTypes = "{DOUBLE,B,L,NS,NULL,DICT,S,DECIMALSET,FLOAT,BS,INT,SS,M,BOOL," +
	"N,DOUBLESET,INTSET,FS,HD,HDS,DECIMAL}"

// CheckSubstitutions checks a request's ExpressionAttributeNames and
// ExpressionAttributeValues as DynamoDB does before it reads any expression: a
// map given empty, a malformed key, an empty name, an invalid value. It does
// not change subs.
func CheckSubstitutions(subs Substitutions) error {
	if subs.Names != nil && len(subs.Names) == 0 {
		return value.Invalid("ExpressionAttributeNames must not be empty")
	}

	for _, k := range sortedKeys(subs.Names) {
		if !validPlaceholder(k, '#') {
			return value.Invalid(`ExpressionAttributeNames contains invalid key: Syntax error; key: "` + k + `"`)
		}

		if subs.Names[k] == "" {
			return value.Invalid("ExpressionAttributeNames contains invalid value: Empty attribute name for key " + k)
		}
	}

	if subs.Values != nil && len(subs.Values) == 0 {
		return value.Invalid("ExpressionAttributeValues must not be empty")
	}

	for _, k := range sortedKeys(subs.Values) {
		if !validPlaceholder(k, ':') {
			return value.Invalid(`ExpressionAttributeValues contains invalid key: Syntax error; key: "` + k + `"`)
		}

		v := subs.Values[k]
		_, err := canonical(k, &v)
		if err != nil {
			return err
		}
	}

	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// canonical returns a validated copy of v with its numbers in canonical form,
// leaving v as the request sent it.
func canonical(key string, v *api.AttributeValue) (*api.AttributeValue, error) {
	c := deepCopy(v)

	err := value.Validate(&c)
	if err != nil {
		msg := err.Error()

		if aerr, ok := errors.AsType[*awserr.Error](err); ok {
			msg = aerr.Message
		}

		return nil, value.Invalid("ExpressionAttributeValues contains invalid value: " + msg + " for key " + key)
	}

	return &c, nil
}

func deepCopy(v *api.AttributeValue) api.AttributeValue {
	c := *v

	if v.N != nil {
		n := *v.N
		c.N = &n
	}

	if v.NS != nil {
		c.NS = append([]string(nil), v.NS...)
	}

	if v.M != nil {
		c.M = make(map[string]api.AttributeValue, len(v.M))
		for k, e := range v.M {
			c.M[k] = deepCopy(&e)
		}
	}

	if v.L != nil {
		c.L = make([]api.AttributeValue, len(v.L))
		for i := range v.L {
			c.L[i] = deepCopy(&v.L[i])
		}
	}

	return c
}

// resolver checks a parsed expression and substitutes its placeholders.
type resolver struct {
	p    *parser
	subs Substitutions
	used map[string]struct{}
}

// use records that the expression used a placeholder.
func (r *resolver) use(key string) {
	if r.used != nil {
		r.used[key] = struct{}{}
	}
}

func (r *resolver) fail(msg string) error { return invalid(r.p.kind, msg) }

// checkReserved refuses the first attribute name, in the order the text
// writes them, that is a reserved word.
func (r *resolver) checkReserved(o *operand) error {
	if o.kind == opFunc {
		for _, a := range o.args {
			err := r.checkReserved(a)
			if err != nil {
				return err
			}
		}

		return nil
	}

	for _, e := range o.elems {
		if e.tok.kind != tokIdent {
			continue
		}

		name := r.p.text(e.tok)
		if _, ok := reserved[strings.ToUpper(name)]; ok {
			return r.fail("Attribute name is a reserved keyword; reserved keyword: " + name)
		}
	}

	return nil
}

// resolvePath substitutes a path's names after checking its depth and indexes.
func (r *resolver) resolvePath(o *operand) error {
	if len(o.elems) > maxDocumentDepth {
		return r.fail("The document path has too many nesting levels; nesting levels: " + strconv.Itoa(len(o.elems)))
	}

	o.path = make(Path, len(o.elems))

	for i, e := range o.elems {
		switch e.tok.kind {
		case tokIndex:
			if e.index < 0 || e.index > math.MaxInt32 {
				return r.fail("List index is not within the allowable range; index: [" + r.p.text(e.tok) + "]")
			}

			o.path[i] = PathElement{Index: int(e.index)}
		case tokName:
			key := r.p.text(e.tok)

			name, ok := r.subs.Names[key]
			if !ok {
				return r.fail("An expression attribute name used in the document path is not defined; attribute name: " + key)
			}

			r.use(key)
			o.path[i] = PathElement{Name: name}
		default:
			o.path[i] = PathElement{Name: r.p.text(e.tok)}
		}
	}

	return nil
}

func (r *resolver) resolveValue(o *operand) error {
	key := r.p.text(o.tok)

	v, ok := r.subs.Values[key]
	if !ok {
		return r.fail("An expression attribute value used in expression is not defined; attribute value: " + key)
	}

	r.use(key)

	c, err := canonical(key, &v)
	if err != nil {
		return err
	}

	o.raw, o.val = &v, c

	return nil
}

// resolveOperand resolves an operand used as an operand: a function there must
// be size.
func (r *resolver) resolveOperand(o *operand) error {
	switch o.kind {
	case opPath:
		return r.resolvePath(o)
	case opValue:
		return r.resolveValue(o)
	default:
		return r.resolveFunc(o, useOperand)
	}
}

// funcUse says where a function call stands: as a condition or as an operand.
type funcUse int

const (
	useOperand funcUse = iota
	useCondition
)

// resolveFunc resolves a function call used as a condition or as an operand.
func (r *resolver) resolveFunc(o *operand, use funcUse) error {
	name := r.p.text(o.tok)

	want, ok := functions[name]
	if !ok {
		return r.fail("Invalid function name; function: " + name)
	}

	o.fn = name

	for _, a := range o.args {
		err := r.resolveOperand(a)
		if err != nil {
			return err
		}
	}

	if (use == useCondition) == (name == fnSize) {
		return r.fail("The function is not allowed to be used this way in an expression; function: " + name)
	}

	if len(o.args) != want {
		return r.fail("Incorrect number of operands for operator or function; operator or function: " +
			name + ", number of operands: " + strconv.Itoa(len(o.args)))
	}

	return r.checkFuncOperands(name, o.args)
}

func (r *resolver) checkFuncOperands(name string, args []*operand) error {
	switch name {
	case fnAttributeExists, fnAttributeNotExists:
		if args[0].kind != opPath {
			return r.fail("Operator or function requires a document path; operator or function: " + name)
		}
	case fnAttributeType:
		return r.checkTypeOperand(args[1])
	case fnBeginsWith:
		err := r.distinct(name, args)
		if err != nil {
			return err
		}

		for _, a := range args {
			if t := operandType(a); t != "" && t != value.TypeS && t != value.TypeB {
				return r.wrongType(name, t)
			}
		}
	case fnContains:
		return r.distinct(name, args)
	default: // size
		if t := operandType(args[0]); t == value.TypeN || t == value.TypeBOOL || t == value.TypeNULL {
			return r.wrongType(name, t)
		}
	}

	return nil
}

func (r *resolver) checkTypeOperand(a *operand) error {
	if a.kind == opPath {
		return r.wrongType(fnAttributeType, pathTypes)
	}

	if a.kind == opFunc {
		return r.wrongType(fnAttributeType, value.TypeN)
	}

	if t := value.Type(a.val); t != value.TypeS {
		return r.wrongType(fnAttributeType, t)
	}

	if !validTypeName(*a.val.S) {
		return r.fail("Invalid attribute type name found; type: " + *a.val.S + ", valid types: " + typeNames)
	}

	return nil
}

func validTypeName(t string) bool {
	switch t {
	case value.TypeS, value.TypeN, value.TypeB, value.TypeSS, value.TypeNS, value.TypeBS,
		value.TypeM, value.TypeL, value.TypeNULL, value.TypeBOOL:
		return true
	default:
		return false
	}
}

// operandType is the type an operand has before any item is read: a value's
// type, N for size(), and "" for a path.
func operandType(o *operand) string {
	switch o.kind {
	case opValue:
		return value.Type(o.val)
	case opFunc:
		return value.TypeN
	default:
		return ""
	}
}

func (r *resolver) wrongType(name, t string) error {
	return r.fail("Incorrect operand type for operator or function; operator or function: " +
		name + ", operand type: " + t)
}

// distinct refuses a first operand that is a path repeated among the others.
func (r *resolver) distinct(name string, ops []*operand) error {
	if ops[0].kind != opPath {
		return nil
	}

	for _, o := range ops[1:] {
		if o.kind == opPath && samePath(o.path, ops[0].path) {
			return r.fail("The first operand must be distinct from the remaining operands " +
				"for this operator or function; operator: " +
				name + ", first operand: " + formatPath(ops[0].path))
		}
	}

	return nil
}

func samePath(a, b Path) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// formatPath writes a path as DynamoDB's messages do: [a, [0], b].
func formatPath(p Path) string {
	parts := make([]string, len(p))
	for i, e := range p {
		if e.Name == "" {
			parts[i] = "[" + strconv.Itoa(e.Index) + "]"
		} else {
			parts[i] = e.Name
		}
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

// formatValue writes a scalar as DynamoDB Local's messages do.
func formatValue(v *api.AttributeValue) string {
	switch {
	case v.S != nil:
		return "AttributeValue: {S:" + *v.S + "}"
	case v.N != nil:
		return "AttributeValue: {N:" + *v.N + "}"
	default:
		n := strconv.Itoa(len(v.B))

		return "AttributeValue: {B:java.nio.HeapByteBuffer[pos=0 lim=" + n + " cap=" + n + "]}"
	}
}
