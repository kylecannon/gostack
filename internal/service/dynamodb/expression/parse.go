package expression

import (
	"strconv"
	"unicode/utf16"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Kind names the request member an expression came from. DynamoDB's messages
// for a refused expression start "Invalid <Kind>: ".
type Kind string

// The expression members this package parses.
const (
	ConditionExpression    Kind = "ConditionExpression"
	FilterExpression       Kind = "FilterExpression"
	KeyConditionExpression Kind = "KeyConditionExpression"
	ProjectionExpression   Kind = "ProjectionExpression"
)

// The limits DynamoDB puts on an expression.
const (
	maxExpressionSize = 4096 // UTF-16 code units, as DynamoDB Local counts them
	maxOperators      = 300
	maxInOperands     = 100
	maxDocumentDepth  = 32
	// maxNesting bounds the recursion of every parser in the package. An
	// expression within maxExpressionSize cannot nest deeper, since each level
	// takes at least one character, so the bound never refuses what DynamoDB
	// accepts; it keeps a parser's stack bounded if it is ever handed a longer
	// text.
	maxNesting = maxExpressionSize
)

// msgTooLarge is the size message without the size, as DynamoDB Local answers
// it for ProjectionExpression and UpdateExpression.
const msgTooLarge = "Expression size has exceeded the maximum allowed size;"

// tooLarge reports whether src is over maxExpressionSize UTF-16 code units, and
// its length in them.
func tooLarge(src string) (bool, int) {
	n := utf16Len(src)

	return n > maxExpressionSize, n
}

// operandKind says what an operand is.
type operandKind int

const (
	opPath operandKind = iota
	opValue
	opFunc
)

// rawElem is one step of a path as written: an attribute name or #name token,
// or a list index.
type rawElem struct {
	tok   token // tokIdent, tokName or tokIndex
	index int64
}

// operand is a path, a :value or a function call. The parser fills the raw
// fields; resolve fills path and val.
type operand struct {
	kind   operandKind
	parens bool
	tok    token // the :value token, or the function name

	elems []rawElem // opPath
	path  Path

	val *api.AttributeValue // opValue: the canonical copy evaluation reads
	raw *api.AttributeValue // opValue: the request's value, for messages

	args []*operand // opFunc
	fn   string     // opFunc: the function's name, set by resolve
}

// condKind says what a condition node is.
type condKind int

const (
	condCompare condKind = iota
	condBetween
	condIn
	condFunc
	condNot
	condAnd
	condOr
)

// cond is one node of a parsed condition.
type cond struct {
	kind     condKind
	parens   bool
	op       tokKind    // condCompare: the comparator
	operands []*operand // compare: 2; between: value, low, high; in: value, list...
	fn       *operand   // condFunc
	sub      [2]*cond   // not: sub[0]; and, or: both
}

// parser is a recursive-descent parser over one expression's tokens.
type parser struct {
	src   string
	toks  []token
	pos   int
	kind  Kind
	depth int // the recursion depth of parseUnary and parseOperand
}

// enter counts one level of recursion and refuses one past maxNesting; the
// caller defers leave.
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxNesting {
		return invalid(p.kind, msgTooLarge)
	}

	return nil
}

func (p *parser) leave() { p.depth-- }

// newParser is the way into the parser of an expression a request sends: it
// refuses an empty or oversized expression before lexing, since the size cap is
// what bounds a parser's work and recursion, and then splits src into tokens.
// The paths of a PartiQL SELECT come in through newUnboundedParser.
func newParser(kind Kind, src string) (*parser, error) {
	err := checkSize(kind, src)
	if err != nil {
		return nil, err
	}

	return newUnboundedParser(kind, src), nil
}

// newUnboundedParser splits src into tokens without holding it to a size. Its
// caller bounds src.
func newUnboundedParser(kind Kind, src string) *parser {
	return &parser{src: src, toks: lex(src), kind: kind}
}

// checkSize refuses an empty or oversized expression. DynamoDB Local names the
// size of an oversized condition, filter or key condition, and not of a
// projection or update expression.
func checkSize(kind Kind, src string) error {
	if src == "" {
		return invalid(kind, "The expression can not be empty;")
	}

	over, n := tooLarge(src)
	if !over {
		return nil
	}

	if kind == ProjectionExpression || kind == updateExpression {
		return invalid(kind, msgTooLarge)
	}

	return invalid(kind, "Expression size has exceeded the maximum allowed size; expression size: "+strconv.Itoa(n))
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}

	return n
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tokEOF {
		p.pos++
	}

	return t
}

func (p *parser) text(t token) string { return p.src[t.start:t.end] }

func (p *parser) isKeyword(word string) bool {
	t := p.peek()

	return t.kind == tokKeyword && equalFold(p.text(t), word)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range len(a) {
		if a[i]|0x20 != b[i]|0x20 {
			return false
		}
	}

	return true
}

// syntaxError reports the token at the parser's position as DynamoDB does:
// the token, and the text from the token before it to the token after it. At
// the first token the text starts at the beginning of the expression, leading
// spaces and all (probed).
func (p *parser) syntaxError() error {
	t := p.peek()

	start := 0
	if p.pos > 0 {
		start = p.toks[p.pos-1].start
	}

	end := len(p.src)
	if t.kind != tokEOF && p.toks[p.pos+1].kind != tokEOF {
		end = p.toks[p.pos+1].end
	}

	name := "<EOF>"
	if t.kind != tokEOF {
		name = p.text(t)
	}

	return invalid(p.kind, `Syntax error; token: "`+name+`", near: "`+p.src[start:end]+`"`)
}

func (p *parser) expect(kind tokKind) error {
	if p.peek().kind != kind {
		return p.syntaxError()
	}

	p.next()

	return nil
}

func (p *parser) redundant() error {
	return invalid(p.kind, "The expression has redundant parentheses;")
}

// parseCondition parses a whole condition expression.
func (p *parser) parseCondition() (*cond, error) {
	c, _, err := p.parseOr(false)
	if err != nil {
		return nil, err
	}

	if p.peek().kind != tokEOF {
		return nil, p.syntaxError()
	}

	return c, nil
}

// parseOr parses a disjunction. With allowOperand, a bare operand directly
// followed by ")" is returned as the operand, for the caller to decide whether
// its parentheses wrapped an operand or a condition.
func (p *parser) parseOr(allowOperand bool) (*cond, *operand, error) {
	left, o, err := p.parseAnd(allowOperand)
	if err != nil || o != nil {
		return nil, o, err
	}

	for p.isKeyword("OR") {
		p.next()

		right, _, err := p.parseAnd(false)
		if err != nil {
			return nil, nil, err
		}

		left = &cond{kind: condOr, sub: [2]*cond{left, right}}
	}

	return left, nil, nil
}

func (p *parser) parseAnd(allowOperand bool) (*cond, *operand, error) {
	left, o, err := p.parseUnary(allowOperand)
	if err != nil || o != nil {
		return nil, o, err
	}

	for p.isKeyword("AND") {
		p.next()

		right, _, err := p.parseUnary(false)
		if err != nil {
			return nil, nil, err
		}

		left = &cond{kind: condAnd, sub: [2]*cond{left, right}}
	}

	return left, nil, nil
}

func (p *parser) parseUnary(allowOperand bool) (*cond, *operand, error) {
	defer p.leave()

	err := p.enter()
	if err != nil {
		return nil, nil, err
	}

	if p.isKeyword("NOT") {
		p.next()

		c, _, inner := p.parseUnary(false)
		if inner != nil {
			return nil, nil, inner
		}

		return &cond{kind: condNot, sub: [2]*cond{c}}, nil, nil
	}

	if p.peek().kind != tokLParen {
		o, inner := p.parseOperand()
		if inner != nil {
			return nil, nil, inner
		}

		return p.parseTail(o, allowOperand)
	}

	p.next()

	c, o, err := p.parseOr(true)
	if err != nil {
		return nil, nil, err
	}

	err = p.expect(tokRParen)
	if err != nil {
		return nil, nil, err
	}

	if o != nil {
		if o.parens {
			return nil, nil, p.redundant()
		}

		o.parens = true

		return p.parseTail(o, allowOperand)
	}

	if c.parens {
		return nil, nil, p.redundant()
	}

	c.parens = true

	return c, nil, nil
}

// parseTail parses what follows a condition's first operand: a comparator, a
// BETWEEN or an IN. With none, a function call stands as a condition of its own.
func (p *parser) parseTail(left *operand, allowOperand bool) (*cond, *operand, error) {
	t := p.peek()

	switch {
	case t.kind >= tokEQ && t.kind <= tokGE:
		p.next()

		right, err := p.parseOperand()
		if err != nil {
			return nil, nil, err
		}

		return &cond{kind: condCompare, op: t.kind, operands: []*operand{left, right}}, nil, nil
	case p.isKeyword("BETWEEN"):
		return p.parseBetween(left)
	case p.isKeyword("IN"):
		return p.parseIn(left)
	case allowOperand && t.kind == tokRParen:
		return nil, left, nil
	case left.kind == opFunc:
		return &cond{kind: condFunc, fn: left, parens: left.parens}, nil, nil
	default:
		return nil, nil, p.syntaxError()
	}
}

func (p *parser) parseBetween(left *operand) (*cond, *operand, error) {
	p.next()

	low, err := p.parseOperand()
	if err != nil {
		return nil, nil, err
	}

	if !p.isKeyword("AND") {
		return nil, nil, p.syntaxError()
	}

	p.next()

	high, err := p.parseOperand()
	if err != nil {
		return nil, nil, err
	}

	return &cond{kind: condBetween, operands: []*operand{left, low, high}}, nil, nil
}

func (p *parser) parseIn(left *operand) (*cond, *operand, error) {
	p.next()

	err := p.expect(tokLParen)
	if err != nil {
		return nil, nil, err
	}

	list, err := p.parseList()
	if err != nil {
		return nil, nil, err
	}

	return &cond{kind: condIn, operands: append([]*operand{left}, list...)}, nil, nil
}

// parseList parses operands separated by commas up to and including ")".
func (p *parser) parseList() ([]*operand, error) {
	var list []*operand

	for {
		o, err := p.parseOperand()
		if err != nil {
			return nil, err
		}

		list = append(list, o)

		if p.peek().kind != tokComma {
			break
		}

		p.next()
	}

	return list, p.expect(tokRParen)
}

// parseOperand parses a path, a :value, a function call, or one of them in
// parentheses.
func (p *parser) parseOperand() (*operand, error) {
	defer p.leave()

	err := p.enter()
	if err != nil {
		return nil, err
	}

	t := p.peek()

	switch t.kind {
	case tokLParen:
		p.next()

		o, err := p.parseOperand()
		if err != nil {
			return nil, err
		}

		err = p.expect(tokRParen)
		if err != nil {
			return nil, err
		}

		if o.parens {
			return nil, p.redundant()
		}

		o.parens = true

		return o, nil
	case tokValue:
		p.next()

		return &operand{kind: opValue, tok: t}, nil
	case tokIdent:
		if p.toks[p.pos+1].kind == tokLParen {
			p.next()
			p.next()

			args, err := p.parseList()
			if err != nil {
				return nil, err
			}

			return &operand{kind: opFunc, tok: t, args: args}, nil
		}

		return p.parsePath()
	case tokName:
		return p.parsePath()
	default:
		return nil, p.syntaxError()
	}
}

// parsePath parses a document path: a name, then any number of .name and
// [index] steps.
func (p *parser) parsePath() (*operand, error) {
	o := &operand{kind: opPath, elems: []rawElem{{tok: p.next()}}}

	for {
		switch p.peek().kind {
		case tokDot:
			p.next()

			t := p.peek()
			if t.kind != tokIdent && t.kind != tokName {
				return nil, p.syntaxError()
			}

			o.elems = append(o.elems, rawElem{tok: p.next()})
		case tokLBracket:
			p.next()

			t := p.peek()
			if t.kind != tokIndex {
				return nil, p.syntaxError()
			}

			p.next()

			n, err := strconv.ParseInt(p.text(t), 10, 64)
			if err != nil {
				n = -1 // beyond int64: refused as out of range when resolved
			}

			o.elems = append(o.elems, rawElem{tok: t, index: n})

			err = p.expect(tokRBracket)
			if err != nil {
				return nil, err
			}
		default:
			return o, nil
		}
	}
}

// invalid builds the ValidationException for a refused expression.
func invalid(kind Kind, msg string) error {
	return value.Invalid("Invalid " + string(kind) + ": " + msg)
}
