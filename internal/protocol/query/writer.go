package query

import (
	"encoding/base64"
	"math"
	"strconv"
	"sync"

	"github.com/kylecannon/gostack/internal/protocol"
)

const xmlDeclaration = "<?xml version='1.0' encoding='utf-8'?>\n"

// Options configure a Writer.
type Options struct {
	// EscapeQuotes writes a double quote as &quot; in text. SQS responses do;
	// every writer escapes &, < and >, and the carriage return as &#xD;.
	EscapeQuotes bool
}

// Writer appends the XML elements of one response to a buffer. Elements are
// written in call order and nothing is validated, so generated code must open
// and close them in pairs.
type Writer struct {
	buf   []byte
	quote bool
}

var writerPool = sync.Pool{New: func() any { return &Writer{} }}

// NewWriter returns a Writer with an empty buffer.
func NewWriter(opts Options) *Writer {
	return &Writer{quote: opts.EscapeQuotes}
}

// Bytes returns what has been written. It is valid until the next write.
func (w *Writer) Bytes() []byte {
	return w.buf
}

// Open writes a start tag.
func (w *Writer) Open(name string) {
	w.buf = append(w.buf, '<')
	w.buf = append(w.buf, name...)
	w.buf = append(w.buf, '>')
}

// Close writes an end tag. An element with no content becomes <name/>, as
// LocalStack's ElementTree writes it; a client reads both forms alike, but a
// test of LocalStack looks for the short one.
func (w *Writer) Close(name string) {
	const tagMarks = 2 // the "<" and ">" around the name of the start tag

	start := len(w.buf) - len(name) - tagMarks
	if start >= 0 && w.buf[start] == '<' && w.buf[len(w.buf)-1] == '>' && string(w.buf[start+1:len(w.buf)-1]) == name {
		w.buf = append(w.buf[:len(w.buf)-1], '/', '>')

		return
	}

	w.buf = append(w.buf, '<', '/')
	w.buf = append(w.buf, name...)
	w.buf = append(w.buf, '>')
}

// String writes <name>value</name>, escaping value.
func (w *Writer) String(name, value string) {
	w.Open(name)
	w.escape(value)
	w.Close(name)
}

// Int writes an integer element.
func (w *Writer) Int(name string, value int64) {
	w.Open(name)
	w.buf = strconv.AppendInt(w.buf, value, decimalBase)
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

	w.Close(name)
}

// Bool writes "true" or "false".
func (w *Writer) Bool(name string, value bool) {
	w.Open(name)
	w.buf = strconv.AppendBool(w.buf, value)
	w.Close(name)
}

// Blob writes value in base64.
func (w *Writer) Blob(name string, value []byte) {
	w.Open(name)
	w.buf = base64.StdEncoding.AppendEncode(w.buf, value)
	w.Close(name)
}

// Time writes a timestamp in the given format (see TimeISO8601).
func (w *Writer) Time(name string, value *protocol.Time, format string) {
	w.String(name, FormatTime(value, format))
}

func (w *Writer) escape(s string) {
	start := 0

	for i := range len(s) {
		var repl string

		switch s[i] {
		case '&':
			repl = "&amp;"
		case '<':
			repl = "&lt;"
		case '>':
			repl = "&gt;"
		case '\r':
			repl = "&#xD;"
		case '"':
			if !w.quote {
				continue
			}

			repl = "&quot;"
		default:
			continue
		}

		w.buf = append(w.buf, s[start:i]...)
		w.buf = append(w.buf, repl...)
		start = i + 1
	}

	w.buf = append(w.buf, s[start:]...)
}

// getWriter takes a Writer from the pool.
func getWriter(opts Options) *Writer {
	w, ok := writerPool.Get().(*Writer)
	if !ok {
		w = &Writer{}
	}

	w.buf = w.buf[:0]
	w.quote = opts.EscapeQuotes

	return w
}

func putWriter(w *Writer) {
	const maxPooled = 1 << 16

	if cap(w.buf) <= maxPooled {
		writerPool.Put(w)
	}
}
