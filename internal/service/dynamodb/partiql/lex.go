package partiql

import (
	"strconv"
	"strings"
)

// tokKind is the kind of a token.
type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokQuoted // a "double-quoted" identifier
	tokString
	tokNumber
	tokPunct
)

// Pos is where a token stands in the statement: its line and column, from 1,
// and its length, as DynamoDB Local's messages give them ("at 1:35:4").
type Pos struct {
	Line, Col, Len int
}

// String is the position as DynamoDB Local writes it.
func (p Pos) String() string {
	return strconv.Itoa(p.Line) + ":" + strconv.Itoa(p.Col) + ":" + strconv.Itoa(p.Len)
}

// token is one lexed token. text is the identifier or string with its quotes
// removed, the number as written, or the punctuation.
type token struct {
	kind tokKind
	text string
	at   Pos
}

// is reports whether t is the keyword kw, which is upper case.
func (t token) is(kw string) bool {
	return t.kind == tokIdent && strings.EqualFold(t.text, kw)
}

// punct reports whether t is the punctuation p.
func (t token) punct(p string) bool {
	return t.kind == tokPunct && t.text == p
}

// The punctuation, longest first so "<=" is not read as "<".
var puncts = []string{
	"<<", ">>", "<=", ">=", "<>", "!=",
	"(", ")", "[", "]", "{", "}", ",", ".", ":", ";", "*", "?", "=", "<", ">", "+", "-",
}

// lexer splits a statement into tokens.
type lexer struct {
	src       string
	off       int
	line, col int
}

// lex returns the tokens of src, ending with tokEOF, or false when src holds
// a character no token starts with or an unterminated quote.
func lex(src string) ([]token, bool) {
	l := &lexer{src: src, line: 1, col: 1}

	var out []token

	for {
		if !l.skipSpace() {
			return nil, false
		}

		if l.off >= len(l.src) {
			return append(out, token{kind: tokEOF, at: Pos{Line: l.line, Col: l.col}}), true
		}

		t, ok := l.next()
		if !ok {
			return nil, false
		}

		out = append(out, t)
	}
}

// advance moves past n bytes of one line.
func (l *lexer) advance(n int) {
	l.off += n
	l.col += n
}

// skipSpace moves past white space and comments: a `--` comment to the end of
// its line, and a `/* ... */` comment, which ends at the first `*/` and does not
// nest (verification/SQ-418 c6). It reports false for a block comment that is
// never closed.
func (l *lexer) skipSpace() bool {
	for l.off < len(l.src) {
		switch c := l.src[l.off]; {
		case c == '\n':
			l.off++
			l.line++
			l.col = 1
		case c == ' ' || c == '\t' || c == '\r':
			l.advance(1)
		case strings.HasPrefix(l.src[l.off:], "--"):
			end := strings.IndexByte(l.src[l.off:], '\n')
			if end < 0 {
				end = len(l.src) - l.off
			}

			l.advanceText(l.src[l.off : l.off+end])
		case strings.HasPrefix(l.src[l.off:], "/*"):
			end := strings.Index(l.src[l.off+2:], "*/")
			if end < 0 {
				return false
			}

			l.advanceText(l.src[l.off : l.off+2+end+2])
		default:
			return true
		}
	}

	return true
}

func (l *lexer) next() (token, bool) {
	start := Pos{Line: l.line, Col: l.col}
	c := l.src[l.off]

	switch {
	case c == '\'' || c == '"':
		text, n, ok := quoted(l.src[l.off:], c)
		if !ok {
			return token{}, false
		}

		kind := tokString
		if c == '"' {
			kind = tokQuoted
		}

		start.Len = n
		l.advanceText(l.src[l.off : l.off+n])

		return token{kind: kind, text: text, at: start}, true
	case isDigit(c):
		n := numberLen(l.src[l.off:])
		start.Len = n
		t := token{kind: tokNumber, text: l.src[l.off : l.off+n], at: start}
		l.advance(n)

		return t, true
	case isIdentStart(c):
		n := 1
		for n < len(l.src)-l.off && isIdentPart(l.src[l.off+n]) {
			n++
		}

		start.Len = n
		t := token{kind: tokIdent, text: l.src[l.off : l.off+n], at: start}
		l.advance(n)

		return t, true
	default:
		for _, p := range puncts {
			if strings.HasPrefix(l.src[l.off:], p) {
				start.Len = len(p)
				l.advance(len(p))

				return token{kind: tokPunct, text: p, at: start}, true
			}
		}

		return token{}, false
	}
}

// advanceText moves past text, which may span lines. A column counts the
// characters before it, as the image's does, where a byte offset would count an
// "é" as two (verification/SQ-418 diff.txt).
func (l *lexer) advanceText(text string) {
	for _, r := range text {
		if r == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
	}

	l.off += len(text)
}

// quoted reads the quoted text at the start of s, where a doubled quote stands
// for one, and returns it unquoted with the length it took.
func quoted(s string, q byte) (string, int, bool) {
	var b strings.Builder

	for i := 1; i < len(s); i++ {
		if s[i] != q {
			b.WriteByte(s[i])

			continue
		}

		if i+1 < len(s) && s[i+1] == q {
			b.WriteByte(q)
			i++

			continue
		}

		return b.String(), i + 1, true
	}

	return "", 0, false
}

// numberLen is the length of the number at the start of s: digits, a
// fraction, and an exponent.
func numberLen(s string) int {
	n := digits(s, 0)
	if n < len(s) && s[n] == '.' && n+1 < len(s) && isDigit(s[n+1]) {
		n = digits(s, n+1)
	}

	if n < len(s) && (s[n] == 'e' || s[n] == 'E') {
		m := n + 1
		if m < len(s) && (s[m] == '+' || s[m] == '-') {
			m++
		}

		if m < len(s) && isDigit(s[m]) {
			n = digits(s, m)
		}
	}

	return n
}

func digits(s string, i int) int {
	for i < len(s) && isDigit(s[i]) {
		i++
	}

	return i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }
