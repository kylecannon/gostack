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
	// msgUpdateFuncPlace is list_append's: inside another function or a sum.
	msgUpdateFuncPlace = updatePrefix + "The function is not allowed to be used this way in an expression; function: "
	// msgUpdateSameFirst is if_not_exists naming one path twice.
	msgUpdateSameFirst = updatePrefix + "The first operand must be distinct from the remaining operands for " +
		"this operator or function; operator: if_not_exists, first operand: "
	msgUpdateDepth  = updatePrefix + "The document path has too many nesting levels; nesting levels: "
	msgUpdateNoName = updatePrefix + "An expression attribute name used in the document path is not defined; " +
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
// names, overlapping paths, each action's values and operands, and last the
// operator count.
func ParseUpdate(text string, subs Substitutions, used map[string]struct{}) (*UpdateExpr, error) {
	return ParseUpdateRanked(text, subs, used, nil)
}

// ParseUpdateRanked is ParseUpdate for an expression compiled from a PartiQL
// UPDATE, whose actions the expression writes clause by clause where the
// statement lists them in its own order. rank[i] is the place in the statement
// of the i-th action written. DynamoDB Local adds a statement's paths to its
// overlap trie first to last, where it adds an UpdateExpression's last to
// first, so the pair an overlap names depends on that order (SQ-395). A rank
// that is not one place for each action is ignored.
func ParseUpdateRanked(
	text string, subs Substitutions, used map[string]struct{}, rank []int,
) (*UpdateExpr, error) {
	base, err := newParser(updateExpression, text)
	if err != nil {
		return nil, err
	}

	return parseUpdateFrom(base, subs, used, rank, false)
}

// ParseStatementUpdate is ParseUpdateRanked for the actions of a PartiQL UPDATE,
// which DynamoDB Local holds to no size of their own: an UPDATE of 300 SET
// actions compiles to an UpdateExpression past an expression's 4,096 code
// units and is applied, the statement's own 8,192 bytes bounding the text
// (SQ-412). The operator count holds as for any UpdateExpression: 300 operators
// are applied and 301 refused (SQ-452).
//
// Nor does it hold the value of an action to an UpdateExpression's grammar of
// one + or - and list_append as the whole value: the image applies sums and
// differences chained, parenthesized or nested in a function, and a list_append
// in any operand, and counts each + - and call as one operator (SQ-454).
func ParseStatementUpdate(
	text string, subs Substitutions, used map[string]struct{}, rank []int,
) (*UpdateExpr, error) {
	return parseUpdateFrom(newUnboundedParser(updateExpression, text), subs, used, rank, true)
}

// parseUpdateFrom parses base, the tokens of an UpdateExpression its caller has
// held to a size or knows to be bounded. statement is true for the actions of a
// PartiQL UPDATE.
func parseUpdateFrom(
	base *parser, subs Substitutions, used map[string]struct{}, rank []int, statement bool,
) (*UpdateExpr, error) {
	p := &updateParser{parser: base, statement: statement}

	err := p.parse()
	if err == nil {
		err = p.index
	}

	for _, check := range []func(*updateParser) error{
		updateCheckNames, updateCheckClauses, updateCheckDepth,
	} {
		if err != nil {
			return nil, err
		}

		err = check(p)
	}

	if err != nil {
		return nil, err
	}

	return updateResolve(p, subs, used, rank)
}

// updateResolve substitutes the placeholders of a parsed expression and runs
// the checks that need them.
func updateResolve(
	p *updateParser, subs Substitutions, used map[string]struct{}, rank []int,
) (*UpdateExpr, error) {
	err := updateResolveNames(p, subs.Names, used)
	if err != nil {
		return nil, err
	}

	u := &UpdateExpr{actions: p.actions, paths: make([]Path, len(p.actions))}
	for i, act := range p.actions {
		u.paths[i] = act.target.path
	}

	err = updateCheckOverlap(u.paths, rank)
	if err != nil {
		return nil, err
	}

	// DynamoDB checks the actions last to first, the operands of each in the
	// order written (probed in SQ-331).
	for _, act := range slices.Backward(p.actions) {
		err = updateCheckOperand(act.rhs, subs.Values, used)
		if err == nil {
			err = updateCheckAction(act)
		}

		if err != nil {
			return nil, err
		}
	}

	// DynamoDB Local counts the operators last, after every action's own checks
	// (probed in SQ-452): each + and - and each function call is one, and a
	// plain SET, REMOVE, ADD, DELETE and the elements of a path are none.
	if p.sums+len(p.calls) > maxOperators {
		return nil, invalid(updateExpression, msgTooManyOperators)
	}

	u.order = updateOrder(p.actions)

	return u, nil
}

// Paths returns the path of every action, in the order the expression lists
// them.
func (u *UpdateExpr) Paths() []Path {
	return u.paths
}

// updateCheckNames refuses the first, in the order written, of a reserved word
// used as an attribute name, a function an update expression does not take,
// and a list_append that is not the whole value of its SET action: DynamoDB
// takes it nowhere inside another function or a sum, parenthesized or not. A
// PartiQL UPDATE's list_append may stand anywhere (SQ-454).
func updateCheckNames(p *updateParser) error {
	var (
		err error
		at  int
	)

	for _, tok := range p.pathNames {
		if _, isReserved := reserved[strings.ToUpper(tok.text)]; tok.kind == tokIdent && isReserved {
			err, at = value.Invalid(msgUpdateReserved+tok.text), tok.start

			break
		}
	}

	for _, call := range p.calls {
		if err != nil && call.tok.start > at {
			break
		}

		callErr := updateCheckFunc(call, p.statement)
		if callErr != nil {
			return callErr
		}
	}

	return err
}

// updateCheckFunc refuses a function call an update expression does not take.
// anywhere is true for a PartiQL UPDATE, whose list_append may stand in any
// operand.
//
//nolint:revive // flag-parameter: who wrote the expression, not a mode
func updateCheckFunc(call *updateOperand, anywhere bool) error {
	name := call.tok.text

	if _, cond := updateCondFuncs[name]; cond {
		return value.Invalid(msgUpdateCondFunc + name)
	}

	if name != funcIfNotExists && name != funcListAppend {
		return value.Invalid(msgUpdateBadFunc + name)
	}

	if name == funcListAppend && !call.whole && !anywhere {
		return value.Invalid(msgUpdateFuncPlace + name)
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
		if tok.kind != tokName {
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
			case tokName:
				path.path[i] = PathElement{Name: names[tok.text]}
			case tokIndex:
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
// the same step (conflict), naming the pair DynamoDB Local does. The trie takes
// the paths last to first, as written, or in the order rank gives, first to
// last.
func updateCheckOverlap(paths []Path, rank []int) error {
	c := findPathClash(rankedPaths(paths, rank))
	switch {
	case c == nil:
		return nil
	case c.conflict:
		return value.Invalid(msgUpdateConflict + updatePathPair(c.one, c.two))
	default:
		return value.Invalid(msgUpdateOverlap + updatePathPair(c.one, c.two))
	}
}

// rankedPaths is paths laid out for findPathClash, which takes them last to
// first, so that it takes them in ascending order of rank. Without a rank that
// is a permutation of the paths, it is paths as written.
func rankedPaths(paths []Path, rank []int) []Path {
	if len(rank) != len(paths) {
		return paths
	}

	out := make([]Path, len(paths))
	placed := make([]bool, len(paths))

	for i, r := range rank {
		if r < 0 || r >= len(out) || placed[r] {
			return paths
		}

		out[len(out)-1-r], placed[r] = paths[i], true
	}

	return out
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
// first that its second does not repeat, and a value given to + and - must be
// a number and to list_append a list.
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

		// A validated copy with canonical numbers, as conditions take: ADD
		// and DELETE match set members by their canonical text.
		c, err := canonical(op.tok.text, &v)
		if err != nil {
			return err
		}

		op.val = *c

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
		first := op.args[0]
		if first.kind != updateOpPath {
			return value.Invalid(msgUpdateNeedsPath + name)
		}

		if rest := op.args[1]; rest.kind == updateOpPath && samePath(first.path.path, rest.path.path) {
			return value.Invalid(msgUpdateSameFirst + updatePathString(first.path.path))
		}

		return nil
	}

	return updateCheckValueTypes(op.args, name, value.TypeL)
}

// numberTypes is how DynamoDB Local names the type of a sum or difference in
// the message of an operand it refuses (SQ-454): the number types it holds.
const numberTypes = "{INT,N,FLOAT,HD,DOUBLE,DECIMAL}"

// updateCheckValueTypes refuses the first of args whose type is known before
// any item is read and is not want: a value, a sum or difference, which is a
// number, and a list_append, which is a list. A path, and if_not_exists, whose
// type is the item's, are left to the item.
func updateCheckValueTypes(args []*updateOperand, operator, want string) error {
	for _, arg := range args {
		t, shown := updateStaticType(arg)
		if t != "" && t != want {
			return value.Invalid(msgUpdateOperand + "operator or function: " + operator + ", operand type: " + shown)
		}
	}

	return nil
}

// updateStaticType is the type of op that holds whatever the item is, and the
// name DynamoDB Local gives it in a message, or "" for an operand that takes
// its type from the item.
func updateStaticType(op *updateOperand) (typ, shown string) {
	switch {
	case op.kind == updateOpValue:
		t := value.Type(&op.val)

		return t, t
	case op.kind == updateOpPlus || op.kind == updateOpMinus:
		return value.TypeN, numberTypes
	case op.kind == updateOpCall && op.tok.text == funcListAppend:
		return value.TypeL, value.TypeL
	default:
		return "", ""
	}
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
