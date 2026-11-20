package filter

import (
	"encoding/json/jsontext"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

// kind is the Python type json.loads gives a JSON value.
type kind uint8

const (
	kindNull kind = iota + 1
	kindBool
	kindInt
	kindFloat
	kindString
	kindList
	kindDict
)

// value is a decoded JSON value as Python holds it. Objects keep their members
// in insertion order with a repeated name keeping its first position and its
// last value, as a Python dict built by json.loads does: the first error the
// validator reports and the key it names both depend on that order.
type value struct {
	kind kind
	b    bool
	// numberText marks a string that is a Number attribute's value, which
	// equals a number in an anything-but rule by value.
	numberText bool
	// num is the value of an int or a float. An int past 2**53 is rounded, so
	// it compares with a float the way float64 does, not exactly as Python.
	num float64
	// str is a string's text, or an int's decimal digits as Python prints them.
	str  string
	list []value
	dict []member
}

type member struct {
	key string
	val value
}

var null = &value{kind: kindNull}

// indexAfter is the member count past which decoding an object looks names up
// in a map instead of scanning the members.
const indexAfter = 16

// decodeOptions read JSON as Python's json.loads does, as far as jsontext
// allows: a repeated name is not an error, and a lone surrogate escape is read
// rather than refused. NaN and Infinity, which json.loads also takes, stay
// errors.
var decodeOptions = []jsontext.Options{
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
}

// decode reads one JSON value from s and nothing after it; ok is false for
// anything else.
func decode(s string) (value, bool) {
	dec := jsontext.NewDecoder(strings.NewReader(s), decodeOptions...)
	v, ok := decodeValue(dec)
	if !ok {
		return value{}, false
	}
	_, err := dec.ReadToken()
	return v, errors.Is(err, io.EOF)
}

func decodeValue(dec *jsontext.Decoder) (value, bool) {
	tok, err := dec.ReadToken()
	if err != nil {
		return value{}, false
	}
	switch tok.Kind() {
	case 'n':
		return value{kind: kindNull}, true
	case 't', 'f':
		return value{kind: kindBool, b: tok.Bool()}, true
	case '"':
		return value{kind: kindString, str: tok.String()}, true
	case '0':
		return number(tok.String()), true
	case '[':
		return decodeList(dec)
	case '{':
		return decodeDict(dec)
	default:
		return value{}, false
	}
}

func decodeList(dec *jsontext.Decoder) (value, bool) {
	var list []value
	for dec.PeekKind() != ']' {
		v, ok := decodeValue(dec)
		if !ok {
			return value{}, false
		}
		list = append(list, v)
	}
	_, err := dec.ReadToken()
	return value{kind: kindList, list: list}, err == nil
}

func decodeDict(dec *jsontext.Decoder) (value, bool) {
	var dict []member
	var index map[string]int // past indexAfter members, so a large object is not quadratic
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return value{}, false
		}
		key := name.String() // the token is void after the next read
		v, ok := decodeValue(dec)
		if !ok {
			return value{}, false
		}
		if index == nil && len(dict) < indexAfter {
			dict = setMember(dict, key, v)
			continue
		}
		if index == nil {
			index = make(map[string]int, len(dict))
			for i := range dict {
				index[dict[i].key] = i
			}
		}
		if i, found := index[key]; found {
			dict[i].val = v
			continue
		}
		index[key] = len(dict)
		dict = append(dict, member{key: key, val: v})
	}
	_, err := dec.ReadToken()
	return value{kind: kindDict, dict: dict}, err == nil
}

// setMember sets key in dict the way assignment to a Python dict does.
func setMember(dict []member, key string, v value) []member {
	for i := range dict {
		if dict[i].key == key {
			dict[i].val = v
			return dict
		}
	}
	return append(dict, member{key: key, val: v})
}

// number reads a JSON number literal as json.loads does: an int when it has
// no fraction and no exponent, a float otherwise. A float too large is
// infinite, as Python's float() makes it.
func number(lit string) value {
	f, _ := strconv.ParseFloat(lit, 64)
	if strings.ContainsAny(lit, ".eE") {
		return value{kind: kindFloat, num: f}
	}
	if lit == "-0" {
		lit = "0"
	}
	return value{kind: kindInt, num: f, str: lit}
}

// get returns the value of key in a dict.
func (v *value) get(key string) (*value, bool) {
	for i := range v.dict {
		if v.dict[i].key == key {
			return &v.dict[i].val, true
		}
	}
	return nil, false
}

// truthy reports Python's bool(v).
func (v *value) truthy() bool {
	switch v.kind {
	case kindBool:
		return v.b
	case kindInt, kindFloat:
		return v.num != 0
	case kindString:
		return v.str != ""
	case kindList:
		return len(v.list) > 0
	case kindDict:
		return len(v.dict) > 0
	default:
		return false
	}
}

// numeric reports whether isinstance(v, (int, float)) holds, which it does for
// a bool too, and returns v as a float.
func (v *value) numeric() (float64, bool) {
	switch v.kind {
	case kindBool:
		if v.b {
			return 1, true
		}
		return 0, true
	case kindInt, kindFloat:
		return v.num, true
	default:
		return 0, false
	}
}

// equal reports Python's a == b for decoded JSON values: numbers and bools
// compare by value, and lists and dicts compare member by member, dicts
// without regard to order. The text of a Number attribute equals an int or a
// float of the same value, as AWS compares them for anything-but.
func equal(a, b *value) bool {
	if a.numberText && (b.kind == kindInt || b.kind == kindFloat) {
		f, ok := pyFloat(a)
		return ok && f == b.num
	}
	if an, ok := a.numeric(); ok {
		bn, ok := b.numeric()
		if !ok {
			return false
		}
		if a.kind == kindInt && b.kind == kindInt {
			return a.str == b.str
		}
		return an == bn
	}
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case kindNull:
		return true
	case kindString:
		return a.str == b.str
	case kindList:
		return equalLists(a.list, b.list)
	case kindDict:
		return equalDicts(a, b)
	default:
		return false
	}
}

func equalLists(a, b []value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equal(&a[i], &b[i]) {
			return false
		}
	}
	return true
}

func equalDicts(a, b *value) bool {
	if len(a.dict) != len(b.dict) {
		return false
	}
	for i := range a.dict {
		bv, ok := b.get(a.dict[i].key)
		if !ok || !equal(&a.dict[i].val, bv) {
			return false
		}
	}
	return true
}

// pyStr returns str(v) as Python formats it, for the messages that quote a
// member of the policy.
func pyStr(v *value) string {
	if v.kind == kindString {
		return v.str
	}
	var sb strings.Builder
	writeRepr(&sb, v)
	return sb.String()
}

func writeRepr(sb *strings.Builder, v *value) {
	switch v.kind {
	case kindBool:
		if v.b {
			sb.WriteString("True")
		} else {
			sb.WriteString("False")
		}
	case kindInt:
		sb.WriteString(v.str)
	case kindFloat:
		sb.WriteString(floatRepr(v.num))
	case kindString:
		sb.WriteString(strRepr(v.str))
	case kindList:
		sb.WriteByte('[')
		for i := range v.list {
			if i > 0 {
				sb.WriteString(", ")
			}
			writeRepr(sb, &v.list[i])
		}
		sb.WriteByte(']')
	case kindDict:
		sb.WriteByte('{')
		for i := range v.dict {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(strRepr(v.dict[i].key))
			sb.WriteString(": ")
			writeRepr(sb, &v.dict[i].val)
		}
		sb.WriteByte('}')
	default:
		sb.WriteString("None")
	}
}

// floatRepr formats f as Python's repr does: the shortest digits that round
// trip, positional from 1e-4 up to 1e16 with ".0" on a whole number, and in
// exponent form otherwise, where Go and Python both write a sign and at least
// two exponent digits.
func floatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	_, expText, _ := strings.Cut(sci, "e")
	if exp, _ := strconv.Atoi(expText); exp < -4 || exp >= 16 {
		return sci
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// strRepr quotes s as Python's repr does for the characters a policy holds in
// practice: single quotes unless s has a single quote and no double quote,
// and backslash escapes for the quote, the backslash and control characters.
func strRepr(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var sb strings.Builder
	sb.WriteRune(quote)
	for _, r := range s {
		switch {
		case r == quote || r == '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			sb.WriteString(`\x`)
			sb.WriteString(strconv.FormatInt(int64(r)>>4, 16))
			sb.WriteString(strconv.FormatInt(int64(r)&0xf, 16))
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteRune(quote)
	return sb.String()
}
