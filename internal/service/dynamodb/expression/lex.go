package expression

import (
	"strings"
	"unicode/utf8"
)

// tokKind is the kind of one lexical token of an expression.
type tokKind int

// The token kinds. tokOther is any character the grammar has no use for; the
// parser refuses it as a syntax error naming it.
const (
	tokEOF tokKind = iota
	tokIdent
	tokName  // #name placeholder
	tokValue // :value placeholder
	tokIndex // a list index: 0, or digits not starting with 0
	tokKeyword
	tokEQ
	tokNE
	tokLT
	tokLE
	tokGT
	tokGE
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
	tokDot
	tokPlus
	tokMinus
	tokOther
)

// token is one token: its kind, its byte span in the expression and the text
// of that span.
type token struct {
	kind       tokKind
	start, end int
	text       string
}

// The keywords of the expression grammars. They are case-insensitive and are
// never an attribute name; DynamoDB refuses one in a path as a syntax error.
var keywords = func() map[string]struct{} {
	m := map[string]struct{}{}
	for w := range strings.FieldsSeq("AND OR NOT BETWEEN IN SET REMOVE ADD DELETE") {
		m[w] = struct{}{}
	}

	return m
}()

var punctuation = map[byte]tokKind{
	'=': tokEQ, '(': tokLParen, ')': tokRParen, '[': tokLBracket, ']': tokRBracket,
	',': tokComma, '.': tokDot, '+': tokPlus, '-': tokMinus,
}

// lex splits src into tokens, ending with one tokEOF at len(src). It is the one
// tokenizer of every expression kind: DynamoDB Local reads update expressions
// with the same lexer as conditions, so AND and <= are single tokens there too
// (probed). It never fails: a character the grammar does not use becomes a tokOther the parser
// reports.
func lex(src string) []token {
	toks := make([]token, 0, len(src)/2+1)

	for i := 0; i < len(src); {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++

			continue
		}

		t := lexOne(src, i)
		t.text = src[t.start:t.end]
		toks = append(toks, t)
		i = t.end
	}

	return append(toks, token{kind: tokEOF, start: len(src), end: len(src)})
}

// lexOne returns the token that starts at src[i], which is not whitespace.
func lexOne(src string, i int) token {
	c := src[i]

	switch {
	case isLetter(c):
		end := scanWord(src, i+1)

		kind := tokIdent
		if _, ok := keywords[strings.ToUpper(src[i:end])]; ok {
			kind = tokKeyword
		}

		return token{kind: kind, start: i, end: end}
	case c == '#' || c == ':':
		end := scanWord(src, i+1)
		if end == i+1 {
			return token{kind: tokOther, start: i, end: i + 1}
		}

		kind := tokName
		if c == ':' {
			kind = tokValue
		}

		return token{kind: kind, start: i, end: end}
	case c == '0':
		return token{kind: tokIndex, start: i, end: i + 1}
	case c >= '1' && c <= '9':
		end := i + 1
		for end < len(src) && isDigit(src[end]) {
			end++
		}

		return token{kind: tokIndex, start: i, end: end}
	case c == '<' || c == '>':
		return lexAngle(src, i)
	}

	if kind, ok := punctuation[c]; ok {
		return token{kind: kind, start: i, end: i + 1}
	}

	_, size := utf8.DecodeRuneInString(src[i:])

	return token{kind: tokOther, start: i, end: i + size}
}

// twoChars is the length of the two-character comparators.
const twoChars = 2

// lexAngle reads <, <=, <>, > or >= at src[i].
func lexAngle(src string, i int) token {
	next := byte(0)
	if i+1 < len(src) {
		next = src[i+1]
	}

	switch {
	case src[i] == '<' && next == '>':
		return token{kind: tokNE, start: i, end: i + twoChars}
	case src[i] == '<' && next == '=':
		return token{kind: tokLE, start: i, end: i + twoChars}
	case src[i] == '<':
		return token{kind: tokLT, start: i, end: i + 1}
	case next == '=':
		return token{kind: tokGE, start: i, end: i + twoChars}
	default:
		return token{kind: tokGT, start: i, end: i + 1}
	}
}

// scanWord returns the end of the run of letters, digits and underscores that
// starts at src[i].
func scanWord(src string, i int) int {
	for i < len(src) && (isLetter(src[i]) || isDigit(src[i]) || src[i] == '_') {
		i++
	}

	return i
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// validPlaceholder reports whether key is a well-formed placeholder: prefix
// followed by one or more letters, digits or underscores.
func validPlaceholder(key string, prefix byte) bool {
	return len(key) > 1 && key[0] == prefix && scanWord(key, 1) == len(key)
}
