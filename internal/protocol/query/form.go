package query

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	asciiLimit = 0x80
	hexUpper   = "0123456789ABCDEF"
)

// ErrForm is the error RequestForm returns for a request whose parameters
// cannot be read: a body over protocol.MaxBody, an unreadable body, or text that is
// not valid UTF-8.
var ErrForm = errors.New("query: form cannot be read")

// ParseValues parses urlencoded text the way LocalStack's werkzeug does, which
// is Python's parse_qsl(keep_blank_values=True, errors="werkzeug.url_quote"):
//   - pairs are separated by "&" only, never ";";
//   - a pair with no "=" is a name with an empty value, and an empty pair is
//     skipped;
//   - "+" is a space, and an escape that is not two hex digits stays as typed;
//   - an escape that decodes to bytes that are not UTF-8 stays percent-encoded,
//     in upper case, so "a%ffb" is the text "a%FFb".
//
// The text has to be valid UTF-8, as werkzeug decodes it before it parses it.
// Every value ParseValues returns is valid UTF-8 as well.
func ParseValues(text string) url.Values {
	values := url.Values{}

	for pair := range strings.SplitSeq(text, "&") {
		if pair == "" {
			continue
		}

		name, value, _ := strings.Cut(pair, "=")
		name = unescape(name)
		values[name] = append(values[name], unescape(value))
	}

	return values
}

// unescape is Python's unquote(s.replace("+", " ")) with the werkzeug.url_quote
// error handler. Python decodes each run of ASCII characters on its own, so a
// character that is not ASCII ends a run: its bytes pass through, and the
// percent-escapes on either side do not join into one sequence.
func unescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}

	var out strings.Builder

	for len(s) > 0 {
		run := 0
		for run < len(s) && s[run] < asciiLimit {
			run++
		}

		if run == 0 {
			// Valid UTF-8 text, so this is one whole non-ASCII character.
			_, size := utf8.DecodeRuneInString(s)
			out.WriteString(s[:size])
			s = s[size:]

			continue
		}

		unescapeRun(&out, s[:run])
		s = s[run:]
	}

	return out.String()
}

// unescapeRun decodes one run of ASCII characters.
func unescapeRun(out *strings.Builder, run string) {
	decoded := make([]byte, 0, len(run))

	for i := 0; i < len(run); i++ {
		switch c := run[i]; {
		case c == '+':
			decoded = append(decoded, ' ')
		case c == '%' && i+2 < len(run) && isHex(run[i+1]) && isHex(run[i+2]):
			decoded = append(decoded, unhex(run[i+1])<<4|unhex(run[i+2]))
			i += 2
		default:
			decoded = append(decoded, c)
		}
	}

	for len(decoded) > 0 {
		r, size := utf8.DecodeRune(decoded)
		if r == utf8.RuneError && size == 1 {
			out.WriteByte('%')
			out.WriteByte(hexUpper[decoded[0]>>4])
			out.WriteByte(hexUpper[decoded[0]&0x0f])
		} else {
			out.Write(decoded[:size])
		}

		decoded = decoded[size:]
	}
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10 //nolint:mnd // a hex digit's value.
	case c >= 'A':
		return c - 'A' + 10 //nolint:mnd // a hex digit's value.
	default:
		return c - '0'
	}
}

// MalformedQueryString is the answer to a request whose parameters RequestForm
// cannot read.
func MalformedQueryString() *awserr.Error {
	return &awserr.Error{
		Code:       "MalformedQueryString",
		Message:    "The request could not be parsed as a form.",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// RequestForm returns the parameters of r the way werkzeug's request.values
// does: the pairs of the URL query come first, then those of a form body, so
// the first value of a name is the one in the URL. The body is read at most
// once and put back, so a service can read it whole.
//
// A request whose Form field is already set returns that field. A successful
// read sets Form, so one request is parsed once however many readers ask.
//
// An error wraps ErrForm. The values returned with it are whatever was read,
// for a caller that only routes the call. They can hold text that is not UTF-8
// and are not stored in Form.
func RequestForm(r *http.Request) (url.Values, error) {
	if r.Form != nil {
		return r.Form, nil
	}

	values, err := requestValues(r)
	if err != nil {
		return values, err
	}

	r.Form = values

	return values, nil
}

func requestValues(r *http.Request) (url.Values, error) {
	values := ParseValues(r.URL.RawQuery)

	// Text that is not UTF-8 is still parsed, so a caller that only routes the
	// call can find its Action, but the read fails.
	var failure error

	if !utf8.ValidString(r.URL.RawQuery) {
		failure = fmt.Errorf("%w: the query string is not UTF-8", ErrForm)
	}

	if !isForm(r.Header.Get("Content-Type")) || r.Body == nil || r.Body == http.NoBody {
		return values, failure
	}

	original := r.Body

	body, err := io.ReadAll(protocol.LimitBody(original))
	r.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}

	if err != nil {
		return values, fmt.Errorf("%w: %w", ErrForm, err)
	}

	if protocol.OverBody(len(body)) {
		return values, fmt.Errorf("%w: the body is over %d bytes", ErrForm, protocol.MaxBody())
	}

	if !utf8.Valid(body) {
		failure = fmt.Errorf("%w: the body is not UTF-8", ErrForm)
	}

	for name, list := range ParseValues(string(body)) {
		values[name] = append(values[name], list...)
	}

	return values, failure
}

// readCloser reads from one place and closes another.
type readCloser struct {
	io.Reader
	io.Closer
}

func isForm(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "application/x-www-form-urlencoded")
}
