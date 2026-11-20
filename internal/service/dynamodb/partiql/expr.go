package partiql

import "strings"

// expr reads a condition: OR binds loosest, then AND, then NOT.
func (p *parser) expr() (*Node, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}

	for p.peek().is("OR") {
		p.i++

		right, err := p.and()
		if err != nil {
			return nil, err
		}

		left = &Node{Kind: KindOr, At: left.At, Args: []*Node{left, right}}
	}

	return left, nil
}

func (p *parser) and() (*Node, error) {
	left, err := p.not()
	if err != nil {
		return nil, err
	}

	for p.peek().is("AND") {
		p.i++

		right, err := p.not()
		if err != nil {
			return nil, err
		}

		left = &Node{Kind: KindAnd, At: left.At, Args: []*Node{left, right}}
	}

	return left, nil
}

func (p *parser) not() (*Node, error) {
	if t := p.peek(); t.is("NOT") {
		p.i++

		x, err := p.not()
		if err != nil {
			return nil, err
		}

		return &Node{Kind: KindNot, At: t.at, Args: []*Node{x}}, nil
	}

	return p.predicate()
}

// compareOps are the comparison operators; != is <>.
var compareOps = map[string]string{"=": "=", "<>": "<>", "!=": "<>", "<": "<", "<=": "<=", ">": ">", ">=": ">="}

// predicate reads an operand and what follows it: a comparison, BETWEEN, IN
// or IS, any of the last three negated by NOT.
func (p *parser) predicate() (*Node, error) {
	x, err := p.additive()
	if err != nil {
		return nil, err
	}

	t := p.peek()
	if op, ok := compareOps[t.text]; ok && t.kind == tokPunct {
		p.i++

		var y *Node

		y, err = p.additive()
		if err != nil {
			return nil, err
		}

		return &Node{Kind: KindCompare, At: t.at, Op: op, Args: []*Node{x, y}}, nil
	}

	negated := false
	if t.is("NOT") && (p.peekNext().is("BETWEEN") || p.peekNext().is("IN")) {
		p.i++
		negated = true
	}

	var n *Node

	switch t := p.peek(); {
	case t.is("BETWEEN"):
		n, err = p.between(x)
	case t.is("IN"):
		n, err = p.in(x)
	case t.is("IS") && !negated:
		return p.is(x)
	default:
		if negated {
			return nil, errMalformed
		}

		return x, nil
	}

	if err != nil {
		return nil, err
	}

	if negated {
		n = &Node{Kind: KindNot, At: n.At, Args: []*Node{n}}
	}

	return n, nil
}

func (p *parser) between(x *Node) (*Node, error) {
	at := p.take().at

	lo, err := p.additive()
	if err == nil {
		err = p.keyword("AND")
	}

	var hi *Node
	if err == nil {
		hi, err = p.additive()
	}

	if err != nil {
		return nil, err
	}

	return &Node{Kind: KindBetween, At: at, Args: []*Node{x, lo, hi}}, nil
}

// in reads IN with a list in brackets or parentheses.
func (p *parser) in(x *Node) (*Node, error) {
	at := p.take().at

	closer := "]"
	if p.peek().punct("(") {
		closer = ")"
	} else if !p.peek().punct("[") {
		return nil, errMalformed
	}

	p.i++

	items, err := p.items(closer)
	if err != nil {
		return nil, err
	}

	if len(items) == 0 {
		return nil, errMalformed
	}

	return &Node{Kind: KindIn, At: at, Args: append([]*Node{x}, items...)}, nil
}

func (p *parser) is(x *Node) (*Node, error) {
	at := p.take().at

	n := &Node{At: at, Args: []*Node{x}}
	if p.peek().is("NOT") {
		p.i++
		n.Negated = true
	}

	switch t := p.take(); {
	case t.is("NULL"):
		n.Kind = KindIsNull
	case t.is("MISSING"):
		n.Kind = KindIsMissing
	default:
		return nil, errMalformed
	}

	return n, nil
}

// additive reads operands joined by + and -.
func (p *parser) additive() (*Node, error) {
	left, err := p.primary()
	if err != nil {
		return nil, err
	}

	for {
		t := p.peek()
		if !t.punct("+") && !t.punct("-") {
			return left, nil
		}

		p.i++

		right, err := p.primary()
		if err != nil {
			return nil, err
		}

		left = &Node{Kind: KindArith, At: t.at, Op: t.text, Args: []*Node{left, right}}
	}
}

// primary reads one operand.
func (p *parser) primary() (*Node, error) {
	t := p.peek()

	switch t.kind {
	case tokString:
		p.i++

		return &Node{Kind: KindLiteral, At: t.at, Text: t.text, Literal: Literal{Kind: LitString, Text: t.text}}, nil
	case tokNumber:
		p.i++

		return &Node{Kind: KindLiteral, At: t.at, Text: t.text, Literal: Literal{Kind: LitNumber, Text: t.text}}, nil
	case tokIdent, tokQuoted:
		return p.word(t)
	case tokPunct:
		return p.punctOperand(t)
	default:
		return nil, errMalformed
	}
}

// word reads an operand that starts with a name: a keyword literal, a
// function call, or a path.
func (p *parser) word(t token) (*Node, error) {
	if t.kind == tokIdent {
		switch strings.ToUpper(t.text) {
		case "TRUE", "FALSE":
			p.i++

			return &Node{Kind: KindLiteral, At: t.at, Text: t.text, Literal: Literal{Kind: LitBool, Bool: t.is("TRUE")}}, nil
		case "NULL":
			p.i++

			return &Node{Kind: KindLiteral, At: t.at, Text: t.text, Literal: Literal{Kind: LitNull}}, nil
		case "MISSING":
			p.i++

			return &Node{Kind: KindMissing, At: t.at, Text: t.text}, nil
		case "SELECT":
			return nil, atError("Unsupported token in expression: "+t.text, t.at)
		default:
		}

		if p.peekNext().punct("(") {
			return p.call(t)
		}
	}

	path, err := p.path()
	if err != nil {
		return nil, err
	}

	return &Node{Kind: KindPath, At: t.at, Text: t.text, Path: path}, nil
}

func (p *parser) call(t token) (*Node, error) {
	p.i += 2

	args, err := p.items(")")
	if err != nil {
		return nil, err
	}

	return &Node{Kind: KindCall, At: t.at, Text: t.text, Name: strings.ToLower(t.text), Args: args}, nil
}

// punctOperand reads an operand that starts with punctuation.
func (p *parser) punctOperand(t token) (*Node, error) {
	switch t.text {
	case "(":
		p.i++

		x, err := p.expr()
		if err == nil {
			err = p.expect(")")
		}

		if err != nil {
			return nil, err
		}

		return x, nil
	case "?":
		p.i++
		p.params++

		return &Node{Kind: KindParam, At: t.at, Text: t.text, Param: p.params - 1}, nil
	case "-", "+":
		if n := p.peekNext(); n.kind == tokNumber {
			p.i += 2

			text := n.text
			if t.text == "-" {
				text = "-" + text
			}

			return &Node{Kind: KindLiteral, At: t.at, Text: text, Literal: Literal{Kind: LitNumber, Text: text}}, nil
		}

		return nil, errMalformed
	case "[":
		return p.collection(KindList, "]")
	case "<<":
		return p.collection(KindBag, ">>")
	case "{":
		return p.tuple()
	default:
		return nil, errMalformed
	}
}

func (p *parser) collection(kind Kind, closer string) (*Node, error) {
	at := p.take().at

	items, err := p.items(closer)
	if err != nil {
		return nil, err
	}

	return &Node{Kind: kind, At: at, Args: items}, nil
}

// items reads comma-separated operands up to closer, which it takes.
func (p *parser) items(closer string) ([]*Node, error) {
	var out []*Node

	if p.peek().punct(closer) {
		p.i++

		return out, nil
	}

	for {
		x, err := p.expr()
		if err != nil {
			return nil, err
		}

		out = append(out, x)

		switch t := p.take(); {
		case t.punct(","):
		case t.punct(closer):
			return out, nil
		default:
			return nil, errMalformed
		}
	}
}

// tuple reads {'key': value, ...}; a key is a string.
func (p *parser) tuple() (*Node, error) {
	n := &Node{Kind: KindTuple, At: p.take().at}

	if p.peek().punct("}") {
		p.i++

		return n, nil
	}

	for {
		k := p.take()
		if k.kind != tokString || p.expect(":") != nil {
			return nil, errMalformed
		}

		v, err := p.expr()
		if err != nil {
			return nil, err
		}

		n.Keys = append(n.Keys, k.text)
		n.KeyAt = append(n.KeyAt, k.at)
		n.Args = append(n.Args, v)

		switch t := p.take(); {
		case t.punct(","):
		case t.punct("}"):
			return n, nil
		default:
			return nil, errMalformed
		}
	}
}
