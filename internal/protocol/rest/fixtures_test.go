package rest_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/require"
)

// The botocore protocol fixtures are written from the client's side: an input
// case gives the params a client was handed and the request it serializes, and
// an output case gives the response a service sent and the result the client
// parses. The drivers here run them the other way round, as a server does: an
// input case decodes the serialized request with the runtime and compares the
// members with params; an output case encodes result with the runtime, parses
// the response the way botocore does, and compares that with result and with
// what the same parse makes of the fixture's own response.
//
// The drivers are interpreters over the fixture's shapes. The emitter replaces
// them with generated code; until then they call the runtime the way generated
// code will.

// The model's shape types and member locations, and the names an error reads.
const (
	typeStructure = "structure"
	typeList      = "list"
	typeMap       = "map"
	typeString    = "string"
	typeInteger   = "integer"
	typeLong      = "long"
	typeFloat     = "float"
	typeDouble    = "double"
	typeBoolean   = "boolean"
	typeTimestamp = "timestamp"
	typeBlob      = "blob"

	locHeader  = "header"
	locHeaders = "headers"
	locQuery   = "querystring"
	locURI     = "uri"
	locStatus  = "statusCode"

	requestID  = "foo-id"
	codeKey    = "Code"
	messageKey = "Message"
)

// group is a set of cases that share a service model.
type group struct {
	Description string            `json:"description"`
	Shapes      map[string]*shape `json:"shapes"`
	Cases       []fixtureCase     `json:"cases"`
}

// shape and ref carry the traits the rest protocols read. codegen/spec does
// not hold the location and xml traits yet, so the drivers keep their own.
type shape struct {
	Type            string          `json:"type"`
	Members         map[string]*ref `json:"members"`
	Member          *ref            `json:"member"`
	Key             *ref            `json:"key"`
	Value           *ref            `json:"value"`
	LocationName    string          `json:"locationName"`
	Flattened       bool            `json:"flattened"`
	TimestampFormat string          `json:"timestampFormat"`
	XMLNamespace    *xmlNamespace   `json:"xmlNamespace"`
	Payload         string          `json:"payload"`
	Document        bool            `json:"document"`
}

type ref struct {
	Shape           string        `json:"shape"`
	Location        string        `json:"location"`
	LocationName    string        `json:"locationName"`
	Flattened       bool          `json:"flattened"`
	XMLAttribute    bool          `json:"xmlAttribute"`
	XMLNamespace    *xmlNamespace `json:"xmlNamespace"`
	TimestampFormat string        `json:"timestampFormat"`
	JSONValue       bool          `json:"jsonvalue"`
}

type xmlNamespace struct {
	Prefix string `json:"prefix"`
	URI    string `json:"uri"`
}

// UnmarshalJSON reads the trait either as {"prefix", "uri"} or as the bare uri.
func (n *xmlNamespace) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte(`"`)) {
		return json.Unmarshal(data, &n.URI) //nolint:wrapcheck // a json.Unmarshaler returns the decode error as is
	}

	type plainNamespace xmlNamespace

	return json.Unmarshal(data, (*plainNamespace)(n)) //nolint:wrapcheck // as above
}

type fixtureCase struct {
	ID           string          `json:"id"`
	Given        given           `json:"given"`
	Params       json.RawMessage `json:"params"`
	Serialized   serialized      `json:"serialized"`
	Response     response        `json:"response"`
	Result       json.RawMessage `json:"result"`
	Error        json.RawMessage `json:"error"`
	ErrorCode    string          `json:"errorCode"`
	ErrorMessage string          `json:"errorMessage"`
}

type given struct {
	Name   string    `json:"name"`
	HTTP   httpTrait `json:"http"`
	Input  *ref      `json:"input"`
	Output *ref      `json:"output"`
	Errors []ref     `json:"errors"`
}

type httpTrait struct {
	Method       string `json:"method"`
	RequestURI   string `json:"requestUri"`
	ResponseCode int    `json:"responseCode"`
}

type serialized struct {
	Method  string            `json:"method"`
	URI     string            `json:"uri"`
	Body    string            `json:"body"`
	Host    string            `json:"host"`
	Headers map[string]string `json:"headers"`
}

type response struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// model is the shapes of one group.
type model map[string]*shape

// member is a reference resolved against its shape: the reference's traits
// over the shape's own.
type member struct {
	name      string // the member name in its structure; empty for an element
	shape     *shape
	loc       string // locationName
	location  string
	flattened bool
	attribute bool
	namespace *xmlNamespace
	tsfmt     string
	jsonvalue bool
}

func (m model) resolve(name string, r *ref) member {
	s := m[r.Shape]
	if s == nil {
		panic("unknown shape " + r.Shape)
	}

	mm := member{
		name: name, shape: s, loc: s.LocationName, location: r.Location, flattened: s.Flattened || r.Flattened,
		attribute: r.XMLAttribute, namespace: s.XMLNamespace, tsfmt: s.TimestampFormat, jsonvalue: r.JSONValue,
	}

	if r.LocationName != "" {
		mm.loc = r.LocationName
	}

	if r.XMLNamespace != nil {
		mm.namespace = r.XMLNamespace
	}

	if r.TimestampFormat != "" {
		mm.tsfmt = r.TimestampFormat
	}

	return mm
}

// serial is the name a member goes by on the wire.
func (mm member) serial() string {
	if mm.loc != "" {
		return mm.loc
	}

	return mm.name
}

// inBody reports whether an output member travels in the body. A response has
// no querystring or URI, so botocore reads members bound there from the body.
func inBody(mm member) bool {
	switch mm.location {
	case locHeader, locHeaders, locStatus:
		return false
	default:
		return true
	}
}

// headerLines returns the lines of the header called name, matched in any
// case, as a client reads the wire.
func headerLines(h http.Header, name string) []string {
	var lines []string

	for k, v := range h {
		if strings.EqualFold(k, name) {
			lines = append(lines, v...)
		}
	}

	return lines
}

// members returns the members of a structure in name order.
func (m model) members(s *shape) []member {
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range s.Members {
			if !yield(name) {
				return
			}
		}
	})

	out := make([]member, 0, len(names))
	for _, name := range names {
		out = append(out, m.resolve(name, s.Members[name]))
	}

	return out
}

// ignoreList is botocore's protocol-tests-ignore-list.json.
type ignoreList struct {
	General   map[string]ignoreSet            `json:"general"`
	Protocols map[string]map[string]ignoreSet `json:"protocols"`
}

type ignoreSet struct {
	Suites []string `json:"suites"`
	Cases  []string `json:"cases"`
}

// ignored is botocore's _should_ignore_test.
func (l *ignoreList) ignored(proto, kind, suite, id string) bool {
	for _, set := range []ignoreSet{l.General[kind], l.Protocols[proto][kind]} {
		if slices.Contains(set.Suites, suite) || slices.Contains(set.Cases, id) {
			return true
		}
	}

	return false
}

func readFixture(t *testing.T, name string, v any) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "testdata", "botocore", name))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, v))
}

// fixtureFunc runs one case of a group.
type fixtureFunc func(t *testing.T, m model, c *fixtureCase)

// runCases runs every case of <kind>/<proto>.json that botocore's ignore list
// keeps, each as a parallel subtest named after the case id.
func runCases(t *testing.T, proto, kind string, run fixtureFunc) {
	t.Helper()

	var (
		groups []group
		ignore ignoreList
	)

	readFixture(t, kind+"/"+proto+".json", &groups)
	readFixture(t, "protocol-tests-ignore-list.json", &ignore)

	for _, g := range groups {
		for i := range g.Cases {
			c := &g.Cases[i]

			t.Run(c.ID, func(t *testing.T) {
				t.Parallel()

				if ignore.ignored(proto, kind, g.Description, c.ID) {
					t.Skip("botocore's ignore list skips this case")
				}

				run(t, model(g.Shapes), c)
			})
		}
	}
}

// Test_FixtureTally pins how many cases each fixture holds and how many
// botocore's ignore list keeps, so a fixture update or a broken ignore list
// shows up here and not as a quiet change in the pass count.
func Test_FixtureTally(t *testing.T) {
	t.Parallel()

	tests := []struct {
		giveProto, giveKind     string
		wantCases, wantRunnable int
	}{
		{giveProto: "rest-xml", giveKind: "input", wantCases: 94, wantRunnable: 82},
		{giveProto: "rest-xml", giveKind: "output", wantCases: 78, wantRunnable: 72},
		{giveProto: "rest-json", giveKind: "input", wantCases: 113, wantRunnable: 101},
		{giveProto: "rest-json", giveKind: "output", wantCases: 91, wantRunnable: 82},
	}

	var ignore ignoreList

	readFixture(t, "protocol-tests-ignore-list.json", &ignore)

	for _, tt := range tests {
		t.Run(tt.giveKind+"/"+tt.giveProto, func(t *testing.T) {
			t.Parallel()

			var groups []group

			readFixture(t, tt.giveKind+"/"+tt.giveProto+".json", &groups)

			cases, runnable := 0, 0

			for _, g := range groups {
				for _, c := range g.Cases {
					cases++

					if !ignore.ignored(tt.giveProto, tt.giveKind, g.Description, c.ID) {
						runnable++
					}
				}
			}

			require.Equal(t, tt.wantCases, cases)
			require.Equal(t, tt.wantRunnable, runnable)
		})
	}
}

// buildRequest writes the serialized request as HTTP/1.1 and reads it back
// with http.ReadRequest, so the runtime sees what a server sees: canonical
// header names, trimmed values and a parsed URL.
func buildRequest(t *testing.T, c *fixtureCase) *http.Request {
	t.Helper()

	host := c.Serialized.Host
	if host == "" {
		host = "example.com"
	}

	method := c.Serialized.Method
	if method == "" {
		method = c.Given.HTTP.Method
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, c.Serialized.URI, host)

	for k, v := range c.Serialized.Headers {
		if !strings.EqualFold(k, "Content-Length") && !strings.EqualFold(k, "Host") {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}

	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(c.Serialized.Body), c.Serialized.Body)

	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(b.String())))
	require.NoError(t, err)

	return req
}

// decodeBindings reads every input member bound outside the body.
func decodeBindings(t *testing.T, m model, in member, d *rest.Decoder) map[string]any {
	t.Helper()

	out := map[string]any{}

	for _, mm := range m.members(in.shape) {
		var v any

		switch mm.location {
		case locHeader:
			v = headerValue(t, m, mm, d)
		case locHeaders:
			v = plain(d.HeaderMap(mm.loc))
		case locQuery:
			v = queryValue(m, mm, d)
		case locURI:
			v = labelValue(mm, d)
		default:
			continue
		}

		if v != nil {
			out[mm.name] = v
		}
	}

	return out
}

func headerValue(t *testing.T, m model, mm member, d *rest.Decoder) any {
	t.Helper()

	name := mm.loc

	switch mm.shape.Type {
	case typeString:
		if !mm.jsonvalue {
			return plain(d.Header(name))
		}

		text := d.HeaderJSONValue(name)
		if text == nil {
			return nil
		}

		return jsonDynamic(t, []byte(*text))
	case typeInteger:
		return plain(d.HeaderInt32(name))
	case typeLong:
		return plain(d.HeaderInt64(name))
	case typeFloat, typeDouble:
		return plain(d.HeaderFloat64(name))
	case typeBoolean:
		return plain(d.HeaderBool(name))
	case typeTimestamp:
		return plain(d.HeaderTime(name, mm.tsfmt))
	case typeBlob:
		return plain(d.HeaderBlob(name))
	case typeList:
		el := m.resolve("", mm.shape.Member)

		switch el.shape.Type {
		case typeInteger:
			return plain(d.HeaderInt32s(name))
		case typeLong:
			return plain(d.HeaderInt64s(name))
		case typeFloat, typeDouble:
			return plain(d.HeaderFloat64s(name))
		case typeBoolean:
			return plain(d.HeaderBools(name))
		case typeTimestamp:
			return plain(d.HeaderTimes(name, el.tsfmt))
		default:
			return plain(d.HeaderStrings(name))
		}
	default:
		panic("header of type " + mm.shape.Type)
	}
}

func queryValue(m model, mm member, d *rest.Decoder) any {
	name := mm.loc

	switch mm.shape.Type {
	case typeString:
		return plain(d.Query(name))
	case typeInteger:
		return plain(d.QueryInt32(name))
	case typeLong:
		return plain(d.QueryInt64(name))
	case typeFloat, typeDouble:
		return plain(d.QueryFloat64(name))
	case typeBoolean:
		return plain(d.QueryBool(name))
	case typeTimestamp:
		return plain(d.QueryTime(name, mm.tsfmt))
	case typeList:
		el := m.resolve("", mm.shape.Member)

		switch el.shape.Type {
		case typeInteger:
			return plain(d.QueryInt32s(name))
		case typeLong:
			return plain(d.QueryInt64s(name))
		case typeFloat, typeDouble:
			return plain(d.QueryFloat64s(name))
		case typeBoolean:
			return plain(d.QueryBools(name))
		case typeTimestamp:
			return plain(d.QueryTimes(name, el.tsfmt))
		default:
			return plain(d.QueryStrings(name))
		}
	case typeMap:
		if m.resolve("", mm.shape.Value).shape.Type == typeList {
			return plain(d.QueryMapStrings())
		}

		return plain(d.QueryMap())
	default:
		panic("querystring member of type " + mm.shape.Type)
	}
}

func labelValue(mm member, d *rest.Decoder) any {
	name := mm.loc

	switch mm.shape.Type {
	case typeInteger:
		return plain(d.LabelInt32(name))
	case typeLong:
		return plain(d.LabelInt64(name))
	case typeFloat, typeDouble:
		return plain(d.LabelFloat64(name))
	case typeBoolean:
		return plain(d.LabelBool(name))
	case typeTimestamp:
		return plain(d.LabelTime(name, mm.tsfmt))
	default:
		return plain(d.Label(name))
	}
}

// queryMapsMatch relaxes the comparison of querystring maps: the server's map
// holds every parameter, those bound to named members too, while a client's
// params hold only what it put in the map. Every entry the client set must be
// in the server's map, and every other entry must be a named member's.
func queryMapsMatch(t *testing.T, m model, in member, want, got map[string]any) {
	t.Helper()

	named := map[string]bool{}

	for _, mm := range m.members(in.shape) {
		if mm.location == locQuery && mm.shape.Type != typeMap {
			named[mm.loc] = true
		}
	}

	for _, mm := range m.members(in.shape) {
		if mm.location != locQuery || mm.shape.Type != typeMap {
			continue
		}

		w, _ := want[mm.name].(map[string]any)
		g, _ := got[mm.name].(map[string]any)

		for k, v := range g {
			if _, ok := w[k]; !ok {
				require.True(t, named[k], "query map entry %q is neither set nor a named member", k)

				continue
			}

			require.Equal(t, canonical(w[k], dropEmpty), canonical(v, dropEmpty), "query map entry %q", k)
		}

		for k := range w {
			require.Contains(t, g, k)
		}

		delete(want, mm.name)
		delete(got, mm.name)
	}
}

// encodeBindings writes every output member bound to a header and returns
// the status a statusCode member asks for, or def.
func encodeBindings(t *testing.T, m model, out member, h http.Header, v map[string]any, def int) int {
	t.Helper()

	status := def

	for _, mm := range m.members(out.shape) {
		val, ok := v[mm.name]
		if !ok || val == nil {
			continue
		}

		switch mm.location {
		case locHeader:
			setHeader(t, m, mm, h, val)
		case locHeaders:
			entries := map[string]string{}
			for k, e := range must[map[string]any](t, val) {
				entries[k] = must[string](t, e)
			}

			rest.SetMap(h, mm.loc, entries)
		case locStatus:
			status = rest.Status(new(int32(number(t, val))), def)
		default:
			// Body members, and querystring and uri members, which a response
			// does not carry.
		}
	}

	return status
}

func setHeader(t *testing.T, m model, mm member, h http.Header, val any) {
	t.Helper()

	name := mm.loc

	switch mm.shape.Type {
	case typeString:
		if mm.jsonvalue {
			text, err := json.Marshal(val)
			require.NoError(t, err)
			rest.SetJSONValue(h, name, new(string(text)))

			return
		}

		rest.SetString(h, name, new(must[string](t, val)))
	case typeInteger:
		rest.SetInt32(h, name, new(int32(number(t, val))))
	case typeLong:
		rest.SetInt64(h, name, new(int64(number(t, val))))
	case typeFloat, typeDouble:
		rest.SetFloat64(h, name, new(number(t, val)))
	case typeBoolean:
		rest.SetBool(h, name, new(must[bool](t, val)))
	case typeTimestamp:
		rest.SetTime(h, name, timeOf(t, val), mm.tsfmt)
	case typeBlob:
		rest.SetBlob(h, name, []byte(must[string](t, val)))
	case typeList:
		setHeaderList(t, m, mm, h, must[[]any](t, val))
	default:
		panic("header of type " + mm.shape.Type)
	}
}

func setHeaderList(t *testing.T, m model, mm member, h http.Header, items []any) {
	t.Helper()

	el := m.resolve("", mm.shape.Member)

	switch el.shape.Type {
	case typeInteger:
		rest.SetInt32s(h, mm.loc, mapItems(items, func(v any) int32 { return int32(number(t, v)) }))
	case typeLong:
		rest.SetInt64s(h, mm.loc, mapItems(items, func(v any) int64 { return int64(number(t, v)) }))
	case typeFloat, typeDouble:
		rest.SetFloat64s(h, mm.loc, mapItems(items, func(v any) float64 { return number(t, v) }))
	case typeBoolean:
		rest.SetBools(h, mm.loc, mapItems(items, func(v any) bool { return must[bool](t, v) }))
	case typeTimestamp:
		rest.SetTimes(h, mm.loc, mapItems(items, func(v any) *protocol.Time { return timeOf(t, v) }), el.tsfmt)
	default:
		rest.SetStrings(h, mm.loc, mapItems(items, func(v any) string { return must[string](t, v) }))
	}
}

func mapItems[T any](items []any, f func(any) T) []T {
	out := make([]T, 0, len(items))
	for _, v := range items {
		out = append(out, f(v))
	}

	return out
}

// parseHeaders reads the members bound to headers and the status line out of a
// response the way botocore's parser does: header lists split on commas, and
// a prefix map keyed by the rest of each header name as it came.
func parseHeaders(t *testing.T, m model, out member, h http.Header, status int) map[string]any {
	t.Helper()

	got := map[string]any{}

	for _, mm := range m.members(out.shape) {
		switch mm.location {
		case locHeader:
			lines := headerLines(h, mm.loc)
			if len(lines) == 0 {
				continue
			}

			got[mm.name] = parseHeaderValue(t, m, mm, strings.Join(lines, ","))
		case locHeaders:
			entries := map[string]any{}

			for k, v := range h {
				if len(k) >= len(mm.loc) && strings.EqualFold(k[:len(mm.loc)], mm.loc) {
					entries[k[len(mm.loc):]] = v[0]
				}
			}

			got[mm.name] = entries
		case locStatus:
			got[mm.name] = float64(status)
		default:
		}
	}

	return got
}

func parseHeaderValue(t *testing.T, m model, mm member, s string) any {
	t.Helper()

	switch mm.shape.Type {
	case typeString:
		if !mm.jsonvalue {
			return s
		}

		b, err := base64.StdEncoding.DecodeString(s)
		require.NoError(t, err)

		return jsonDynamic(t, b)
	case typeList:
		el := m.resolve("", mm.shape.Member)
		parts := strings.Split(s, ",")

		if el.shape.Type == typeTimestamp && (el.tsfmt == "" || strings.EqualFold(el.tsfmt, rest.TimeRFC822)) {
			var paired []string
			for i := 0; i+1 < len(parts); i += 2 {
				paired = append(paired, parts[i]+","+parts[i+1])
			}

			parts = paired
		}

		items := make([]any, 0, len(parts))
		for _, p := range parts {
			items = append(items, parseScalar(t, el, strings.TrimSpace(p), rest.TimeRFC822))
		}

		return items
	default:
		return parseScalar(t, mm, s, rest.TimeRFC822)
	}
}

// parseScalar reads a scalar from its text form, timestamps in mm's format or
// def.
func parseScalar(t *testing.T, mm member, s, def string) any {
	t.Helper()

	switch mm.shape.Type {
	case typeInteger, typeLong, typeFloat, typeDouble:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		require.NoError(t, err)

		return plain(f)
	case typeBoolean:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		require.NoError(t, err)

		return b
	case typeTimestamp:
		tm, err := rest.ParseTime(strings.TrimSpace(s), mm.tsfmt, def)
		require.NoError(t, err)

		return plain(tm)
	case typeBlob:
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
		require.NoError(t, err)

		return string(b)
	default:
		return s
	}
}

// plain turns what the runtime returns into the values a fixture holds:
// numbers are float64s, the special floats their names, timestamps epoch
// seconds, blobs strings. A nil pointer, slice or map is nil.
func plain(v any) any {
	switch x := v.(type) {
	case *string:
		return deref(x)
	case *int32:
		return deref(x)
	case *int64:
		return deref(x)
	case *float64:
		return deref(x)
	case *bool:
		return deref(x)
	case *protocol.Time:
		if x == nil {
			return nil
		}

		return float64(x.Unix()) + float64(x.Nanosecond())/1e9
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		default:
			return x
		}
	case []byte:
		if x == nil {
			return nil
		}

		return string(x)
	case []string:
		return plainSlice(x)
	case []int32:
		return plainSlice(x)
	case []int64:
		return plainSlice(x)
	case []float64:
		return plainSlice(x)
	case []bool:
		return plainSlice(x)
	case []*protocol.Time:
		return plainSlice(x)
	case map[string]string:
		return plainMap(x)
	case map[string][]string:
		return plainMap(x)
	default:
		return v
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}

	return plain(*p)
}

func plainSlice[T any](s []T) any {
	if s == nil {
		return nil
	}

	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, plain(v))
	}

	return out
}

func plainMap[T any](m map[string]T) any {
	if m == nil {
		return nil
	}

	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = plain(v)
	}

	return out
}

// must asserts that a fixture value has the type the shape says.
func must[T any](t *testing.T, v any) T {
	t.Helper()

	x, ok := v.(T)
	require.True(t, ok, "fixture value %v is a %T", v, v)

	return x
}

// number reads a fixture number, or a special float written as its name.
func number(t *testing.T, v any) float64 {
	t.Helper()

	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		require.NoError(t, err)

		return f
	case float64:
		return x
	case string:
		f, err := strconv.ParseFloat(x, 64)
		require.NoError(t, err)

		return f
	default:
		panic(fmt.Sprintf("number of type %T", v))
	}
}

// timeOf reads a fixture timestamp, which is epoch seconds.
func timeOf(t *testing.T, v any) *protocol.Time {
	t.Helper()

	tm := &protocol.Time{}
	require.NoError(t, tm.UnmarshalJSON([]byte(fmt.Sprint(v))))

	return tm
}

// jsonDynamic reads json into maps, slices, strings, json.Numbers and bools.
// Nothing is nil.
func jsonDynamic(t *testing.T, raw []byte) any {
	t.Helper()

	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any

	require.NoError(t, dec.Decode(&v))

	return v
}

type emptyRule bool

const (
	dropEmpty emptyRule = true
	keepEmpty emptyRule = false
)

// canonical makes two values comparable: numbers become float64s rounded to
// the microsecond, and nulls are dropped, empty lists and maps too under
// dropEmpty. An empty string always stays.
func canonical(v any, rule emptyRule) any {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}

		return canonical(f, rule)
	case float64:
		if x != math.Trunc(x) {
			return math.Round(x*1e6) / 1e6
		}

		return x
	case map[string]any:
		out := map[string]any{}

		for k, e := range x {
			c := canonical(e, rule)
			if c == nil || rule == dropEmpty && isEmpty(c) {
				continue
			}

			out[k] = c
		}

		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, canonical(e, rule))
		}

		return out
	default:
		return v
	}
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	default:
		return false
	}
}
