package rest_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_RestXML_Input(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-xml", "input", func(t *testing.T, m model, c *fixtureCase) {
		got := map[string]any{}

		if c.Given.Input != nil {
			req := buildRequest(t, c)
			in := m.resolve("", c.Given.Input)

			labels, ok := rest.MustParsePattern(c.Given.HTTP.RequestURI).Match(req.URL.EscapedPath())
			require.True(t, ok, "%s does not match %s", req.URL.EscapedPath(), c.Given.HTTP.RequestURI)

			d := rest.NewDecoder(req, labels)
			got = decodeBindings(t, m, in, d)
			require.Nil(t, d.Err())

			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)

			maps.Copy(got, decodeXMLBody(t, m, in, body))
		}

		want, _ := jsonDynamic(t, c.Params).(map[string]any)
		if want == nil {
			want = map[string]any{}
		}

		if c.Given.Input != nil {
			queryMapsMatch(t, m, m.resolve("", c.Given.Input), want, got)
		}

		assert.Equal(t, canonical(want, dropEmpty), canonical(got, dropEmpty))
	})
}

func Test_RestXML_Output(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-xml", "output", func(t *testing.T, m model, c *fixtureCase) {
		if len(c.Given.Errors) > 0 {
			checkXMLError(t, m, c)

			return
		}

		rec := httptest.NewRecorder()
		got, want := map[string]any{}, map[string]any{}

		if c.Given.Output == nil {
			restxml.WriteResponse(rec, &restxml.Envelope{RequestID: requestID}, c.Given.HTTP.ResponseCode, nil)
		} else {
			out := m.resolve("", c.Given.Output)
			result, _ := jsonDynamic(t, c.Result).(map[string]any)

			writeXMLResponse(t, m, out, rec, c.Given.HTTP.ResponseCode, result)

			got = parseXMLResponse(t, m, out, rec.Header(), rec.Code, rec.Body.Bytes())
			want = parseXMLResponse(t, m, out, wireHeader(c.Response.Headers), c.Response.StatusCode,
				[]byte(c.Response.Body))
		}

		require.Equal(t, c.Response.StatusCode, rec.Code)
		assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty), "what a client parses from the fixture")

		result, _ := jsonDynamic(t, c.Result).(map[string]any)
		if result == nil {
			result = map[string]any{}
		}

		assert.Equal(t, canonical(result, keepEmpty), canonical(got, keepEmpty))
	})
}

// wireHeader holds a fixture's headers under the names as written, as they
// arrive on the wire.
func wireHeader(headers map[string]string) http.Header {
	h := http.Header{}
	for k, v := range headers {
		h[k] = []string{v}
	}

	return h
}

// decodeXMLBody reads the body members of an input: the payload member when
// the input names one, otherwise every member not bound elsewhere.
func decodeXMLBody(t *testing.T, m model, in member, body []byte) map[string]any {
	t.Helper()

	got := map[string]any{}

	target := in
	if in.shape.Payload != "" {
		pm := m.resolve(in.shape.Payload, in.shape.Members[in.shape.Payload])

		switch pm.shape.Type {
		case typeBlob, typeString:
			if len(body) > 0 {
				got[pm.name] = string(body)
			}

			return got
		default:
			target = pm
		}
	}

	d := restxml.NewDecoder(bytes.NewReader(body))
	if _, ok := d.Root(); !ok {
		require.NoError(t, d.Err())

		return got
	}

	v := decodeXMLStruct(m, target, d)
	require.NoError(t, d.Err())

	if target.name != "" {
		got[target.name] = v

		return got
	}

	return v
}

// decodeXMLStruct reads the members of the structure entered last, as
// generated code does: attributes first, then each child by its name.
func decodeXMLStruct(m model, s member, d *restxml.Decoder) map[string]any {
	out := map[string]any{}
	byName := map[string]member{}

	for _, mm := range m.members(s.shape) {
		switch {
		case mm.location != "":
		case mm.attribute:
			if v, ok := d.Attr(mm.loc); ok {
				out[mm.name] = v
			}
		default:
			byName[mm.serial()] = mm

			if mm.shape.Type == typeList && mm.flattened {
				if el := m.resolve("", mm.shape.Member); el.loc != "" {
					byName[el.loc] = mm
				}
			}
		}
	}

	for {
		name, ok := d.Next()
		if !ok {
			return out
		}

		mm, found := byName[name]
		if !found {
			d.Skip()

			continue
		}

		switch {
		case mm.shape.Type == typeList && mm.flattened:
			items, _ := out[mm.name].([]any)
			out[mm.name] = append(items, decodeXMLValue(m, m.resolve("", mm.shape.Member), d))
		case mm.shape.Type == typeMap && mm.flattened:
			entries, _ := out[mm.name].(map[string]any)
			if entries == nil {
				entries = map[string]any{}
			}

			decodeXMLEntry(m, mm, d, entries)
			out[mm.name] = entries
		default:
			out[mm.name] = decodeXMLValue(m, mm, d)
		}
	}
}

func decodeXMLValue(m model, mm member, d *restxml.Decoder) any {
	switch mm.shape.Type {
	case typeStructure:
		return decodeXMLStruct(m, mm, d)
	case typeList:
		items := []any{}
		el := m.resolve("", mm.shape.Member)

		for {
			if _, ok := d.Next(); !ok {
				return items
			}

			items = append(items, decodeXMLValue(m, el, d))
		}
	case typeMap:
		entries := map[string]any{}

		for {
			if _, ok := d.Next(); !ok {
				return entries
			}

			decodeXMLEntry(m, mm, d, entries)
		}
	case typeInteger:
		return plain(d.Int32())
	case typeLong:
		return plain(d.Int64())
	case typeFloat, typeDouble:
		return plain(d.Float64())
	case typeBoolean:
		return plain(d.Bool())
	case typeTimestamp:
		return plain(d.Time(mm.tsfmt))
	case typeBlob:
		return plain(d.Blob())
	default:
		return plain(d.String())
	}
}

// decodeXMLEntry reads one map entry, the element entered last, into entries.
func decodeXMLEntry(m model, mm member, d *restxml.Decoder, entries map[string]any) {
	key, value := m.resolve("", mm.shape.Key), m.resolve("", mm.shape.Value)
	keyName, valueName := orDefault(key.loc, "key"), orDefault(value.loc, "value")

	var (
		k string
		v any
	)

	for {
		name, ok := d.Next()
		if !ok {
			break
		}

		switch name {
		case keyName:
			if s := d.String(); s != nil {
				k = *s
			}
		case valueName:
			v = decodeXMLValue(m, value, d)
		default:
			d.Skip()
		}
	}

	entries[k] = v
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}

	return s
}

// writeXMLResponse writes result as generated code will: header members with
// rest, the body as raw bytes for a blob or string payload, otherwise as XML
// with restxml.
func writeXMLResponse(
	t *testing.T, m model, out member, w http.ResponseWriter, def int, result map[string]any,
) {
	t.Helper()

	status := encodeBindings(t, m, out, w.Header(), result, def)
	env := &restxml.Envelope{RequestID: requestID}

	if p := out.shape.Payload; p != "" {
		pm := m.resolve(p, out.shape.Members[p])
		v, ok := result[p]

		switch {
		case !ok || v == nil:
			restxml.WriteResponse(w, env, status, nil)
		case pm.shape.Type == typeBlob || pm.shape.Type == typeString:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(must[string](t, v)))
		default:
			restxml.WriteResponse(w, env, status, func(xw *restxml.Writer) {
				encodeXMLValue(t, m, pm, orDefault(pm.loc, pm.name), v, xw)
			})
		}

		return
	}

	hasBody := false

	for _, mm := range m.members(out.shape) {
		if _, ok := result[mm.name]; ok && inBody(mm) {
			hasBody = true
		}
	}

	if !hasBody {
		restxml.WriteResponse(w, env, status, nil)

		return
	}

	restxml.WriteResponse(w, env, status, func(xw *restxml.Writer) {
		encodeXMLValue(t, m, out, orDefault(out.loc, "Response"), result, xw)
	})
}

// encodeXMLMember writes one member of a structure under its wire name.
func encodeXMLMember(t *testing.T, m model, mm member, v any, xw *restxml.Writer) {
	t.Helper()

	name := mm.serial()

	switch mm.shape.Type {
	case typeList:
		el := m.resolve("", mm.shape.Member)
		if !mm.flattened {
			encodeXMLValue(t, m, mm, name, v, xw)

			return
		}

		for _, item := range must[[]any](t, v) {
			encodeXMLValue(t, m, el, orDefault(el.loc, name), item, xw)
		}
	case typeMap:
		if !mm.flattened {
			encodeXMLValue(t, m, mm, name, v, xw)

			return
		}

		for k, e := range must[map[string]any](t, v) {
			xw.Open(name)
			encodeXMLEntry(t, m, mm, k, e, xw)
			xw.Close(name)
		}
	default:
		encodeXMLValue(t, m, mm, name, v, xw)
	}
}

// encodeXMLValue writes v as the element name.
func encodeXMLValue(t *testing.T, m model, mm member, name string, v any, xw *restxml.Writer) {
	t.Helper()

	xw.Open(name)

	if ns := mm.namespace; ns != nil {
		xw.Namespace(ns.Prefix, ns.URI)
	}

	switch mm.shape.Type {
	case typeStructure:
		fields, _ := v.(map[string]any)
		members := m.members(mm.shape)

		for _, f := range members {
			if fv, ok := fields[f.name]; ok && fv != nil && f.attribute {
				xw.Attr(f.loc, must[string](t, fv))
			}
		}

		for _, f := range members {
			if fv, ok := fields[f.name]; ok && fv != nil && !f.attribute && inBody(f) {
				encodeXMLMember(t, m, f, fv, xw)
			}
		}
	case typeList:
		el := m.resolve("", mm.shape.Member)
		for _, item := range must[[]any](t, v) {
			encodeXMLValue(t, m, el, orDefault(el.loc, "member"), item, xw)
		}
	case typeMap:
		for k, e := range must[map[string]any](t, v) {
			xw.Open("entry")
			encodeXMLEntry(t, m, mm, k, e, xw)
			xw.Close("entry")
		}
	default:
		xw.Text(scalarText(t, mm, v))
	}

	xw.Close(name)
}

func encodeXMLEntry(t *testing.T, m model, mm member, k string, v any, xw *restxml.Writer) {
	t.Helper()

	key, value := m.resolve("", mm.shape.Key), m.resolve("", mm.shape.Value)
	encodeXMLValue(t, m, key, orDefault(key.loc, "key"), k, xw)
	encodeXMLValue(t, m, value, orDefault(value.loc, "value"), v, xw)
}

// scalarText formats a scalar the way the Writer's typed methods do.
func scalarText(t *testing.T, mm member, v any) string {
	t.Helper()

	xw := restxml.NewWriter()

	switch mm.shape.Type {
	case typeInteger, typeLong:
		xw.Int("v", int64(number(t, v)))
	case typeFloat, typeDouble:
		xw.Float("v", number(t, v))
	case typeBoolean:
		xw.Bool("v", must[bool](t, v))
	case typeTimestamp:
		xw.Time("v", timeOf(t, v), mm.tsfmt)
	case typeBlob:
		xw.Blob("v", []byte(must[string](t, v)))
	default:
		return must[string](t, v)
	}

	text := strings.TrimSuffix(strings.TrimPrefix(string(xw.Bytes()), "<v>"), "</v>")
	if text == "<v/>" {
		return ""
	}

	return text
}

// xmlNode is an element as botocore's parser sees it: namespaces resolved
// away, attributes, text and children.
type xmlNode struct {
	name     string
	attrs    []xml.Attr
	text     string
	children []*xmlNode
}

func (n *xmlNode) childrenNamed(name string) []*xmlNode {
	var out []*xmlNode

	for _, c := range n.children {
		if c.name == name {
			out = append(out, c)
		}
	}

	return out
}

// parseXMLTree reads a body with encoding/xml, independently of the restxml
// Decoder. An empty body is nil.
func parseXMLTree(t *testing.T, body []byte) *xmlNode {
	t.Helper()

	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	dec := xml.NewDecoder(bytes.NewReader(body))

	var stack []*xmlNode

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			t.Fatalf("no root element in %s", body)
		}

		require.NoError(t, err)

		switch tk := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{name: tk.Name.Local, attrs: tk.Attr}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			}

			stack = append(stack, n)
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(tk)
			}
		case xml.EndElement:
			n := stack[len(stack)-1]
			if stack = stack[:len(stack)-1]; len(stack) == 0 {
				return n
			}
		default:
		}
	}
}

// parseXMLResponse reads a response the way botocore's rest-xml parser does.
func parseXMLResponse(t *testing.T, m model, out member, h http.Header, status int, body []byte) map[string]any {
	t.Helper()

	got := parseHeaders(t, m, out, h, status)

	if p := out.shape.Payload; p != "" {
		pm := m.resolve(p, out.shape.Members[p])

		switch pm.shape.Type {
		case typeBlob, typeString:
			if len(body) > 0 {
				got[p] = string(body)
			}
		default:
			if root := parseXMLTree(t, body); root != nil {
				got[p] = parseXMLValue(t, m, pm, root)
			}
		}

		return got
	}

	if root := parseXMLTree(t, body); root != nil {
		maps.Copy(got, parseXMLStruct(t, m, out, root))
	}

	return got
}

func parseXMLStruct(t *testing.T, m model, s member, n *xmlNode) map[string]any {
	t.Helper()

	out := map[string]any{}

	for _, mm := range m.members(s.shape) {
		if !inBody(mm) {
			continue
		}

		if mm.attribute {
			local := mm.loc[strings.LastIndexByte(mm.loc, ':')+1:]
			for _, a := range n.attrs {
				if a.Name.Local == local {
					out[mm.name] = a.Value
				}
			}

			continue
		}

		key := mm.serial()
		el := mm
		if mm.shape.Type == typeList && mm.flattened {
			el = m.resolve("", mm.shape.Member)
			key = orDefault(el.loc, key)
		}

		nodes := n.childrenNamed(key)
		if len(nodes) == 0 {
			continue
		}

		switch {
		case mm.shape.Type == typeList && mm.flattened:
			items := make([]any, 0, len(nodes))
			for _, c := range nodes {
				items = append(items, parseXMLValue(t, m, el, c))
			}

			out[mm.name] = items
		case mm.shape.Type == typeMap && mm.flattened:
			entries := map[string]any{}
			for _, c := range nodes {
				parseXMLEntry(t, m, mm, c, entries)
			}

			out[mm.name] = entries
		default:
			out[mm.name] = parseXMLValue(t, m, mm, nodes[0])
		}
	}

	return out
}

func parseXMLValue(t *testing.T, m model, mm member, n *xmlNode) any {
	t.Helper()

	switch mm.shape.Type {
	case typeStructure:
		return parseXMLStruct(t, m, mm, n)
	case typeList:
		el := m.resolve("", mm.shape.Member)
		items := []any{}

		for _, c := range n.children {
			items = append(items, parseXMLValue(t, m, el, c))
		}

		return items
	case typeMap:
		entries := map[string]any{}
		for _, c := range n.childrenNamed("entry") {
			parseXMLEntry(t, m, mm, c, entries)
		}

		return entries
	case typeString:
		return n.text
	case typeBlob:
		return parseScalar(t, mm, n.text, rest.TimeISO8601)
	default:
		if strings.TrimSpace(n.text) == "" {
			return nil
		}

		return parseScalar(t, mm, n.text, rest.TimeISO8601)
	}
}

func parseXMLEntry(t *testing.T, m model, mm member, n *xmlNode, entries map[string]any) {
	t.Helper()

	key, value := m.resolve("", mm.shape.Key), m.resolve("", mm.shape.Value)

	var k string
	if kn := n.childrenNamed(orDefault(key.loc, "key")); len(kn) > 0 {
		k = kn[0].text
	}

	var v any
	if vn := n.childrenNamed(orDefault(value.loc, "value")); len(vn) > 0 {
		v = parseXMLValue(t, m, value, vn[0])
	}

	entries[k] = v
}

// checkXMLError writes the case's error with restxml.WriteError, its modeled
// members included, and reads it back as botocore does.
func checkXMLError(t *testing.T, m model, c *fixtureCase) {
	t.Helper()

	em := m.resolve("", &c.Given.Errors[0])
	fields, _ := jsonDynamic(t, c.Error).(map[string]any)

	rec := httptest.NewRecorder()
	encodeBindings(t, m, em, rec.Header(), fields, 0)
	restxml.WriteError(rec, &restxml.Envelope{RequestID: requestID}, &awserr.Error{
		Code: c.ErrorCode, Message: c.ErrorMessage, StatusCode: c.Response.StatusCode, Fault: awserr.FaultSender,
	}, func(xw *restxml.Writer) {
		for _, f := range m.members(em.shape) {
			if v, ok := fields[f.name]; ok && inBody(f) && f.name != messageKey {
				encodeXMLMember(t, m, f, v, xw)
			}
		}
	})

	require.Equal(t, c.Response.StatusCode, rec.Code)

	want := map[string]any{"Error": map[string]any{codeKey: c.ErrorCode, messageKey: c.ErrorMessage}}
	maps.Copy(want, fields)

	got := parseXMLError(t, m, em, rec.Header(), rec.Body.Bytes())
	assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty))

	fromFixture := parseXMLError(t, m, em, wireHeader(c.Response.Headers), []byte(c.Response.Body))
	assert.Equal(t, canonical(want, keepEmpty), canonical(fromFixture, keepEmpty),
		"what a client parses from the fixture")
}

func parseXMLError(t *testing.T, m model, em member, h http.Header, body []byte) map[string]any {
	t.Helper()

	root := parseXMLTree(t, body)
	require.NotNil(t, root)

	errNode := root
	if root.name == "ErrorResponse" {
		errNode = root.childrenNamed("Error")[0]
	}

	text := func(name string) string {
		if nodes := errNode.childrenNamed(name); len(nodes) > 0 {
			return nodes[0].text
		}

		return ""
	}

	got := parseHeaders(t, m, em, h, 0)
	maps.Copy(got, parseXMLStruct(t, m, em, errNode))

	got["Error"] = map[string]any{codeKey: text(codeKey), messageKey: text(messageKey)}

	return got
}

const s3RequestID = "req-1"

// Test_RestXML_S3Error pins the S3 envelope, which no fixture covers.
func Test_RestXML_S3Error(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	restxml.WriteS3Error(rec, &restxml.Envelope{RequestID: s3RequestID}, &awserr.Error{
		Code: "NoSuchBucket", StatusCode: http.StatusNotFound, Fault: awserr.FaultSender,
	}, func(xw *restxml.Writer) { xw.String("BucketName", "b&b") })

	require.Equal(t, http.StatusNotFound, rec.Code)
	//nolint:staticcheck // SA1008: the name is sent as written, so it is read back that way
	assert.Equal(t, []string{s3RequestID}, rec.Header()["x-amz-request-id"])

	root := parseXMLTree(t, rec.Body.Bytes())
	require.Equal(t, "Error", root.name)

	got := map[string]string{}
	for _, c := range root.children {
		got[c.name] = c.text
	}

	assert.Equal(t, map[string]string{
		codeKey: "NoSuchBucket", messageKey: "", "RequestId": s3RequestID, "BucketName": "b&b",
	}, got)
	assert.Equal(t, codeKey, root.children[0].name)
}
