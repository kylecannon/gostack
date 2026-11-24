package dynamodb

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The item reader of the DynamoDB snapshot, for an item value.JSONPacker
// leaves to it. Items are most of a snapshot, so it skips json v2's
// reflection: itemParser reads an item from the raw value the decoder has
// already checked for syntax. value.Orderer writes them.

// maxInterned bounds the attribute names one restore shares between items.
const maxInterned = 4096

// itemParser reads items out of raw json values that jsontext has checked for
// syntax and duplicate names. One parser serves a whole restore: it shares
// attribute names between items, so a million items with the same four names
// hold four strings, and reuses its scratch.
type itemParser struct {
	b       []byte
	i       int
	names   map[string]string
	scratch []byte
}

func newItemParser() *itemParser {
	return &itemParser{names: map[string]string{}}
}

// parse reads one item. Each string, slice and map it returns is its own:
// nothing aliases raw or another item.
func (p *itemParser) parse(raw jsontext.Value) (value.Item, error) {
	p.b, p.i = raw, 0

	item, err := p.object()
	p.b = nil

	return item, err
}

func (p *itemParser) fail(what string) error {
	return fmt.Errorf("%w: an item has %s at offset %d", errSnapshot, what, p.i)
}

func (p *itemParser) space() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

// expect skips space and then c, which must be next.
func (p *itemParser) expect(c byte) error {
	p.space()

	if p.i >= len(p.b) || p.b[p.i] != c {
		return p.fail(fmt.Sprintf("no %q", c))
	}

	p.i++

	return nil
}

// next skips space and reports whether c is next, taking it if so.
func (p *itemParser) next(c byte) bool {
	p.space()

	if p.i < len(p.b) && p.b[p.i] == c {
		p.i++

		return true
	}

	return false
}

// object reads a map of attribute values.
func (p *itemParser) object() (value.Item, error) {
	err := p.expect('{')
	if err != nil {
		return nil, err
	}

	item := value.Item{}

	if p.next('}') {
		return item, nil
	}

	for {
		name, err := p.name()
		if err != nil {
			return nil, err
		}

		if _, twice := item[name]; twice {
			return nil, p.fail(fmt.Sprintf("the name %q twice", name))
		}

		err = p.expect(':')
		if err != nil {
			return nil, err
		}

		v, err := p.value()
		if err != nil {
			return nil, err
		}

		item[name] = v

		if p.next('}') {
			return item, nil
		}

		err = p.expect(',')
		if err != nil {
			return nil, err
		}
	}
}

// name reads an attribute name, shared with every earlier item that has it.
// The names map is keyed by the decoded name only: a name without escapes is
// looked up by its bytes between the quotes, which are the decoded name, and
// one with escapes is decoded first.
func (p *itemParser) name() (string, error) {
	raw, err := p.quoted()
	if err != nil {
		return "", err
	}

	inner := raw[1 : len(raw)-1]
	if !slices.Contains(inner, '\\') && utf8.Valid(inner) {
		if s, ok := p.names[string(inner)]; ok {
			return s, nil
		}
	}

	s := p.unquote(raw)
	if shared, ok := p.names[s]; ok {
		return shared, nil
	}

	if len(p.names) < maxInterned {
		p.names[s] = s
	}

	return s, nil
}

// value reads one attribute value object.
func (p *itemParser) value() (api.AttributeValue, error) {
	var v api.AttributeValue

	err := p.expect('{')
	if err != nil {
		return v, err
	}

	if p.next('}') {
		return v, nil
	}

	for {
		raw, err := p.quoted()
		if err != nil {
			return v, err
		}

		name := raw[1 : len(raw)-1]
		if slices.Contains(name, '\\') {
			name = []byte(p.unquote(raw))
		}

		err = p.expect(':')
		if err != nil {
			return v, err
		}

		err = p.member(name, &v)
		if err != nil {
			return v, err
		}

		if p.next('}') {
			return v, nil
		}

		err = p.expect(',')
		if err != nil {
			return v, err
		}
	}
}

// member reads the member name of v.
func (p *itemParser) member(name []byte, v *api.AttributeValue) error {
	var err error

	switch string(name) {
	case "S":
		v.S, err = p.stringPtr()
	case "N":
		v.N, err = p.stringPtr()
	case "B":
		v.B, err = p.bytes()
	case "SS":
		v.SS, err = p.strings()
	case "NS":
		v.NS, err = p.strings()
	case "BS":
		v.BS, err = p.byteLists()
	case "M":
		v.M, err = p.object()
	case "L":
		v.L, err = p.list()
	case "NULL":
		v.NULL, err = p.boolPtr()
	case "BOOL":
		v.BOOL, err = p.boolPtr()
	default:
		err = p.fail(fmt.Sprintf("the member %q", name))
	}

	return err
}

// quoted reads a json string and returns it with its quotes, escapes as they
// are.
func (p *itemParser) quoted() ([]byte, error) {
	p.space()

	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return nil, p.fail("no string")
	}

	start := p.i
	p.i++

	// The next quote ends the string unless an escape comes before it. Each
	// byte is searched once for each of the two.
	end := -1

	for p.i < len(p.b) {
		if end < p.i {
			next := bytes.IndexByte(p.b[p.i:], '"')
			if next < 0 {
				break
			}

			end = p.i + next
		}

		escape := bytes.IndexByte(p.b[p.i:end], '\\')
		if escape < 0 {
			p.i = end + 1

			return p.b[start:p.i], nil
		}

		p.i += escape + len(`\"`) // the backslash and the byte it escapes
	}

	return nil, p.fail("an unterminated string")
}

// unquote decodes a quoted string. Invalid UTF-8 becomes U+FFFD, as json v2
// reads it under AllowInvalidUTF8.
func (p *itemParser) unquote(raw []byte) string {
	inner := raw[1 : len(raw)-1]
	if !slices.Contains(inner, '\\') && utf8.Valid(inner) {
		return string(inner)
	}

	p.scratch, _ = jsontext.AppendUnquote(p.scratch[:0], raw)

	return string(p.scratch)
}

func (p *itemParser) string() (string, error) {
	raw, err := p.quoted()
	if err != nil {
		return "", err
	}

	return p.unquote(raw), nil
}

func (p *itemParser) stringPtr() (*string, error) {
	s, err := p.string()
	if err != nil {
		return nil, err
	}

	return &s, nil
}

func (p *itemParser) bytes() ([]byte, error) {
	raw, err := p.quoted()
	if err != nil {
		return nil, err
	}

	inner := raw[1 : len(raw)-1]
	if slices.Contains(inner, '\\') {
		p.scratch, _ = jsontext.AppendUnquote(p.scratch[:0], raw)
		inner = p.scratch
	}

	out := make([]byte, 0, base64.StdEncoding.DecodedLen(len(inner)))

	out, err = base64.StdEncoding.AppendDecode(out, inner)
	if err != nil {
		return nil, p.fail("binary that is not base64")
	}

	return out, nil
}

// array reads a json array, calling each for every element.
func (p *itemParser) array(each func() error) error {
	err := p.expect('[')
	if err != nil {
		return err
	}

	if p.next(']') {
		return nil
	}

	for {
		err = each()
		if err != nil {
			return err
		}

		if p.next(']') {
			return nil
		}

		err = p.expect(',')
		if err != nil {
			return err
		}
	}
}

func (p *itemParser) strings() ([]string, error) {
	out := []string{}

	err := p.array(func() error {
		s, err := p.string()
		out = append(out, s)

		return err
	})

	return out, err
}

func (p *itemParser) byteLists() ([][]byte, error) {
	out := [][]byte{}

	err := p.array(func() error {
		b, err := p.bytes()
		out = append(out, b)

		return err
	})

	return out, err
}

func (p *itemParser) list() ([]api.AttributeValue, error) {
	out := []api.AttributeValue{}

	err := p.array(func() error {
		v, err := p.value()
		out = append(out, v)

		return err
	})

	return out, err
}

func (p *itemParser) boolPtr() (*bool, error) {
	p.space()

	var b bool

	switch {
	case p.has("true"):
		b = true
	case p.has("false"):
	default:
		return nil, p.fail("no boolean")
	}

	return &b, nil
}

// has takes word if it is next.
func (p *itemParser) has(word string) bool {
	if len(p.b)-p.i >= len(word) && string(p.b[p.i:p.i+len(word)]) == word {
		p.i += len(word)

		return true
	}

	return false
}
