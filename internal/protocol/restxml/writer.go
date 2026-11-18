package restxml

import (
	"encoding/base64"
	"math"
	"strconv"
	"sync"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
)

// xmlDeclaration is the one S3 sends, in double quotes.
const xmlDeclaration = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

// Writer appends the XML elements of one body to a buffer. Elements are
// written in call order and nothing is validated, so generated code opens and
// closes them in pairs. The model traits map onto it this way:
//   - a member's locationName, or a list member's, is the name passed to Open;
//   - a flattened list or map writes its items with no wrapping element;
//   - an xmlAttribute member is an Attr call right after its element's Open;
//   - an xmlNamespace trait is a Namespace call right after Open;
//   - a timestampFormat is the format passed to Time.
type Writer struct {
	buf []byte
	// open is where the start tag written last begins, while nothing has been
	// written after it; -1 otherwise. Close turns such a tag into <name/>.
	open int
}

var writerPool = sync.Pool{New: func() any { return &Writer{open: -1} }}

// NewWriter returns a Writer with an empty buffer.
func NewWriter() *Writer {
	return &Writer{open: -1}
}

// Bytes returns what has been written. It is valid until the next write.
func (w *Writer) Bytes() []byte {
	return w.buf
}

// Open writes a start tag.
func (w *Writer) Open(name string) {
	w.open = len(w.buf)
	w.buf = append(w.buf, '<')
	w.buf = append(w.buf, name...)
	w.buf = append(w.buf, '>')
}

// Attr adds an attribute to the start tag Open wrote last. It must come before
// anything else is written into the element.
func (w *Writer) Attr(name, value string) {
	w.buf = w.buf[:len(w.buf)-1] // the start tag's ">"
	w.buf = append(w.buf, ' ')
	w.buf = append(w.buf, name...)
	w.buf = append(w.buf, '=', '"')
	w.escape(value, attrEscapes)
	w.buf = append(w.buf, '"', '>')
}

// Namespace declares uri on the start tag Open wrote last: as the default
// namespace when prefix is empty, otherwise bound to prefix.
func (w *Writer) Namespace(prefix, uri string) {
	if prefix == "" {
		w.Attr("xmlns", uri)

		return
	}

	w.Attr("xmlns:"+prefix, uri)
}

// Close writes an end tag. An element with nothing in it becomes <name/>, as
// LocalStack's ElementTree writes it; a client reads both forms alike.
func (w *Writer) Close(name string) {
	if w.open >= 0 {
		w.buf = append(w.buf[:len(w.buf)-1], '/', '>')
		w.open = -1

		return
	}

	w.buf = append(w.buf, '<', '/')
	w.buf = append(w.buf, name...)
	w.buf = append(w.buf, '>')
}

// Text writes escaped text into the element Open wrote last, for a scalar
// whose start tag carries an attribute or a namespace.
func (w *Writer) Text(value string) {
	if value != "" {
		w.escape(value, textEscapes)
		w.open = -1
	}
}

// String writes <name>value</name>, escaping value.
func (w *Writer) String(name, value string) {
	w.Open(name)

	if value != "" {
		w.escape(value, textEscapes)
		w.open = -1
	}

	w.Close(name)
}

// Int writes an integer element.
func (w *Writer) Int(name string, value int64) {
	w.Open(name)
	w.buf = strconv.AppendInt(w.buf, value, decimalBase)
	w.open = -1
	w.Close(name)
}

// Float writes a floating-point element. NaN and the infinities are written
// as "NaN", "Infinity" and "-Infinity".
func (w *Writer) Float(name string, value float64) {
	w.Open(name)

	switch {
	case math.IsNaN(value):
		w.buf = append(w.buf, "NaN"...)
	case math.IsInf(value, 1):
		w.buf = append(w.buf, "Infinity"...)
	case math.IsInf(value, -1):
		w.buf = append(w.buf, "-Infinity"...)
	default:
		w.buf = strconv.AppendFloat(w.buf, value, 'g', -1, int64Bits)
	}

	w.open = -1
	w.Close(name)
}

// Bool writes "true" or "false".
func (w *Writer) Bool(name string, value bool) {
	w.Open(name)
	w.buf = strconv.AppendBool(w.buf, value)
	w.open = -1
	w.Close(name)
}

// Blob writes value in base64. An empty blob is an empty element.
func (w *Writer) Blob(name string, value []byte) {
	w.Open(name)

	if len(value) > 0 {
		w.buf = base64.StdEncoding.AppendEncode(w.buf, value)
		w.open = -1
	}

	w.Close(name)
}

// Time writes a timestamp in format, iso8601 when empty.
func (w *Writer) Time(name string, value *protocol.Time, format string) {
	w.String(name, rest.FormatTime(value, format, rest.TimeISO8601))
}

// textEscapes escapes &, < and > in text, and the carriage return as &#xD; so
// a parser does not fold it into a line feed. attrEscapes adds the double
// quote, line feed and tab, which an attribute value would otherwise end at or
// normalize.
var textEscapes, attrEscapes = escapeTable("&<>\r"), escapeTable("&<>\r\"\n\t")

// escapeTable returns the replacement of each of chars, indexed by byte.
func escapeTable(chars string) *[256]string {
	refs := map[byte]string{
		'&': "&amp;", '<': "&lt;", '>': "&gt;", '\r': "&#xD;", '"': "&quot;", '\n': "&#xA;", '\t': "&#x9;",
	}

	var t [256]string
	for i := range len(chars) {
		t[chars[i]] = refs[chars[i]]
	}

	return &t
}

// escape appends s with the replacements of table.
func (w *Writer) escape(s string, table *[256]string) {
	start := 0

	for i := range len(s) {
		repl := table[s[i]]
		if repl == "" {
			continue
		}

		w.buf = append(w.buf, s[start:i]...)
		w.buf = append(w.buf, repl...)
		start = i + 1
	}

	w.buf = append(w.buf, s[start:]...)
}

func getWriter() *Writer {
	w, ok := writerPool.Get().(*Writer)
	if !ok {
		w = NewWriter()
	}

	w.buf = w.buf[:0]
	w.open = -1

	return w
}

func putWriter(w *Writer) {
	const maxPooled = 1 << 16

	if cap(w.buf) <= maxPooled {
		writerPool.Put(w)
	}
}
