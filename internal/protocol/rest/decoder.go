package rest

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	int32Bits   = 32
	int64Bits   = 64
	decimalBase = 10
)

// MaxBody is how many bytes of a request body that is not streamed a rest
// service reads: a json body or a blob or string payload. A streamed payload
// has no limit, and an xml body is read element by element.
const MaxBody = 16 << 20

// Decoder reads the members a request carries outside its body: headers, the
// querystring and URI labels. A malformed value does not stop decoding: the
// first one is kept and reported by Err, so generated code checks once, at the
// end. The strings it returns point into the request.
type Decoder struct {
	header http.Header
	query  url.Values
	labels Labels
	err    *awserr.Error
}

// NewDecoder returns a Decoder over r and the labels its Pattern matched. It
// parses the querystring once and never changes r.
func NewDecoder(r *http.Request, labels Labels) *Decoder {
	return &Decoder{header: r.Header, query: r.URL.Query(), labels: labels}
}

// Err returns the first malformed value met so far, or nil.
func (d *Decoder) Err() *awserr.Error {
	return d.err
}

// Header returns the first value of the header called name, or nil when the
// request has none.
func (d *Decoder) Header(name string) *string {
	v := d.header[textproto.CanonicalMIMEHeaderKey(name)]
	if len(v) == 0 {
		return nil
	}

	return &v[0]
}

// HeaderInt32 returns the header called name as an int32.
func (d *Decoder) HeaderInt32(name string) *int32 {
	return parseOne(d, name, d.Header(name), parseInt32)
}

// HeaderInt64 returns the header called name as an int64.
func (d *Decoder) HeaderInt64(name string) *int64 {
	return parseOne(d, name, d.Header(name), parseInt64)
}

// HeaderFloat64 returns the header called name as a float64. "NaN",
// "Infinity" and "-Infinity" are read as such.
func (d *Decoder) HeaderFloat64(name string) *float64 {
	return parseOne(d, name, d.Header(name), parseFloat64)
}

// HeaderBool returns the header called name as a bool: "true" or "false", in
// any case.
func (d *Decoder) HeaderBool(name string) *bool { return parseOne(d, name, d.Header(name), parseBool) }

// HeaderTime returns the header called name read in format, rfc822 when empty.
func (d *Decoder) HeaderTime(name, format string) *protocol.Time {
	return readTime(d, name, d.Header(name), format, TimeRFC822)
}

// HeaderBlob returns the base64-decoded header called name, or nil when the
// request has none.
func (d *Decoder) HeaderBlob(name string) []byte {
	s := d.Header(name)
	if s == nil {
		return nil
	}

	b, err := base64.StdEncoding.DecodeString(*s)
	if err != nil {
		d.fail(name, *s)

		return nil
	}

	return b
}

// HeaderJSONValue returns the json text a jsonvalue header carries in base64.
func (d *Decoder) HeaderJSONValue(name string) *string {
	b := d.HeaderBlob(name)
	if b == nil {
		return nil
	}

	return new(string(b))
}

// HeaderStrings returns the items of a list header: every line of the header
// joined, then split on commas. An item in double quotes may hold commas and
// backslash-escaped quotes.
func (d *Decoder) HeaderStrings(name string) []string {
	lines := d.header[textproto.CanonicalMIMEHeaderKey(name)]
	if len(lines) == 0 {
		return nil
	}

	items, err := splitList(strings.Join(lines, ","))
	if err != nil {
		d.fail(name, strings.Join(lines, ","))

		return nil
	}

	return items
}

// HeaderInt32s returns the items of a list header as int32s.
func (d *Decoder) HeaderInt32s(name string) []int32 {
	return parseList(d, name, d.HeaderStrings(name), parseInt32)
}

// HeaderInt64s returns the items of a list header as int64s.
func (d *Decoder) HeaderInt64s(name string) []int64 {
	return parseList(d, name, d.HeaderStrings(name), parseInt64)
}

// HeaderFloat64s returns the items of a list header as float64s.
func (d *Decoder) HeaderFloat64s(name string) []float64 {
	return parseList(d, name, d.HeaderStrings(name), parseFloat64)
}

// HeaderBools returns the items of a list header as bools.
func (d *Decoder) HeaderBools(name string) []bool {
	return parseList(d, name, d.HeaderStrings(name), parseBool)
}

// HeaderTimes returns the items of a list header as timestamps in format,
// rfc822 when empty. An HTTP date holds a comma itself, so rfc822 items are
// the split items taken in pairs.
func (d *Decoder) HeaderTimes(name, format string) []*protocol.Time {
	items := d.HeaderStrings(name)
	if format == "" || strings.EqualFold(format, TimeRFC822) {
		paired := make([]string, 0, len(items)/2+1)
		for i := 0; i < len(items); i += 2 {
			if i+1 == len(items) {
				d.fail(name, strings.Join(items, ","))

				return nil
			}

			paired = append(paired, items[i]+", "+items[i+1])
		}

		items = paired
	}

	return parseList(d, name, items, func(s string) (*protocol.Time, error) {
		return ParseTime(s, format, TimeRFC822)
	})
}

// HeaderMap returns every header whose name starts with prefix, matched in any
// case, keyed by the rest of its name in lower case: the server sees names
// already canonicalized, and S3 keeps user metadata keys in lower case. The
// lines of one header are joined with commas. It is nil when no header
// matches.
func (d *Decoder) HeaderMap(prefix string) map[string]string {
	var m map[string]string

	for name, lines := range d.header {
		if len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
			continue
		}

		if m == nil {
			m = make(map[string]string)
		}

		m[strings.ToLower(name[len(prefix):])] = strings.Join(lines, ",")
	}

	return m
}

// Query returns the first value of the querystring parameter called name, or
// nil when the request has none.
func (d *Decoder) Query(name string) *string {
	v := d.query[name]
	if len(v) == 0 {
		return nil
	}

	return &v[0]
}

// QueryInt32 returns the querystring parameter called name as an int32. An
// empty value reads as absent, as LocalStack's S3 request parser does for an
// integer member (S3RequestParser._parse_integer); long, boolean and timestamp
// members stay strict there, so they stay strict here.
func (d *Decoder) QueryInt32(name string) *int32 {
	if v := d.Query(name); v != nil && *v == "" {
		return nil
	}

	return parseOne(d, name, d.Query(name), parseInt32)
}

// QueryInt64 returns the querystring parameter called name as an int64.
func (d *Decoder) QueryInt64(name string) *int64 { return parseOne(d, name, d.Query(name), parseInt64) }

// QueryFloat64 returns the querystring parameter called name as a float64.
func (d *Decoder) QueryFloat64(name string) *float64 {
	return parseOne(d, name, d.Query(name), parseFloat64)
}

// QueryBool returns the querystring parameter called name as a bool.
func (d *Decoder) QueryBool(name string) *bool { return parseOne(d, name, d.Query(name), parseBool) }

// QueryTime returns the querystring parameter called name read in format,
// iso8601 when empty.
func (d *Decoder) QueryTime(name, format string) *protocol.Time {
	return readTime(d, name, d.Query(name), format, TimeISO8601)
}

// QueryStrings returns every value of the querystring parameter called name.
func (d *Decoder) QueryStrings(name string) []string {
	return d.query[name]
}

// QueryInt32s returns every value of the querystring parameter called name as
// an int32.
func (d *Decoder) QueryInt32s(name string) []int32 {
	return parseList(d, name, d.query[name], parseInt32)
}

// QueryInt64s returns every value of the querystring parameter called name as
// an int64.
func (d *Decoder) QueryInt64s(name string) []int64 {
	return parseList(d, name, d.query[name], parseInt64)
}

// QueryFloat64s returns every value of the querystring parameter called name
// as a float64.
func (d *Decoder) QueryFloat64s(name string) []float64 {
	return parseList(d, name, d.query[name], parseFloat64)
}

// QueryBools returns every value of the querystring parameter called name as a
// bool.
func (d *Decoder) QueryBools(name string) []bool {
	return parseList(d, name, d.query[name], parseBool)
}

// QueryTimes returns every value of the querystring parameter called name as a
// timestamp in format, iso8601 when empty.
func (d *Decoder) QueryTimes(name, format string) []*protocol.Time {
	return parseList(d, name, d.query[name], func(s string) (*protocol.Time, error) {
		return ParseTime(s, format, TimeISO8601)
	})
}

// QueryMap returns every querystring parameter, by its first value. A map
// member without a name takes the whole querystring, those bound to other
// members included. It is nil for an empty querystring.
func (d *Decoder) QueryMap() map[string]string {
	if len(d.query) == 0 {
		return nil
	}

	m := make(map[string]string, len(d.query))
	for k, v := range d.query {
		m[k] = v[0]
	}

	return m
}

// QueryMapStrings returns every querystring parameter with all its values. It
// is nil for an empty querystring.
func (d *Decoder) QueryMapStrings() map[string][]string {
	if len(d.query) == 0 {
		return nil
	}

	return d.query
}

// Label returns the URI label called name, or nil when the pattern has none.
func (d *Decoder) Label(name string) *string {
	for i := range d.labels {
		if d.labels[i].Name == name {
			return &d.labels[i].Value
		}
	}

	return nil
}

// LabelInt32 returns the URI label called name as an int32.
func (d *Decoder) LabelInt32(name string) *int32 { return parseOne(d, name, d.Label(name), parseInt32) }

// LabelInt64 returns the URI label called name as an int64.
func (d *Decoder) LabelInt64(name string) *int64 { return parseOne(d, name, d.Label(name), parseInt64) }

// LabelFloat64 returns the URI label called name as a float64.
func (d *Decoder) LabelFloat64(name string) *float64 {
	return parseOne(d, name, d.Label(name), parseFloat64)
}

// LabelBool returns the URI label called name as a bool.
func (d *Decoder) LabelBool(name string) *bool { return parseOne(d, name, d.Label(name), parseBool) }

// LabelTime returns the URI label called name read in format, iso8601 when
// empty.
func (d *Decoder) LabelTime(name, format string) *protocol.Time {
	return readTime(d, name, d.Label(name), format, TimeISO8601)
}

func (d *Decoder) fail(name, value string) {
	if d.err != nil {
		return
	}

	d.err = &awserr.Error{
		Code:       "InvalidArgument",
		Message:    fmt.Sprintf("Value %s for parameter %s is invalid.", value, name),
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// parseOne parses s, when there is one, and records a malformed value.
func parseOne[T any](d *Decoder, name string, s *string, parse func(string) (T, error)) *T {
	if s == nil {
		return nil
	}

	v, err := parse(*s)
	if err != nil {
		d.fail(name, *s)

		return nil
	}

	return &v
}

func readTime(d *Decoder, name string, s *string, format, def string) *protocol.Time {
	if s == nil {
		return nil
	}

	t, err := ParseTime(*s, format, def)
	if err != nil {
		d.fail(name, *s)

		return nil
	}

	return t
}

// parseList parses every item, or records the first malformed one and returns
// nil.
func parseList[T any](d *Decoder, name string, items []string, parse func(string) (T, error)) []T {
	if len(items) == 0 {
		return nil
	}

	out := make([]T, 0, len(items))

	for _, s := range items {
		v, err := parse(strings.TrimSpace(s))
		if err != nil {
			d.fail(name, s)

			return nil
		}

		out = append(out, v)
	}

	return out
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

// splitList splits a header list on commas. Items are trimmed of spaces and
// tabs; an item in double quotes keeps its commas and has its backslash
// escapes removed.
func splitList(s string) ([]string, error) {
	var items []string

	for {
		s = strings.TrimLeft(s, " \t")

		var item string

		if strings.HasPrefix(s, `"`) {
			var err error

			item, s, err = cutQuoted(s[1:])
			if err != nil {
				return nil, err
			}

			s = strings.TrimLeft(s, " \t")
			if s != "" && s[0] != ',' {
				return nil, fmt.Errorf("header list: text after a quoted item: %q", s)
			}
		} else {
			item, _, _ = strings.Cut(s, ",")
			s = s[len(item):]
			item = strings.TrimRight(item, " \t")
		}

		items = append(items, item)

		if s == "" {
			return items, nil
		}

		s = s[1:] // the comma
	}
}

// cutQuoted reads a quoted item up to its closing quote and returns it
// unescaped, with what follows the quote.
func cutQuoted(s string) (string, string, error) {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 == len(s) {
				return "", "", fmt.Errorf("header list: escape at the end of %q", s)
			}

			i++
			b.WriteByte(s[i])
		case '"':
			return b.String(), s[i+1:], nil
		default:
			b.WriteByte(s[i])
		}
	}

	return "", "", fmt.Errorf("header list: unterminated quote in %q", s)
}
