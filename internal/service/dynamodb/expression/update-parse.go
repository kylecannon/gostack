package expression

import (
	"math"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// updateExpression is the Kind of an UpdateExpression: its messages start
// "Invalid UpdateExpression: ".
const updateExpression Kind = "UpdateExpression"

// The messages DynamoDB Local answers for an update expression it cannot
// parse, as LocalStack's image returns them.
const (
	updatePrefix        = "Invalid " + string(updateExpression) + ": "
	msgUpdateIndexRange = updatePrefix + "List index is not within the allowable range; index: "
)

// updateClause is one of the four clauses of an update expression.
type updateClause int

const (
	updateSet updateClause = iota + 1
	updateRemove
	updateAdd
	updateDelete
)

// updateClauses maps a clause keyword, upper-cased, to its clause.
var updateClauses = map[string]updateClause{
	"SET": updateSet, "REMOVE": updateRemove, "ADD": updateAdd, "DELETE": updateDelete,
}

// updateClauseNames is the keyword of each clause, as DynamoDB spells it in a
// message.
var updateClauseNames = map[updateClause]string{
	updateSet: "SET", updateRemove: "REMOVE", updateAdd: "ADD", updateDelete: "DELETE",
}

// updatePathExpr is a document path as written, and once the names are
// substituted, the path it stands for.
type updatePathExpr struct {
	raw  []token
	path Path
}

// updateOperandKind is the kind of one operand of a SET action.
type updateOperandKind int

const (
	updateOpPath updateOperandKind = iota + 1
	updateOpValue
	updateOpCall
	updateOpPlus
	updateOpMinus
)

// updateOperand is the right-hand side of a SET action, or a part of one: a
// path, a value, a function call, or a sum or difference of two operands. The
// value of a ":value" is filled in once the placeholders are checked.
type updateOperand struct {
	kind   updateOperandKind
	parens bool // written in parentheses
	whole  bool // the whole right-hand side of a SET action, not a part of one
	path   *updatePathExpr
	tok    token // the placeholder of a value, or the name of a function
	val    api.AttributeValue
	args   []*updateOperand
}

// updateAction is one action of a clause: SET target = rhs, REMOVE target,
// ADD target rhs, or DELETE target rhs, where rhs of ADD and DELETE is a value.
type updateAction struct {
	clause updateClause
	target *updatePathExpr
	rhs    *updateOperand
}

// updateParser is a recursive descent parser over the tokens of one update
// expression. It shares the condition parser's tokens, path parser, syntax
// error and nesting bound, and records, in the order they appear, what the
// checks after a successful parse need: the clauses, every path, every
// attribute name and "#name" written in a path, and every function call.
type updateParser struct {
	*parser

	statement bool // the actions of a PartiQL UPDATE: a value is an expression of any depth
	actions   []*updateAction
	clauses   []updateClause
	paths     []*updatePathExpr
	pathNames []token
	calls     []*updateOperand
	sums      int   // the + and - written, which with the calls are the operators maxOperators counts
	index     error // the first list index out of range, reported after the parse
}

// parse reads the whole expression: one or more clauses, then the end.
func (p *updateParser) parse() error {
	if p.peek().kind == tokEOF {
		return p.syntaxError()
	}

	for p.peek().kind != tokEOF {
		err := p.clause()
		if err != nil {
			return err
		}
	}

	return nil
}

// clause reads a clause keyword and its comma-separated actions.
func (p *updateParser) clause() error {
	tok := p.peek()

	clause, ok := updateClauses[strings.ToUpper(tok.text)]
	if tok.kind != tokKeyword || !ok {
		return p.syntaxError()
	}

	p.next()
	p.clauses = append(p.clauses, clause)

	for {
		err := p.action(clause)
		if err != nil {
			return err
		}

		if p.peek().kind != tokComma {
			return nil
		}

		p.next()
	}
}

// action reads one action of clause.
func (p *updateParser) action(clause updateClause) error {
	target, err := p.pathExpr()
	if err != nil {
		return err
	}

	act := &updateAction{clause: clause, target: target}

	switch clause {
	case updateRemove:
	case updateSet:
		err = p.expect(tokEQ)
		if err == nil {
			act.rhs, err = p.setValue()
		}
	default:
		act.rhs, err = p.valueRef()
	}

	if err != nil {
		return err
	}

	if act.rhs != nil {
		act.rhs.whole = true
	}

	p.actions = append(p.actions, act)

	return nil
}

// setValue reads an operand, or two joined by + or -. Its first operand may be
// a parenthesized set value; a parenthesized sum or difference is the whole
// set value and takes no + or - after it, as DynamoDB Local's grammar has it.
// The value of a PartiQL UPDATE is read by chain instead.
func (p *updateParser) setValue() (*updateOperand, error) {
	left, err := p.operand(true)
	if err != nil {
		return nil, err
	}

	if p.statement {
		return p.chain(left)
	}

	var kind updateOperandKind

	switch {
	case left.kind == updateOpPlus || left.kind == updateOpMinus:
		return left, nil
	case p.peek().kind == tokPlus:
		kind = updateOpPlus
	case p.peek().kind == tokMinus:
		kind = updateOpMinus
	default:
		return left, nil
	}

	p.next()

	p.sums++

	right, err := p.operand(false)
	if err != nil {
		return nil, err
	}

	return &updateOperand{kind: kind, args: []*updateOperand{left, right}}, nil
}

// chain reads the + and - after left, grouping from the left: the image applies
// a PartiQL UPDATE's value as an expression, so that a + b - c is (a + b) - c,
// and counts each one an operator (SQ-454). A right operand is a set value of
// its own when it is parenthesized.
func (p *updateParser) chain(left *updateOperand) (*updateOperand, error) {
	for {
		var kind updateOperandKind

		switch p.peek().kind {
		case tokPlus:
			kind = updateOpPlus
		case tokMinus:
			kind = updateOpMinus
		default:
			return left, nil
		}

		p.next()

		p.sums++

		right, err := p.operand(true)
		if err != nil {
			return nil, err
		}

		left = &updateOperand{kind: kind, args: []*updateOperand{left, right}}
	}
}

// operand reads a value, a parenthesized operand, a function call or a path.
// Inside the parentheses comes a set value when sum is true, the first operand
// of a set value, and an operand otherwise. Parentheses directly around
// parentheses are refused as redundant when they close.
func (p *updateParser) operand(sum bool) (*updateOperand, error) {
	defer p.leave()

	err := p.enter()
	if err != nil {
		return nil, err
	}

	tok := p.peek()

	switch {
	case tok.kind == tokValue:
		return p.valueRef()
	case tok.kind == tokLParen:
		return p.parenthesized(sum)
	case tok.kind == tokIdent && p.toks[p.pos+1].kind == tokLParen:
		return p.call()
	default:
		path, err := p.pathExpr()
		if err != nil {
			return nil, err
		}

		return &updateOperand{kind: updateOpPath, path: path}, nil
	}
}

// parenthesized reads "(", a set value or an operand, and ")".
//
//nolint:revive // flag-parameter: the grammar position, not a mode
func (p *updateParser) parenthesized(sum bool) (*updateOperand, error) {
	p.next()

	var (
		inner *updateOperand
		err   error
	)

	if sum {
		inner, err = p.setValue()
	} else {
		inner, err = p.operand(false)
	}

	if err != nil {
		return nil, err
	}

	err = p.expect(tokRParen)
	if err != nil {
		return nil, err
	}

	if inner.parens {
		return nil, p.redundant()
	}

	inner.parens = true

	return inner, nil
}

// call reads a function name and its parenthesized, comma-separated operands.
func (p *updateParser) call() (*updateOperand, error) {
	op := &updateOperand{kind: updateOpCall, tok: p.next()}
	p.calls = append(p.calls, op)
	p.next()

	for {
		arg, err := p.argument()
		if err != nil {
			return nil, err
		}

		op.args = append(op.args, arg)

		switch p.peek().kind {
		case tokComma:
			p.next()
		case tokRParen:
			p.next()

			return op, nil
		default:
			return nil, p.syntaxError()
		}
	}
}

// argument reads an operand of a function: a single operand, or in a PartiQL
// UPDATE a set value, which may be a sum.
func (p *updateParser) argument() (*updateOperand, error) {
	if p.statement {
		return p.setValue()
	}

	return p.operand(false)
}

// valueRef reads a ":value" placeholder.
func (p *updateParser) valueRef() (*updateOperand, error) {
	if p.peek().kind != tokValue {
		return nil, p.syntaxError()
	}

	return &updateOperand{kind: updateOpValue, tok: p.next()}, nil
}

// pathExpr reads a document path with the shared path parser: a name or
// "#name", then any run of .name and [index]. A clause keyword, like every
// keyword, is not a name.
func (p *updateParser) pathExpr() (*updatePathExpr, error) {
	if k := p.peek().kind; k != tokIdent && k != tokName {
		return nil, p.syntaxError()
	}

	o, err := p.parsePath()
	if err != nil {
		return nil, err
	}

	path := &updatePathExpr{raw: make([]token, len(o.elems))}

	for i, e := range o.elems {
		path.raw[i] = e.tok

		switch {
		case e.tok.kind != tokIndex:
			p.pathNames = append(p.pathNames, e.tok)
		case (e.index < 0 || e.index > math.MaxInt32) && p.index == nil:
			// Past a Java int: DynamoDB reports it once the whole expression parses.
			p.index = value.Invalid(msgUpdateIndexRange + "[" + e.tok.text + "]")
		default:
		}
	}

	p.paths = append(p.paths, path)

	return path, nil
}
