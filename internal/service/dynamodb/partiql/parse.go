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

	// A set quantifier: ALL is the default and changes nothing, where DISTINCT
	// parses and is refused later (Statement.Distinct).
	switch {
	case p.peek().is("DISTINCT"):
		p.i++

		s.Distinct = true
	case p.peek().is("ALL"):
		p.i++
	default:
	}

	if p.peek().punct("*") {
		p.i++
	} else {
		for {
			path, err := p.projectedPath()
			if err != nil {
				return nil, err
			}

			s.Projection = append(s.Projection, path)

			err = p.aliased(s)
			if err != nil {
				return nil, err
			}

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
		err = p.tableAliased(s)
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

// alias takes the alias after a projected path or a table, `AS name` or a bare
// name, and reports whether there was one. The image parses an alias in a
// SELECT, UPDATE and DELETE and refuses it later; `AS` with no name after it
// is not well formed.
func (p *parser) alias() (bool, error) {
	as := p.peek().is("AS")
	if as {
		p.i++
	}

	_, ok := p.name()
	if as && !ok {
		return false, errMalformed
	}

	return ok, nil
}

// tableAliased reads an alias after a table, and marks s with it.
func (p *parser) tableAliased(s *Statement) error {
	ok, err := p.alias()
	if ok {
		s.Aliased, s.TableAliased = true, true
	}

	return err
}

// aliased reads an alias after a projected path, and marks s with it.
func (p *parser) aliased(s *Statement) error {
	ok, err := p.alias()
	if ok {
		s.Aliased = true
	}

	return err
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
	if err == nil {
		err = p.tableAliased(s)
	}

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

// removeActions reads the one path a REMOVE names. The image refuses a comma
// after it as not well formed (`REMOVE a, b`), where SET takes a list: a second
// path takes a second REMOVE.
func (p *parser) removeActions(s *Statement) error {
	path, err := p.path()
	if err != nil {
		return err
	}

	s.Actions = append(s.Actions, Action{Path: path})

	return nil
}

func (p *parser) deleteStatement() (*Statement, error) {
	s := &Statement{Kind: Delete}

	err := p.keyword("FROM")
	if err == nil {
		err = p.target(s, false)
	}

	if err == nil {
		err = p.tableAliased(s)
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
// anything but UPDATE, DELETE or FROM, is an EXISTS it can refuse outside a
// transaction (NULL, TRUE, NOT a, INSERT INTO ... and a name all are), where an
// EXISTS around one of those, or with something after it, is not well formed
// (verification/SQ-439 round3 exists-cand.log, 78 statements probed).
func (p *parser) existsOperand() error {
	first := p.peek()
	if first.kind == tokEOF || first.is("UPDATE") || first.is("DELETE") || first.is("FROM") ||
		first.is("WHERE") || first.is("ORDER") || first.is("RETURNING") || first.punct(",") {
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

			step, err := p.bracketStep()
			if err != nil {
				return nil, err
			}

			path = append(path, step)
		default:
			return path, nil
		}
	}
}

// MsgAliasing is the refusal of a statement that names an alias.
const MsgAliasing = "Aliasing is not supported"

// MsgDistinct is the refusal of a SELECT DISTINCT.
const MsgDistinct = "Unsupported token in expression: DISTINCT"

// bracketStep reads what follows a `[` of a path: a list index, or a name in
// quotes or bare, which the image reads as the step `.name` (`m['x']` is `m.x`).
// A step with no name, `[”]`, is read as one: the image refuses it where the
// path is put to use, with the statement's other expressions (Path.Empty).
func (p *parser) bracketStep() (Step, error) {
	t := p.take()

	var step Step

	switch {
	case t.kind == tokNumber:
		n, err := strconv.Atoi(t.text)
		if err != nil {
			return Step{}, errMalformed
		}

		step = Step{Index: n, IsIndex: true}
	case t.kind == tokString || t.kind == tokQuoted || (t.kind == tokIdent && !reserved(t.text)):
		step = Step{Name: t.text}
	default:
		return Step{}, errMalformed
	}

	if p.expect("]") != nil {
		return Step{}, errMalformed
	}

	return step, nil
}

// reservedWords are the keywords of the grammar, which only a quoted identifier
// may spell. The last ten are the image's: it answers each, as an attribute
// name, a table name or an alias, as not well formed (SQ-418 p2b, p2c). The
// image reserves others of the PartiQL grammar that were not probed.
var reservedWords = [...]string{
	"SELECT", "FROM", "WHERE", "AND", "OR", "NOT", "BETWEEN", "IN", "IS", "NULL", "MISSING", "TRUE",
	"FALSE", "INSERT", "INTO", "VALUE", "UPDATE", "SET", "REMOVE", "DELETE", "RETURNING", "EXISTS", "ORDER",
	"BY", "ALL", "DISTINCT", "AS", "ASC", "DESC", "LIMIT", "MODIFIED", "OLD", "NEW", "ONLY", "PIVOT", "UNPIVOT",
}

// reserved reports whether a bare identifier is a keyword of the grammar. It
// compares without a case-folded copy, so the check costs no allocation.
func reserved(s string) bool {
	for _, w := range reservedWords {
		if len(w) == len(s) && strings.EqualFold(w, s) {
			return true
		}
	}

	return false
}
