package restxml

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
)

// MaxDepth is how deeply elements may nest in a request body. The deepest S3
// request nests eight levels; past MaxDepth the Decoder stops with an error.
const MaxDepth = 64

const (
	int32Bits   = 32
	int64Bits   = 64
	decimalBase = 10
)

// MalformedXMLMessage is AWS's message for the MalformedXML error a rest-xml
// service answers a request body that does not parse or does not match the
// model.
const MalformedXMLMessage = "The XML you provided was not well-formed or did not validate against our published schema"

// ErrMalformed marks every error a Decoder reports: a body that is not
// well-formed XML, nests past MaxDepth, or holds a value of the wrong type.
var ErrMalformed = errors.New("malformed xml")

// Decoder reads a request body one element at a time, over
// xml.Decoder.RawToken, so nothing is buffered beyond the current token.
// Elements and attributes are matched by local name: a namespace prefix, and
// the xmlns declarations, are ignored, so the namespaced and the bare form of a
// body read alike.
//
// Root enters the root element and Next enters each child of the current
// element in turn. An entered element is consumed by reading its value
// (String, Int32, ...), by Skip, or by a loop of Next calls that ends when Next
// reports false. The first error stops decoding: from then on Root and Next
// report false and the value methods nil, and Err returns it.
type Decoder struct {
	xd     *xml.Decoder
	stack  []xml.Name // the open elements, to check each end tag
	attrs  []xml.Attr // the attributes of the element entered last
	err    error
	cost   int64 // what decoding has charged so far (Charge)
	factor int   // the service's decode factor (protocol.DecodeBudgetAt)

	hold    protocol.DecodeHolder // what cost past covered is charged to, or nil
	covered int64                 // what cost may reach before hold is asked again
}

// NewDecoder returns a Decoder that reads r, at protocol.DecodeFactor.
func NewDecoder(r io.Reader) *Decoder {
	return NewDecoderAt(r, 0)
}

// NewDecoderAt returns a Decoder that reads r and holds what it decodes to
// protocol.DecodeBudgetAt of the bytes read so far and factor (budget.go).
func NewDecoderAt(r io.Reader, factor int) *Decoder {
	return NewDecoderFor(r, factor, nil)
}

// NewDecoderFor is NewDecoderAt for a call whose decoding is charged to hold
// (request.Context) once it passes protocol.FreeDecode. When hold refuses,
// decoding stops with an error that wraps protocol.ErrBusy. A nil hold charges
// nothing.
func NewDecoderFor(r io.Reader, factor int, hold protocol.DecodeHolder) *Decoder {
	covered := int64(math.MaxInt64)
	if hold != nil {
		covered = protocol.FreeDecode
	}

	return &Decoder{xd: xml.NewDecoder(&tagLimit{r: r}), factor: factor, hold: hold, covered: covered}
}

// Err returns the first error met so far, or nil.
func (d *Decoder) Err() error {
	return d.err
}

// Root enters the root element and returns its local name. It reports false,
// with no error, for a body that holds no element at all.
func (d *Decoder) Root() (string, bool) {
	if d.err != nil || len(d.stack) > 0 {
		return "", false
	}

	for {
		tok, err := d.xd.RawToken()
		if errors.Is(err, io.EOF) {
			return "", false
		}

		if err != nil {
			d.fail(err)

			return "", false
		}

		if d.chargeToken(tok); d.err != nil {
			return "", false
		}

		if start, ok := tok.(xml.StartElement); ok {
			return d.enter(start)
		}

		if _, ok := tok.(xml.EndElement); ok {
			d.fail(errors.New("end tag before the root element"))

			return "", false
		}
	}
}

// Next enters the next child of the current element and returns its local
// name. At the current element's end tag it consumes the tag and reports
// false.
func (d *Decoder) Next() (string, bool) {
	if d.err != nil || len(d.stack) == 0 {
		return "", false
	}

	for {
		tok, ok := d.token()
		if !ok {
			return "", false
		}

		switch t := tok.(type) {
		case xml.StartElement:
			return d.enter(t)
		case xml.EndElement:
			d.leave(t)

			return "", false
		default:
			// Text between child elements, comments and processing
			// instructions carry no member.
		}
	}
}

// Attr returns the attribute called name of the element entered last. A
// prefixed name such as "xsi:type" matches by its local part.
func (d *Decoder) Attr(name string) (string, bool) {
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}

	for _, a := range d.attrs {
		if a.Name.Local == name && a.Name.Space != "xmlns" && (a.Name.Space != "" || a.Name.Local != "xmlns") {
			return a.Value, true
		}
	}

	return "", false
}

// Skip consumes the element entered last, with everything inside it.
func (d *Decoder) Skip() {
	_ = d.read(nil)
}

// String returns the text of the element entered last and consumes it. An
// empty element is the empty string.
func (d *Decoder) String() *string {
	s, ok := d.text()
	if !ok {
		return nil
	}

	return &s
}

// Int32 returns the element entered last as an int32.
func (d *Decoder) Int32() *int32 { return parseValue(d, parseInt32) }

// Int64 returns the element entered last as an int64.
func (d *Decoder) Int64() *int64 { return parseValue(d, parseInt64) }

// Float64 returns the element entered last as a float64. "NaN", "Infinity"
// and "-Infinity" are read as such.
func (d *Decoder) Float64() *float64 { return parseValue(d, parseFloat64) }

// Bool returns the element entered last as a bool: "true" or "false", in any
// case.
func (d *Decoder) Bool() *bool { return parseValue(d, parseBool) }

// Time returns the element entered last read in format, iso8601 when empty.
func (d *Decoder) Time(format string) *protocol.Time {
	s, ok := d.text()
	if !ok {
		return nil
	}

	t, err := rest.ParseTime(strings.TrimSpace(s), format, rest.TimeISO8601)
	if err != nil {
		d.fail(err)

		return nil
	}

	return t
}

// Blob returns the base64-decoded element entered last. An empty element is
// an empty, non-nil blob.
func (d *Decoder) Blob() []byte {
	s, ok := d.text()
	if !ok {
		return nil
	}

	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		d.fail(fmt.Errorf("blob: %w", err))

		return nil
	}

	if b == nil {
		b = []byte{}
	}

	return b
}

// enter records a start element as the current one.
func (d *Decoder) enter(start xml.StartElement) (string, bool) {
	if len(d.stack) == MaxDepth {
		d.fail(fmt.Errorf("elements nest deeper than %d", MaxDepth))

		return "", false
	}

	d.stack = append(d.stack, start.Name)
	d.attrs = start.Attr

	return start.Name.Local, true
}

// leave checks an end tag against the current element and closes it.
func (d *Decoder) leave(end xml.EndElement) {
	top := d.stack[len(d.stack)-1]
	if end.Name != top {
		d.fail(fmt.Errorf("element <%s> closed by </%s>", qualified(top), qualified(end.Name)))

		return
	}

	d.stack = d.stack[:len(d.stack)-1]
	d.attrs = nil
}

// text reads the character data of the current element up to its end tag,
// skipping any element inside it, and closes it.
func (d *Decoder) text() (string, bool) {
	var b strings.Builder

	ok := d.read(&b)

	return b.String(), ok
}

// read consumes the current element as text does, writing its character data
// to b. A nil b keeps nothing, so Skip costs nothing for the text it skips.
func (d *Decoder) read(b *strings.Builder) bool {
	if d.err != nil || len(d.stack) == 0 {
		return false
	}

	depth := len(d.stack)

	for {
		tok, ok := d.token()
		if !ok {
			return false
		}

		switch t := tok.(type) {
		case xml.CharData:
			if b != nil && len(d.stack) == depth {
				b.Write(t)
			}
		case xml.StartElement:
			if _, ok := d.enter(t); !ok {
				return false
			}
		case xml.EndElement:
			d.leave(t)

			if d.err != nil {
				return false
			}

			if len(d.stack) < depth {
				return true
			}
		default:
			// Comments and processing instructions carry no text.
		}
	}
}

// token reads the next token inside the current element. The end of the body
// is an error there.
func (d *Decoder) token() (xml.Token, bool) {
	tok, err := d.xd.RawToken()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("body ends inside <%s>", qualified(d.stack[len(d.stack)-1]))
		}

		d.fail(err)

		return nil, false
	}

	if d.chargeToken(tok); d.err != nil {
		return nil, false
	}

	return tok, true
}

func (d *Decoder) fail(err error) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: %w", ErrMalformed, err)
	}
}

// parseValue reads the text of the current element, trimmed of white space,
// and parses it. An empty element is an error.
func parseValue[T any](d *Decoder, parse func(string) (T, error)) *T {
	s, ok := d.text()
	if !ok {
		return nil
	}

	v, err := parse(strings.TrimSpace(s))
	if err != nil {
		d.fail(err)

		return nil
	}

	return &v
}

func qualified(n xml.Name) string {
	if n.Space == "" {
		return n.Local
	}

	return n.Space + ":" + n.Local
}

func parseInt32(s string) (int32, error) {
	v, err := strconv.ParseInt(s, decimalBase, int32Bits)
	if err != nil {
		return 0, fmt.Errorf("int32: %w", err)
	}

	return int32(v), nil
}

func parseInt64(s string) (int64, error) {
	v, err := strconv.ParseInt(s, decimalBase, int64Bits)
	if err != nil {
		return 0, fmt.Errorf("int64: %w", err)
	}

	return v, nil
}

func parseFloat64(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, int64Bits)
	if err != nil {
		return 0, fmt.Errorf("float64: %w", err)
	}

	return v, nil
}

func parseBool(s string) (bool, error) {
	switch {
	case strings.EqualFold(s, "true"):
		return true, nil
	case strings.EqualFold(s, "false"):
		return false, nil
	default:
		return false, fmt.Errorf("bool: %q", s)
	}
}
