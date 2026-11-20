package query

import (
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

const (
	// PairCost is what RequestForm charges a name=value pair against
	// protocol.DecodeBudget: about what one pair holds once parsed into
	// url.Values (its slot in the map, a one-value slice), rounded up. A form
	// whose pairs cost more than the budget of its URL and body is refused
	// before they are parsed, so 16 MiB of "a&", 8 million pairs that would
	// hold some 900 MiB, costs the body. The largest query request the vendored
	// specs allow a served service is SQS SendMessageBatch at 4,853 pairs (10
	// entries, each with 10 message and 10 system attributes carrying every
	// member, at the limits AWS documents where the model sets no max); the
	// budget's own slack allows 131,072 pairs of any size.
	PairCost = 128

	// refusedPairs is how many pairs of a refused form are still parsed, for a
	// caller that only routes the call and looks for its Action.
	refusedPairs = 10_000
)

// ErrForm is the error RequestForm returns for a request whose parameters
// cannot be read: a body over protocol.MaxBody, pairs that cost more than
// protocol.DecodeBudget, an unreadable body, or text that is not valid UTF-8.
var ErrForm = errors.New("query: form cannot be read")

var errTooManyPairs = fmt.Errorf("%w: more pairs than the decode budget allows", ErrForm)

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
	parseInto(values, text, -1)

	return values
}

// parseInto adds the pairs of text to values as ParseValues reads them, after
// any a name already has. It adds at most left pairs, or all of them when left
// is negative, and returns how many more it may add.
func parseInto(values url.Values, text string, left int) int {
	for pair := range strings.SplitSeq(text, "&") {
		if pair == "" {
			continue
		}

		if left == 0 {
			return 0
		}

		left--

		name, value, _ := strings.Cut(pair, "=")
		name = unescape(name)
		values[name] = append(values[name], unescape(value))
	}

	return left
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
//
// Its decoding is charged to the Charge the gateway put on the request's
// context (protocol.ChargeOf), when there is one.
func RequestForm(r *http.Request) (url.Values, error) {
	if c := protocol.ChargeOf(r.Context()); c != nil {
		return RequestFormHeld(r, c)
	}

	return RequestFormHeld(r, nil)
}

// RequestFormHeld is RequestForm for a call whose decoding is charged to hold
// (request.Context): what the pairs cost past protocol.FreeDecode is held
// before any is parsed. When hold refuses, the error wraps protocol.ErrBusy and
// only the pairs FreeDecode pays for are parsed, for a caller that routes the
// call.
func RequestFormHeld(r *http.Request, hold protocol.DecodeHolder) (url.Values, error) {
	if r.Form != nil {
		return r.Form, nil
	}

	values, err := requestValues(r, hold)
	if err != nil {
		return values, err
	}

	r.Form = values

	return values, nil
}

// readBody reads the body of a form request as text and puts it back, so a
// service can read it whole. The pairs are parsed out of that text, and the
// values that need no unescaping share it, so the form costs no copy of its
// body past protocol.ReadBodyText's. A body an earlier call read and put back,
// and nothing has read since, gives the same text again without a second copy:
// a request whose form is refused is read by every caller that asks.
func readBody(r *http.Request) (string, error) {
	if put, ok := r.Body.(*formBody); ok && put.Len() == len(put.body) {
		return put.body, put.err
	}

	original := r.Body

	body, err := protocol.ReadRequestText(r, protocol.FormDecode)
	r.Body = &formBody{Reader: strings.NewReader(body), rest: original, body: body, err: err}

	return body, err //nolint:wrapcheck // the caller wraps it in ErrForm
}

func requestValues(r *http.Request, hold protocol.DecodeHolder) (url.Values, error) {
	raw := r.URL.RawQuery

	// Text that is not UTF-8 is still parsed, so a caller that only routes the
	// call can find its Action, but the read fails.
	var failure error

	if !utf8.ValidString(raw) {
		failure = fmt.Errorf("%w: the query string is not UTF-8", ErrForm)
	}

	if !isForm(r.Header.Get("Content-Type")) || r.Body == nil || r.Body == http.NoBody {
		return parsePairs(raw, "", failure, hold)
	}

	body, err := readBody(r)
	if err != nil {
		values, _ := parsePairs(raw, "", nil, nil)

		return values, fmt.Errorf("%w: %w", ErrForm, err)
	}

	if protocol.OverBody(len(body)) {
		values, _ := parsePairs(raw, "", nil, nil)

		return values, fmt.Errorf("%w: the body is over %d bytes", ErrForm, protocol.MaxBody())
	}

	if !utf8.ValidString(body) {
		failure = fmt.Errorf("%w: the body is not UTF-8", ErrForm)
	}

	return parsePairs(raw, body, failure, hold)
}

// parsePairs parses the pairs of raw, then those of body, and returns them
// with failure. Pairs that cost more than protocol.DecodeBudget of their text
// are counted before any is parsed: then only the first refusedPairs are, and
// the error is errTooManyPairs. What the pairs cost past protocol.FreeDecode
// is held from hold, when there is one, before any is parsed.
func parsePairs(raw, body string, failure error, hold protocol.DecodeHolder) (url.Values, error) {
	values := url.Values{}
	limit := int(protocol.DecodeBudget(len(raw)+len(body)) / PairCost)

	_, left, over := pairsEnd(raw, limit)
	if !over {
		_, left, over = pairsEnd(body, left)
	}

	pairs := limit - left
	if over {
		failure = errTooManyPairs
		limit, pairs = refusedPairs, refusedPairs
	}

	if cost := int64(pairs) * PairCost; hold != nil && cost > protocol.FreeDecode {
		_, err := hold.HoldDecode(cost)
		if err != nil {
			failure = fmt.Errorf("%w: %w", ErrForm, err)
			limit = protocol.FreeDecode / PairCost
		}
	}

	left = parseInto(values, raw, limit)
	end, _, _ := pairsEnd(body, left)
	parseInto(values, body[:end], left)

	return values, failure
}

// pairsEnd counts the pairs of text up to left of them. It returns where the
// pair after those starts, with over true, or the length of text and how many
// more pairs were allowed. Empty pairs are not counted, as parseInto skips
// them.
func pairsEnd(text string, left int) (int, int, bool) {
	for start := 0; start < len(text); {
		next := strings.IndexByte(text[start:], '&')
		if next == 0 {
			start++

			continue
		}

		if left == 0 {
			return start, 0, true
		}

		left--

		if next < 0 {
			break
		}

		start += next + 1
	}

	return len(text), left, false
}

// formBody is a form body read and put back: the bytes read, then whatever
// was left unread, closed with the original body.
type formBody struct {
	*strings.Reader

	rest io.ReadCloser
	body string
	err  error
}

func (b *formBody) Read(p []byte) (int, error) {
	if b.Len() > 0 {
		return b.Reader.Read(p) //nolint:wrapcheck // io.Reader: a strings.Reader's own result
	}

	return b.rest.Read(p)
}

func (b *formBody) Close() error {
	return b.rest.Close()
}

func isForm(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "application/x-www-form-urlencoded")
}
