package expression

import (
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for an update expression it cannot
// parse, as LocalStack's image returns them.
const (
	updatePrefix        = "Invalid UpdateExpression: "
	msgUpdateEmpty      = updatePrefix + "The expression can not be empty;"
	msgUpdateIndexRange = updatePrefix + "List index is not within the allowable range; index: "
)

// indexBits is the width of the largest list index DynamoDB accepts: a Java int.
const indexBits = 32

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
	raw  []updateToken
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
	kind updateOperandKind
	path *updatePathExpr
	tok  updateToken // the placeholder of a value, or the name of a function
	val  api.AttributeValue
	args []*updateOperand
}

// updateAction is one action of a clause: SET target = rhs, REMOVE target,
// ADD target rhs, or DELETE target rhs, where rhs of ADD and DELETE is a value.
type updateAction struct {
	clause updateClause
	target *updatePathExpr
	rhs    *updateOperand
}

// updateParser is a recursive descent parser over the tokens of one update
// expression. It records, in the order they appear, what the checks after a
// successful parse need: the clauses, every path, every attribute name and
// "#name" written in a path, and every function name.
type updateParser struct {
	text  string
	toks  []updateToken
	pos   int
	index error
	depth int // the recursion depth of operand

	actions   []*updateAction
	clauses   []updateClause
	paths     []*updatePathExpr
	pathNames []updateToken
	funcs     []updateToken
}

// parse reads the whole expression: one or more clauses, then the end.
func (p *updateParser) parse() error {
	if len(p.toks) == 0 {
		return p.fail()
	}

	for p.peek() != nil {
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
	if tok.kind != updateTokName || !ok {
		return p.fail()
	}

	p.pos++
	p.clauses = append(p.clauses, clause)

	for {
		err := p.action(clause)
		if err != nil {
			return err
		}

		if !p.peek().is(",") {
			return nil
		}

		p.pos++
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
		if !p.peek().is("=") {
			return p.fail()
		}

		p.pos++

		act.rhs, err = p.setValue()
	default:
		act.rhs, err = p.valueRef()
	}

	if err != nil {
		return err
	}

	p.actions = append(p.actions, act)

	return nil
}

// setValue reads an operand, or two joined by + or -.
func (p *updateParser) setValue() (*updateOperand, error) {
	left, err := p.operand()
	if err != nil {
		return nil, err
	}

	var kind updateOperandKind

	switch tok := p.peek(); {
	case tok.is("+"):
		kind = updateOpPlus
	case tok.is("-"):
		kind = updateOpMinus
	default:
		return left, nil
	}

	p.pos++

	right, err := p.operand()
	if err != nil {
		return nil, err
	}

	return &updateOperand{kind: kind, args: []*updateOperand{left, right}}, nil
}

// operand reads a value, a parenthesized set value, a function call or a path.
func (p *updateParser) operand() (*updateOperand, error) {
	p.depth++
	defer func() { p.depth-- }()

	if p.depth > maxNesting {
		return nil, value.Invalid(updatePrefix + msgTooLarge)
	}

	tok := p.peek()

	switch {
	case tok == nil:
		return nil, p.fail()
	case tok.kind == updateTokValueRef:
		return p.valueRef()
	case tok.is("("):
		p.pos++

		inner, err := p.setValue()
		if err != nil {
			return nil, err
		}

		if !p.peek().is(")") {
			return nil, p.fail()
		}

		p.pos++

		return inner, nil
	case tok.kind == updateTokName && p.pos+1 < len(p.toks) && p.toks[p.pos+1].is("("):
		return p.call()
	default:
		path, err := p.pathExpr()
		if err != nil {
			return nil, err
		}

		return &updateOperand{kind: updateOpPath, path: path}, nil
	}
}

// call reads a function name and its parenthesized, comma-separated operands.
func (p *updateParser) call() (*updateOperand, error) {
	op := &updateOperand{kind: updateOpCall, tok: p.toks[p.pos]}
	p.funcs = append(p.funcs, op.tok)
	p.pos += 2

	for {
		arg, err := p.operand()
		if err != nil {
			return nil, err
		}

		op.args = append(op.args, arg)

		switch tok := p.peek(); {
		case tok.is(","):
			p.pos++
		case tok.is(")"):
			p.pos++

			return op, nil
		default:
			return nil, p.fail()
		}
	}
}

// valueRef reads a ":value" placeholder.
func (p *updateParser) valueRef() (*updateOperand, error) {
	tok := p.peek()
	if tok == nil || tok.kind != updateTokValueRef {
		return nil, p.fail()
	}

	p.pos++

	return &updateOperand{kind: updateOpValue, tok: *tok}, nil
}

// pathExpr reads a document path: a name, then any run of .name and [index].
func (p *updateParser) pathExpr() (*updatePathExpr, error) {
	path := &updatePathExpr{}

	err := p.pathName(path)
	if err != nil {
		return nil, err
	}

	for {
		switch tok := p.peek(); {
		case tok.is("."):
			p.pos++

			err = p.pathName(path)
		case tok.is("["):
			p.pos++

			err = p.pathIndex(path)
		default:
			p.paths = append(p.paths, path)

			return path, nil
		}

		if err != nil {
			return nil, err
		}
	}
}

// pathName reads one name of a path: a bare name that is not a clause keyword,
// or a "#name".
func (p *updateParser) pathName(path *updatePathExpr) error {
	tok := p.peek()
	if tok == nil {
		return p.fail()
	}

	_, keyword := updateClauses[strings.ToUpper(tok.text)]

	switch {
	case tok.kind == updateTokName && !keyword:
	case tok.kind == updateTokNameRef:
	default:
		return p.fail()
	}

	p.pos++
	path.raw = append(path.raw, *tok)
	p.pathNames = append(p.pathNames, *tok)

	return nil
}

// pathIndex reads the index and closing bracket of a [index] step. An index
// past the largest a list may hold is reported only once the whole expression
// parses, as DynamoDB reports it.
func (p *updateParser) pathIndex(path *updatePathExpr) error {
	tok := p.peek()
	if tok == nil || tok.kind != updateTokIndex {
		return p.fail()
	}

	p.pos++

	if !p.peek().is("]") {
		return p.fail()
	}

	p.pos++

	_, err := strconv.ParseInt(tok.text, decimalBase, indexBits)
	if err != nil && p.index == nil {
		p.index = value.Invalid(msgUpdateIndexRange + "[" + tok.text + "]")
	}

	path.raw = append(path.raw, *tok)

	return nil
}

// peek returns the next token, or nil at the end.
func (p *updateParser) peek() *updateToken {
	if p.pos >= len(p.toks) {
		return nil
	}

	return &p.toks[p.pos]
}

// fail returns DynamoDB's syntax error for the token at p.pos: the token, and
// the text from the token before it to the token after it.
func (p *updateParser) fail() error {
	token := "<EOF>"
	start, end := 0, len(p.text)

	if p.pos < len(p.toks) {
		token = p.toks[p.pos].text
	}

	if p.pos > 0 {
		start = p.toks[p.pos-1].start
	}

	if p.pos+1 < len(p.toks) {
		end = p.toks[p.pos+1].end
	}

	return value.Invalid(updatePrefix + "Syntax error; token: \"" + token + "\", near: \"" + p.text[start:end] + "\"")
}
