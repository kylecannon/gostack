package expression

import (
	"strings"
	"unicode/utf8"
)

// updateTokenKind is the kind of one token of an update expression.
type updateTokenKind int

const (
	// updateTokName is a bare word: an attribute name, a clause keyword or a
	// function name.
	updateTokName updateTokenKind = iota + 1
	// updateTokNameRef is a "#name" placeholder.
	updateTokNameRef
	// updateTokValueRef is a ":value" placeholder.
	updateTokValueRef
	// updateTokIndex is the digits of a list index.
	updateTokIndex
	// updateTokPunct is one of the punctuation characters in updatePunct.
	updateTokPunct
	// updateTokInvalid is a character the grammar has no use for.
	updateTokInvalid
)

// updatePunct holds every single-character token the grammar uses.
const updatePunct = ".[],=+-()"

// updateToken is one token and the byte range of its text in the expression.
type updateToken struct {
	kind       updateTokenKind
	text       string
	start, end int
}

// is reports whether t is the punctuation character c.
func (t *updateToken) is(c string) bool {
	return t != nil && t.kind == updateTokPunct && t.text == c
}

// updateLex splits an update expression into tokens. It never fails: a
// character the grammar has no use for becomes an updateTokInvalid token, which
// the parser reports as a syntax error where it meets it, as DynamoDB does.
func updateLex(s string) []updateToken {
	var toks []updateToken

	for i := 0; i < len(s); {
		c := s[i]
		if updateIsSpace(c) {
			i++

			continue
		}

		kind, end := updateScan(s, i)
		toks = append(toks, updateToken{kind: kind, text: s[i:end], start: i, end: end})
		i = end
	}

	return toks
}

// updateScan returns the kind and end of the token that starts at s[i], which
// is not a space.
func updateScan(s string, i int) (updateTokenKind, int) {
	c := s[i]

	switch {
	case updateIsLetter(c):
		return updateTokName, updateScanWord(s, i+1)
	case c == '#' || c == ':':
		end := updateScanWord(s, i+1)
		if end == i+1 {
			return updateTokInvalid, i + 1
		}

		if c == '#' {
			return updateTokNameRef, end
		}

		return updateTokValueRef, end
	case c == '0':
		// An index never has a leading zero: "01" is the index 0 and then 1.
		return updateTokIndex, i + 1
	case c >= '1' && c <= '9':
		end := i + 1
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}

		return updateTokIndex, end
	case strings.IndexByte(updatePunct, c) >= 0:
		return updateTokPunct, i + 1
	default:
		_, size := utf8.DecodeRuneInString(s[i:])

		return updateTokInvalid, i + size
	}
}

// updateScanWord returns the end of the run of word characters from s[i].
func updateScanWord(s string, i int) int {
	for i < len(s) && (updateIsLetter(s[i]) || s[i] == '_' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}

	return i
}

func updateIsLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func updateIsSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
