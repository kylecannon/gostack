package partiql

import (
	"strconv"
	"strings"
)

// Error is a statement DynamoDB refuses, with DynamoDB Local's message.
type Error struct {
	Message string
	// ExistsOnly is set on a statement that is not well formed but is an
	// EXISTS around an expression, which a caller outside a transaction refuses
	// as an EXISTS (existsOperand).
	ExistsOnly bool
}

func (e *Error) Error() string { return e.Message }

// MsgNotWellFormed starts the message for a statement that does not parse.
const MsgNotWellFormed = "Statement wasn't well formed, can't be processed: "

// errMalformed is the sentinel a parse step returns for a statement that
// does not parse; Parse turns it into the message naming the statement.
var errMalformed = &Error{}

// errExistsExpression is the sentinel for an EXISTS around an expression, a
// statement Parse refuses as not well formed with ExistsOnly set.
var errExistsExpression = &Error{}

// atError is a message that names the token at.
func atError(msg string, at Pos) *Error {
	return &Error{Message: msg + " at " + at.String()}
}

// Parse parses one statement.
func Parse(src string) (*Statement, error) {
	toks, ok := lex(src)
	if !ok {
		return nil, &Error{Message: MsgNotWellFormed + src}
	}

	p := &parser{toks: toks}

	s, err := p.statement()
	if err == nil && p.peek().punct(";") {
		p.i++
	}

	if err == nil && p.peek().kind != tokEOF {
		err = errMalformed
	}

	if err == errMalformed { //nolint:errorlint // the sentinel itself, never wrapped
		return nil, &Error{Message: MsgNotWellFormed + src}
	}

	if err == errExistsExpression { //nolint:errorlint // the sentinel itself, never wrapped
		return nil, &Error{Message: MsgNotWellFormed + src, ExistsOnly: true}
	}

	if err != nil {
		return nil, err
	}

	s.Params = p.params

	return s, nil
}

// parser walks the tokens of one statement.
type parser struct {
	toks   []token
	i      int
	params int
}

func (p *parser) peek() token { return p.toks[p.i] }

// peekNext is the token after the next one, or the end.
func (p *parser) peekNext() token {
	if p.i+1 < len(p.toks) {
		return p.toks[p.i+1]
	}

	return p.toks[len(p.toks)-1]
}

func (p *parser) take() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}

	return t
}

// keyword takes the keyword kw, or fails.
func (p *parser) keyword(kw string) error {
	if !p.peek().is(kw) {
		return errMalformed
	}

	p.i++

	return nil
}

// expect takes the punctuation s, or fails.
func (p *parser) expect(s string) error {
	if !p.peek().punct(s) {
		return errMalformed
	}

	p.i++

	return nil
}

func (p *parser) statement() (*Statement, error) {
	t := p.take()

	switch {
	case t.is("SELECT"):
		return p.selectStatement()
	case t.is("INSERT"):
		return p.insertStatement()
	case t.is("UPDATE"):
		return p.updateStatement()
	case t.is("DELETE"):
		return p.deleteStatement()
	case t.is("EXISTS"):
		return p.existsStatement()
	case t.kind == tokIdent && !reserved(t.text) && p.peek().kind == tokEOF:
		// A lone name is a PartiQL expression, just not a statement
		// DynamoDB runs (verification/SQ-229 probe2, "tx syntax").
		return nil, &Error{Message: MsgVariable}
	default:
		return nil, errMalformed
	}
}

// MsgVariable refuses a statement that is a lone name.
const MsgVariable = "Unsupported operation: VariableReference"

func (p *parser) selectStatement() (*Statement, error) {
	s := &Statement{Kind: Select}

	if p.peek().punct("*") {
		p.i++
	} else {
		for {
			path, err := p.projectedPath()
			if err != nil {
				return nil, err
			}

			s.Projection = append(s.Projection, path)

			if !p.peek().punct(",") {
				break
			}

			p.i++
		}
	}

	err := p.keyword("FROM")
	if err == nil {
		err = p.target(s, true)
	}

	if err == nil {
		err = p.where(s)
	}

	if err == nil && p.peek().is("ORDER") {
		err = p.orderBy(s)
	}

	if err != nil {
		return nil, err
	}

	return s, nil
}

// projectedPath is one path of a SELECT list. A name followed by "(" is a
// function, which a projection cannot hold.
func (p *parser) projectedPath() (Path, error) {
	if t := p.peek(); t.kind == tokIdent && p.peekNext().punct("(") {
		return nil, atError("Unexpected path component", t.at)
	}

	return p.path()
}

func (p *parser) orderBy(s *Statement) error {
	p.i++

	err := p.keyword("BY")
	if err != nil {
		return err
	}

	s.OrderBy, err = p.path()
	if err != nil {
		return err
	}

	switch t := p.peek(); {
	case t.is("ASC"):
		p.i++
	case t.is("DESC"):
		p.i++
		s.Desc = true
	default:
	}

	return nil
}

// target reads the table a statement names and, where index allows it, the
// index after a dot.
//
//nolint:revive // flag-parameter: whether the statement may read an index
func (p *parser) target(s *Statement, index bool) error {
	name, ok := p.name()
	if !ok {
		return errMalformed
	}

	s.Table = name

	if index && p.peek().punct(".") {
		p.i++

		if s.Index, ok = p.name(); !ok {
			return errMalformed
		}
	}

	return nil
}

// name takes an identifier, bare or quoted.
func (p *parser) name() (string, bool) {
	t := p.peek()
	if t.kind != tokIdent && t.kind != tokQuoted {
		return "", false
	}

	if t.kind == tokIdent && reserved(t.text) {
		return "", false
	}

	p.i++

	return t.text, true
}

// where reads an optional WHERE clause.
func (p *parser) where(s *Statement) error {
	if !p.peek().is("WHERE") {
		return nil
	}

	p.i++

	var err error

	s.Where, err = p.expr()

	return err
}

func (p *parser) insertStatement() (*Statement, error) {
	s := &Statement{Kind: Insert}

	err := p.keyword("INTO")
	if err == nil {
		err = p.target(s, false)
	}

	if err == nil {
		err = p.keyword("VALUE")
	}

	if err != nil {
		return nil, err
	}

	if t := p.peek(); !t.punct("{") {
		return nil, atError("Unsupported format in item expression. "+
			"Please use struct format {...} to express item values for insert statement.", t.at)
	}

	s.Value, err = p.primary()
	if err != nil {
		return nil, err
	}

	if p.peek().is("RETURNING") {
		return nil, &Error{Message: "RETURNING is not supported in INSERT"}
	}

	return s, nil
}

func (p *parser) updateStatement() (*Statement, error) {
	s := &Statement{Kind: Update}

	err := p.target(s, false)
	if err != nil {
		return nil, err
	}

	for {
		var done bool

		switch t := p.peek(); {
		case t.is("SET"):
			p.i++
			err = p.setActions(s)
		case t.is("REMOVE"):
			p.i++
			err = p.removeActions(s)
		default:
			done = true
		}

		if err != nil {
			return nil, err
		}

		if done {
			break
		}
	}

	if len(s.Actions) == 0 {
		return nil, errMalformed
	}

	err = p.where(s)
	if err == nil {
		err = p.returning(s)
	}

	if err != nil {
		return nil, err
	}

	return s, nil
}

func (p *parser) setActions(s *Statement) error {
	for {
		path, err := p.path()
		if err == nil {
			err = p.expect("=")
		}

		var v *Node
		if err == nil {
			v, err = p.additive()
		}

		if err != nil {
			return err
		}

		s.Actions = append(s.Actions, Action{Path: path, Value: v})

		if !p.peek().punct(",") {
			return nil
		}

		p.i++
	}
}

func (p *parser) removeActions(s *Statement) error {
	for {
		path, err := p.path()
		if err != nil {
			return err
		}

		s.Actions = append(s.Actions, Action{Path: path})

		if !p.peek().punct(",") {
			return nil
		}

		p.i++
	}
}

func (p *parser) deleteStatement() (*Statement, error) {
	s := &Statement{Kind: Delete}

	err := p.keyword("FROM")
	if err == nil {
		err = p.target(s, false)
	}

	if err == nil {
		err = p.where(s)
	}

	if err == nil {
		err = p.returning(s)
	}

	if err != nil {
		return nil, err
	}

	return s, nil
}

// returning reads an optional RETURNING clause.
func (p *parser) returning(s *Statement) error {
	if !p.peek().is("RETURNING") {
		return nil
	}

	p.i++

	scope, age := p.take(), p.take()

	err := p.expect("*")
	if err != nil {
		return err
	}

	switch {
	case scope.is("ALL") && age.is("OLD"):
		s.Returning = ReturnAllOld
	case scope.is("MODIFIED") && age.is("OLD"):
		s.Returning = ReturnModifiedOld
	case scope.is("ALL") && age.is("NEW"):
		s.Returning = ReturnAllNew
	case scope.is("MODIFIED") && age.is("NEW"):
		s.Returning = ReturnModifiedNew
	default:
		return errMalformed
	}

	if s.Kind == Delete && s.Returning != ReturnAllOld {
		return &Error{Message: "Invalid returning clause: RETURNING " + strings.ToUpper(scope.text) + " " +
			strings.ToUpper(age.text) + " *. Only RETURNING ALL OLD * is allowed in DELETE statements."}
	}

	return nil
}

func (p *parser) existsStatement() (*Statement, error) {
	err := p.expect("(")
	if err != nil {
		return nil, err
	}

	if !p.peek().is("SELECT") {
		return nil, p.existsOperand()
	}

	p.i++

	s, err := p.selectStatement()
	if err == nil {
		err = p.expect(")")
	}

	if err != nil {
		return nil, err
	}

	s.Kind = Exists

	return s, nil
}

// existsOperand reads what follows `EXISTS(` when it is not a SELECT, and
// returns the error for it. The image reads `EXISTS(...)` as a call of a
// function around an expression, so a statement that is one, empty or around
// anything that is not a statement keyword, is an EXISTS it can refuse outside a
// transaction, where an EXISTS around a statement or with something after it is
// not well formed (verification/SQ-470 r2ord-cand.log "e EXG", "e EXISTS()",
// "e EXISTS(UPDATE)", "e EXT").
func (p *parser) existsOperand() error {
	if !p.peek().punct(")") && (p.peek().kind == tokEOF || p.peek().kind == tokIdent && reserved(p.peek().text)) {
		return errMalformed
	}

	for depth := 1; depth > 0; p.i++ {
		switch t := p.peek(); {
		case t.kind == tokEOF:
			return errMalformed
		case t.punct("("):
			depth++
		case t.punct(")"):
			depth--
		default:
		}
	}

	if p.peek().punct(";") {
		p.i++
	}

	if p.peek().kind != tokEOF {
		return errMalformed
	}

	return errExistsExpression
}

// path reads a document path: a name, then .name and [n] steps.
func (p *parser) path() (Path, error) {
	name, ok := p.name()
	if !ok {
		return nil, errMalformed
	}

	path := Path{{Name: name}}

	for {
		switch {
		case p.peek().punct("."):
			p.i++

			if name, ok = p.name(); !ok {
				return nil, errMalformed
			}

			path = append(path, Step{Name: name})
		case p.peek().punct("["):
			p.i++

			t := p.take()

			n, err := strconv.Atoi(t.text)
			if t.kind != tokNumber || err != nil || p.expect("]") != nil {
				return nil, errMalformed
			}

			path = append(path, Step{Index: n, IsIndex: true})
		default:
			return path, nil
		}
	}
}

// reserved reports whether a bare identifier is a keyword of the grammar,
// which only a quoted identifier may spell.
func reserved(s string) bool {
	switch strings.ToUpper(s) {
	case "SELECT", "FROM", "WHERE", "AND", "OR", "NOT", "BETWEEN", "IN", "IS", "NULL", "MISSING", "TRUE",
		"FALSE", "INSERT", "INTO", "VALUE", "UPDATE", "SET", "REMOVE", "DELETE", "RETURNING", "EXISTS", "ORDER",
		"BY":
		return true
	default:
		return false
	}
}
