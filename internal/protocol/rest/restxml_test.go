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

// wireHeader holds a fixture's headers under the names as written, as they
// arrive on the wire.
func wireHeader(headers map[string]string) http.Header {
	h := http.Header{}
	for k, v := range headers {
		h[k] = []string{v}
	}

	return h
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}

	return s
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
