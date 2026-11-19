package expression

import (
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// maxPathDepth is the most elements a document path may have.
const maxPathDepth = 32

// The messages DynamoDB Local answers for an update expression that parses but
// is refused, as LocalStack's image returns them.
const (
	msgUpdateReserved = updatePrefix + "Attribute name is a reserved keyword; reserved keyword: "
	msgUpdateBadFunc  = updatePrefix + "Invalid function name; function: "
	msgUpdateCondFunc = updatePrefix + "The function is not allowed in an update expression; function: "
	msgUpdateDepth    = updatePrefix + "The document path has too many nesting levels; nesting levels: "
	msgUpdateNoName   = updatePrefix + "An expression attribute name used in the document path is not defined; " +
		"attribute name: "
	msgUpdateNoValue = updatePrefix + "An expression attribute value used in expression is not defined; " +
		"attribute value: "
	msgUpdateOverlap   = updatePrefix + "Two document paths overlap with each other; " + msgUpdateRewrite
	msgUpdateConflict  = updatePrefix + "Two document paths conflict with each other; " + msgUpdateRewrite
	msgUpdateRewrite   = "must remove or rewrite one of these paths; "
	msgUpdateArity     = updatePrefix + "Incorrect number of operands for operator or function; operator or function: "
	msgUpdateNeedsPath = updatePrefix + "Operator or function requires a document path; operator or function: "
	msgUpdateOperand   = updatePrefix + "Incorrect operand type for operator or function; "
)

const (
	funcIfNotExists = "if_not_exists"
	funcListAppend  = "list_append"
)

// updateCondFuncs are the condition expression's functions, which an update
// expression names only to be refused.
var updateCondFuncs = map[string]struct{}{
	"attribute_exists": {}, "attribute_not_exists": {}, "attribute_type": {},
	"begins_with": {}, "contains": {}, "size": {},
}

// updateTypeNames is the name DynamoDB gives each type in the ADD and DELETE
// operand messages.
var updateTypeNames = map[string]string{
	value.TypeS: "STRING", value.TypeN: "NUMBER", value.TypeB: "BINARY",
	value.TypeSS: "STRING_SET", value.TypeNS: "NUMBER_SET", value.TypeBS: "BINARY_SET",
	value.TypeM: "MAP", value.TypeL: "LIST", value.TypeNULL: "NULL", value.TypeBOOL: "BOOLEAN",
}

// UpdateExpr is a parsed UpdateExpression with its placeholders substituted.
type UpdateExpr struct {
	actions []*updateAction
	// order is actions in the order Apply carries them out: every action but
	// REMOVE by ascending path, then REMOVE by descending path, so that list
	// indexes keep naming the elements of the old item.
	order []*updateAction
	paths []Path
}

var _ Update = (*UpdateExpr)(nil)

// ParseUpdate parses text, an UpdateExpression, substituting subs. It adds every
// "#name" and ":value" the expression uses to used, which may be nil, so the
// caller can refuse the unused ones across all of a request's expressions with
// CheckUnused. An error is DynamoDB's ValidationException for the first
// problem, in the order DynamoDB checks: the syntax and list index range, then
// reserved words, function names, a repeated clause, path depth, undefined
// names, overlapping paths, and last each action's values and operands.
func ParseUpdate(text string, subs Substitutions, used map[string]struct{}) (*UpdateExpr, error) {
	if text == "" {
		return nil, value.Invalid(msgUpdateEmpty)
	}

	// Before lexing: the cap is what bounds the parser's work and recursion.
	if over, _ := tooLarge(text); over {
		return nil, value.Invalid(updatePrefix + msgTooLarge)
	}

	p := &updateParser{text: text, toks: updateLex(text)}

	err := p.parse()
	if err == nil {
		err = p.index
	}

	for _, check := range []func(*updateParser) error{
		updateCheckReserved, updateCheckFuncs, updateCheckClauses, updateCheckDepth,
	} {
		if err != nil {
			return nil, err
		}

		err = check(p)
	}

	if err != nil {
		return nil, err
	}

	return updateResolve(p, subs, used)
}

// updateResolve substitutes the placeholders of a parsed expression and runs
// the checks that need them.
func updateResolve(p *updateParser, subs Substitutions, used map[string]struct{}) (*UpdateExpr, error) {
	err := updateResolveNames(p, subs.Names, used)
	if err != nil {
		return nil, err
	}

	u := &UpdateExpr{actions: p.actions, paths: make([]Path, len(p.actions))}
	for i, act := range p.actions {
		u.paths[i] = act.target.path
	}

	err = updateCheckOverlap(u.paths)
	if err != nil {
		return nil, err
	}

	for _, act := range p.actions {
		err = updateCheckOperand(act.rhs, subs.Values, used)
		if err == nil {
			err = updateCheckAction(act)
		}

		if err != nil {
			return nil, err
		}
	}

	u.order = updateOrder(p.actions)

	return u, nil
}

// Paths returns the path of every action, in the order the expression lists
// them.
func (u *UpdateExpr) Paths() []Path {
	return u.paths
}

func updateCheckReserved(p *updateParser) error {
	for _, tok := range p.pathNames {
		if _, reserved := updateReserved[strings.ToLower(tok.text)]; tok.kind == updateTokName && reserved {
			return value.Invalid(msgUpdateReserved + tok.text)
		}
	}

	return nil
}

func updateCheckFuncs(p *updateParser) error {
	for _, tok := range p.funcs {
		if _, cond := updateCondFuncs[tok.text]; cond {
			return value.Invalid(msgUpdateCondFunc + tok.text)
		}

		if tok.text != funcIfNotExists && tok.text != funcListAppend {
			return value.Invalid(msgUpdateBadFunc + tok.text)
		}
	}

	return nil
}

func updateCheckClauses(p *updateParser) error {
	for i, clause := range p.clauses {
		if slices.Contains(p.clauses[:i], clause) {
			return value.Invalid(updatePrefix + "The \"" + updateClauseNames[clause] +
				"\" section can only be used once in an update expression;")
		}
	}

	return nil
}

func updateCheckDepth(p *updateParser) error {
	for _, path := range p.paths {
		if len(path.raw) > maxPathDepth {
			return value.Invalid(msgUpdateDepth + strconv.Itoa(len(path.raw)))
		}
	}

	return nil
}

// updateResolveNames substitutes the "#name" placeholders of every path, in
// the order they are written, and builds each path.
func updateResolveNames(p *updateParser, names map[string]string, used map[string]struct{}) error {
	for _, tok := range p.pathNames {
		if tok.kind != updateTokNameRef {
			continue
		}

		if _, ok := names[tok.text]; !ok {
			return value.Invalid(msgUpdateNoName + tok.text)
		}

		if used != nil {
			used[tok.text] = struct{}{}
		}
	}

	for _, path := range p.paths {
		path.path = make(Path, len(path.raw))

		for i, tok := range path.raw {
			switch tok.kind {
			case updateTokNameRef:
				path.path[i] = PathElement{Name: names[tok.text]}
			case updateTokIndex:
				n, _ := strconv.Atoi(tok.text)
				path.path[i] = PathElement{Index: n}
			default:
				path.path[i] = PathElement{Name: tok.text}
			}
		}
	}

	return nil
}

// updateCheckOverlap refuses two action paths where one is a prefix of the
// other (overlap) or where one names a map key and the other a list index at
// the same step (conflict).
func updateCheckOverlap(paths []Path) error {
	for j := range paths {
		for i := range j {
			prefix, conflict := updateComparePaths(paths[i], paths[j])

			switch {
			case conflict:
				return value.Invalid(msgUpdateConflict + updatePathPair(paths[i], paths[j]))
			case prefix:
				return value.Invalid(msgUpdateOverlap + updatePathPair(paths[i], paths[j]))
			default:
			}
		}
	}

	return nil
}

// updateComparePaths reports whether a and b agree on every step they share,
// and whether they part at a step where one names a key and the other an
// index.
func updateComparePaths(a, b Path) (bool, bool) {
	for k := range min(len(a), len(b)) {
		x, y := a[k], b[k]
		if (x.Name == "") != (y.Name == "") {
			return false, true
		}

		if x != y {
			return false, false
		}
	}

	return true, false
}

func updatePathPair(a, b Path) string {
	return "path one: " + updatePathString(a) + ", path two: " + updatePathString(b)
}

// updatePathString writes a path as DynamoDB's messages do: [a, [1], b].
func updatePathString(path Path) string {
	var b strings.Builder

	b.WriteByte('[')

	for i, el := range path {
		if i > 0 {
			b.WriteString(", ")
		}

		if el.Name == "" {
			b.WriteString("[" + strconv.Itoa(el.Index) + "]")
		} else {
			b.WriteString(el.Name)
		}
	}

	b.WriteByte(']')

	return b.String()
}

// updateCheckOperand checks an operand after its own operands: a value must be
// defined, a function must have two operands and if_not_exists a path for its
// first, and a value given to + and - must be a number and to list_append a
// list.
func updateCheckOperand(op *updateOperand, values map[string]api.AttributeValue, used map[string]struct{}) error {
	if op == nil {
		return nil
	}

	for _, arg := range op.args {
		err := updateCheckOperand(arg, values, used)
		if err != nil {
			return err
		}
	}

	switch op.kind {
	case updateOpValue:
		v, ok := values[op.tok.text]
		if !ok {
			return value.Invalid(msgUpdateNoValue + op.tok.text)
		}

		op.val = v

		if used != nil {
			used[op.tok.text] = struct{}{}
		}
	case updateOpCall:
		return updateCheckCall(op)
	case updateOpPlus:
		return updateCheckValueTypes(op.args, "+", value.TypeN)
	case updateOpMinus:
		return updateCheckValueTypes(op.args, "-", value.TypeN)
	default:
	}

	return nil
}

func updateCheckCall(op *updateOperand) error {
	name := op.tok.text

	const operands = 2
	if len(op.args) != operands {
		return value.Invalid(msgUpdateArity + name + ", number of operands: " + strconv.Itoa(len(op.args)))
	}

	if name == funcIfNotExists {
		if op.args[0].kind != updateOpPath {
			return value.Invalid(msgUpdateNeedsPath + name)
		}

		return nil
	}

	return updateCheckValueTypes(op.args, name, value.TypeL)
}

// updateCheckValueTypes refuses the first of args that is a value of a type
// other than want.
func updateCheckValueTypes(args []*updateOperand, operator, want string) error {
	for _, arg := range args {
		if t := value.Type(&arg.val); arg.kind == updateOpValue && t != want {
			return value.Invalid(msgUpdateOperand + "operator or function: " + operator + ", operand type: " + t)
		}
	}

	return nil
}

// updateCheckAction refuses an ADD value that is not a number or a set, and a
// DELETE value that is not a set.
func updateCheckAction(act *updateAction) error {
	if act.clause != updateAdd && act.clause != updateDelete {
		return nil
	}

	allowed := updateClauseNames[act.clause]

	switch t := value.Type(&act.rhs.val); t {
	case value.TypeSS, value.TypeNS, value.TypeBS:
		return nil
	case value.TypeN:
		if act.clause == updateAdd {
			return nil
		}

		fallthrough
	default:
		return value.Invalid(msgUpdateOperand + "operator: " + allowed + ", operand type: " + updateTypeNames[t] +
			", typeSet: ALLOWED_FOR_" + allowed + "_OPERAND")
	}
}

// updateOrder returns actions in the order Apply carries them out.
func updateOrder(actions []*updateAction) []*updateAction {
	order := slices.Clone(actions)
	slices.SortStableFunc(order, func(a, b *updateAction) int {
		ra, rb := a.clause == updateRemove, b.clause == updateRemove
		switch {
		case ra != rb && ra:
			return 1
		case ra != rb:
			return -1
		case ra:
			return updateComparePathOrder(b.target.path, a.target.path)
		default:
			return updateComparePathOrder(a.target.path, b.target.path)
		}
	})

	return order
}

// updateComparePathOrder orders two paths at the first step where they part:
// by index for two indexes, by name otherwise.
func updateComparePathOrder(a, b Path) int {
	for k := range min(len(a), len(b)) {
		x, y := a[k], b[k]

		switch {
		case x.Name != y.Name:
			return strings.Compare(x.Name, y.Name)
		case x.Index != y.Index:
			return x.Index - y.Index
		default:
		}
	}

	return len(a) - len(b)
}
