package dynamodb

import (
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages a PartiQL statement's expressions are refused with.
const (
	msgMissingCompare = "MISSING must be compared using IS operator"
	msgUnknownFunc    = "Unrecognized function: "
	msgSetFuncTarget  = "The first argument to %s must equal the assignment value"
)

// render turns a statement's expressions into DynamoDB's expression
// language, with a #name for every attribute name and a :value for every
// value, so a statement runs through the expressions and operations the rest
// of the package has.
type render struct {
	params []api.AttributeValue
	names  map[string]string
	byName map[string]string
	values map[string]api.AttributeValue
}

func newRender(params []api.AttributeValue) *render {
	return &render{params: params}
}

// attributeNames is ExpressionAttributeNames: nil when no expression named one.
func (r *render) attributeNames() map[string]string { return r.names }

// attributeValues is ExpressionAttributeValues: nil when none held one.
func (r *render) attributeValues() map[string]api.AttributeValue { return r.values }

// name is the placeholder for the attribute name n.
func (r *render) name(n string) string {
	if ph, ok := r.byName[n]; ok {
		return ph
	}

	if r.names == nil {
		r.names, r.byName = map[string]string{}, map[string]string{}
	}

	ph := "#n" + strconv.Itoa(len(r.names))
	r.names[ph], r.byName[n] = n, ph

	return ph
}

// value is the placeholder for v.
func (r *render) value(v api.AttributeValue) string {
	if r.values == nil {
		r.values = map[string]api.AttributeValue{}
	}

	ph := ":v" + strconv.Itoa(len(r.values))
	r.values[ph] = v

	return ph
}

// path is the document path p with its names as placeholders.
func (r *render) path(p partiql.Path) string {
	var b strings.Builder

	for i, s := range p {
		switch {
		case s.IsIndex:
			b.WriteString("[" + strconv.Itoa(s.Index) + "]")
		case i > 0:
			b.WriteString("." + r.name(s.Name))
		default:
			b.WriteString(r.name(s.Name))
		}
	}

	return b.String()
}

// resolve is the value n stands for, with its parameters.
func (r *render) resolve(n *partiql.Node) (api.AttributeValue, error) {
	v, err := partiql.Value(n, r.params, rootKeyName)
	if err != nil {
		return api.AttributeValue{}, statementError(err)
	}

	// The value is checked on its own, before it stands in an expression, so
	// its message names no placeholder (verification/SQ-229 probe3).
	err = value.Validate(&v)
	if err != nil {
		return api.AttributeValue{}, err
	}

	return v, nil
}

// rootKeyName is where a value outside an INSERT's item stands, for messages.
const rootKeyName = "root"

// operand renders n where an operand stands: a path, a value, or size().
func (r *render) operand(n *partiql.Node) (string, error) {
	switch {
	case n.Kind == partiql.KindPath:
		return r.path(n.Path), nil
	case partiql.IsValue(n):
		v, err := r.resolve(n)
		if err != nil {
			return "", err
		}

		return r.value(v), nil
	case n.Kind == partiql.KindMissing:
		return "", value.Invalid(msgMissingCompare)
	case n.Kind == partiql.KindCall:
		return r.call(n)
	default:
		return r.cond(n)
	}
}

// The functions a condition may call, and those an UPDATE's SET may.
var (
	conditionFuncs = []string{
		"begins_with", "contains", "attribute_type", "size", "attribute_exists", "attribute_not_exists",
	}
	updateFuncs = []string{"list_append", "if_not_exists"}
)

// call renders a function call of a condition.
func (r *render) call(n *partiql.Node) (string, error) {
	if !slices.Contains(conditionFuncs, n.Name) {
		return "", value.Invalid(msgUnknownFunc + n.Text)
	}

	return r.callWith(n, r.operand)
}

func (*render) callWith(n *partiql.Node, arg func(*partiql.Node) (string, error)) (string, error) {
	args := make([]string, len(n.Args))

	for i, a := range n.Args {
		s, err := arg(a)
		if err != nil {
			return "", err
		}

		args[i] = s
	}

	return n.Name + "(" + strings.Join(args, ", ") + ")", nil
}

// cond renders n as a condition.
func (r *render) cond(n *partiql.Node) (string, error) {
	switch n.Kind {
	case partiql.KindAnd, partiql.KindOr:
		return r.joined(n)
	case partiql.KindNot:
		x, err := r.cond(n.Args[0])

		return "NOT (" + x + ")", err
	case partiql.KindCompare:
		return r.operands(n.Args, " "+n.Op+" ")
	case partiql.KindBetween:
		x, err := r.operand(n.Args[0])
		if err != nil {
			return "", err
		}

		bounds, err := r.operands(n.Args[1:], " AND ")

		return x + " BETWEEN " + bounds, err
	case partiql.KindIn:
		x, err := r.operand(n.Args[0])
		if err != nil {
			return "", err
		}

		list, err := r.operands(n.Args[1:], ", ")

		return x + " IN (" + list + ")", err
	case partiql.KindIsNull, partiql.KindIsMissing:
		return r.is(n)
	default:
		return r.operand(n)
	}
}

func (r *render) joined(n *partiql.Node) (string, error) {
	op := " AND "
	if n.Kind == partiql.KindOr {
		op = " OR "
	}

	a, err := r.cond(n.Args[0])
	if err != nil {
		return "", err
	}

	b, err := r.cond(n.Args[1])

	return "(" + a + ")" + op + "(" + b + ")", err
}

// operands renders ns as operands joined by sep.
func (r *render) operands(ns []*partiql.Node, sep string) (string, error) {
	out := make([]string, len(ns))

	for i, x := range ns {
		s, err := r.operand(x)
		if err != nil {
			return "", err
		}

		out[i] = s
	}

	return strings.Join(out, sep), nil
}

// is renders IS [NOT] NULL, a NULL-typed attribute, and IS [NOT] MISSING, an
// absent one.
func (r *render) is(n *partiql.Node) (string, error) {
	x, err := r.operand(n.Args[0])
	if err != nil {
		return "", err
	}

	if n.Kind == partiql.KindIsMissing {
		if n.Negated {
			return "attribute_exists(" + x + ")", nil
		}

		return "attribute_not_exists(" + x + ")", nil
	}

	s := "attribute_type(" + x + ", " + r.value(api.AttributeValue{S: new(value.TypeNULL)}) + ")"
	if n.Negated {
		return "NOT " + s, nil
	}

	return s, nil
}

// update renders an UPDATE's actions as an UpdateExpression: SET for an
// assignment, ADD and DELETE for set_add and set_delete, REMOVE.
func (r *render) update(actions []partiql.Action) (string, error) {
	var set, remove, add, del []string

	for _, a := range actions {
		target := r.path(a.Path)

		switch {
		case a.Value == nil:
			remove = append(remove, target)
		case a.Value.Kind == partiql.KindCall && (a.Value.Name == "set_add" || a.Value.Name == "set_delete"):
			operand, err := r.setFunc(a)
			if err != nil {
				return "", err
			}

			if a.Value.Name == "set_add" {
				add = append(add, target+" "+operand)
			} else {
				del = append(del, target+" "+operand)
			}
		default:
			v, err := r.updateValue(a.Value)
			if err != nil {
				return "", err
			}

			set = append(set, target+" = "+v)
		}
	}

	var b strings.Builder

	for _, clause := range []struct {
		kw    string
		items []string
	}{{"SET", set}, {"REMOVE", remove}, {"ADD", add}, {"DELETE", del}} {
		if len(clause.items) > 0 {
			b.WriteString(clause.kw + " " + strings.Join(clause.items, ", ") + " ")
		}
	}

	return strings.TrimSpace(b.String()), nil
}

// setFunc renders the set operand of SET p = set_add(p, v) or set_delete,
// whose first argument must be the assignment's own path.
func (r *render) setFunc(a partiql.Action) (string, error) {
	args := a.Value.Args
	if len(args) != 2 ||
		args[0].Kind != partiql.KindPath || !slices.Equal(args[0].Path, a.Path) {
		return "", value.Invalid(strings.Replace(msgSetFuncTarget, "%s", strings.ToUpper(a.Value.Name), 1))
	}

	return r.operand(args[1])
}

// updateValue renders the value an UPDATE's SET assigns.
func (r *render) updateValue(n *partiql.Node) (string, error) {
	switch n.Kind {
	case partiql.KindArith:
		return r.joinedUpdate(n)
	case partiql.KindCall:
		if !slices.Contains(updateFuncs, n.Name) {
			return "", value.Invalid(msgUnknownFunc + n.Text)
		}

		return r.callWith(n, r.updateValue)
	default:
		return r.operand(n)
	}
}

func (r *render) joinedUpdate(n *partiql.Node) (string, error) {
	a, err := r.updateValue(n.Args[0])
	if err != nil {
		return "", err
	}

	b, err := r.updateValue(n.Args[1])

	return a + " " + n.Op + " " + b, err
}
