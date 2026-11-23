package sqs

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The content negotiation of a query-protocol answer: LocalStack picks its
// type from the Accept header, and a queue-URL call and the developer API
// both use it.
const (
	mimeXML    = "text/xml"
	mimeAppXML = "application/xml"
	mimeJSON   = "application/json"
	headerType = "Content-Type"
	headerAcc  = "Accept"

	// The specificity of an Accept entry that matches a type: */* is the
	// loosest and the exact type the tightest.
	matchAny   = 0
	matchGroup = 1
	matchExact = 2
	noMatch    = -1

	// qualityScale is the quality 1 of an Accept entry, in thousandths.
	qualityScale = 1000
)

// supportedMimes are the response types of a query-protocol answer, in the
// order LocalStack's serializer prefers them when the caller has no preference.
var supportedMimes = []string{mimeXML, mimeAppXML, mimeJSON}

// heldResponse keeps a response so it can be rewritten before it is sent.
type heldResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header { return h.header }

func (h *heldResponse) WriteHeader(status int) { h.status = status }

func (h *heldResponse) Write(p []byte) (int, error) {
	// A bytes.Buffer never fails to write.
	n, _ := h.body.Write(p)

	return n, nil
}

// negotiateMime is LocalStack's _get_mime_type: the best of the types a
// query-protocol answer can take for the Accept header, or for the Content-Type
// of the call when there is no Accept header worth reading.
func negotiateMime(r *http.Request) string {
	accept := r.Header.Get(headerAcc)

	if accept == "" || accept == "*/*" {
		contentType := r.Header.Get(headerType)
		if contentType == "" {
			return supportedMimes[0]
		}

		accept = contentType + ", */*"
	}

	best, bestQuality, bestSpecificity := supportedMimes[0], -1, noMatch

	for _, supported := range supportedMimes {
		for _, e := range parseAccept(accept) {
			specificity := matchSpecificity(e.media, supported)
			if e.quality <= 0 || specificity == noMatch {
				continue
			}

			if e.quality > bestQuality || (e.quality == bestQuality && specificity > bestSpecificity) {
				best, bestQuality, bestSpecificity = supported, e.quality, specificity
			}
		}
	}

	return best
}

// acceptEntry is one media range of an Accept header, with its quality in
// thousandths.
type acceptEntry struct {
	media   string
	quality int
}

// parseAccept reads the media ranges of an Accept header. A range with no q
// parameter, or one that is not a number, has the quality 1.
func parseAccept(accept string) []acceptEntry {
	var entries []acceptEntry

	for item := range strings.SplitSeq(accept, ",") {
		media, params, _ := strings.Cut(item, ";")
		entries = append(entries, acceptEntry{
			media:   strings.ToLower(strings.TrimSpace(media)),
			quality: acceptQuality(params),
		})
	}

	return entries
}

// acceptQuality reads the q parameter of a media range's parameters.
func acceptQuality(params string) int {
	for param := range strings.SplitSeq(params, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if !found || key != "q" {
			continue
		}

		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			break
		}

		return int(parsed * qualityScale)
	}

	return qualityScale
}

// matchSpecificity tells how an Accept entry matches a type: exactly, by its
// group ("text/*"), by "*/*", or not at all.
func matchSpecificity(entry, supported string) int {
	switch {
	case entry == supported:
		return matchExact
	case entry == "*/*" || entry == "*":
		return matchAny
	case strings.HasSuffix(entry, "/*") && strings.HasPrefix(supported, strings.TrimSuffix(entry, "*")):
		return matchGroup
	default:
		return noMatch
	}
}

// xmlNode is an element of a document read by xmlToJSON.
type xmlNode struct {
	name     string
	children []*xmlNode
	text     strings.Builder
}

// xmlToJSON converts an XML document to the JSON that LocalStack answers with
// when a query-protocol caller asks for application/json, which is what
// xmltodict makes of it: the root element names the only key, an element with
// children is an object, a repeated child is an array, an element with only
// text is a string, an empty element is null, and namespaces and attributes
// are dropped.
func xmlToJSON(document []byte) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(document))

	var (
		root  *xmlNode
		stack []*xmlNode
	)

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // the caller falls back to the XML.
		}

		switch t := token.(type) {
		case xml.StartElement:
			node := &xmlNode{name: t.Name.Local}

			if len(stack) == 0 {
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}

			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		default:
			// Declarations, comments and processing instructions carry nothing.
		}
	}

	if root == nil {
		return nil, io.ErrUnexpectedEOF
	}

	out := []byte{'{'}
	out = appendJSONString(out, root.name)
	out = append(out, ':')
	out = root.appendJSON(out)

	return append(out, '}'), nil
}

func (n *xmlNode) appendJSON(out []byte) []byte {
	if len(n.children) == 0 {
		if n.text.Len() == 0 {
			return append(out, "null"...)
		}

		return appendJSONString(out, n.text.String())
	}

	var order []string

	groups := map[string][]*xmlNode{}

	for _, child := range n.children {
		if _, seen := groups[child.name]; !seen {
			order = append(order, child.name)
		}

		groups[child.name] = append(groups[child.name], child)
	}

	out = append(out, '{')

	for i, name := range order {
		if i > 0 {
			out = append(out, ',')
		}

		out = appendJSONString(out, name)
		out = append(out, ':')

		group := groups[name]
		if len(group) == 1 {
			out = group[0].appendJSON(out)

			continue
		}

		out = append(out, '[')

		for j, child := range group {
			if j > 0 {
				out = append(out, ',')
			}

			out = child.appendJSON(out)
		}

		out = append(out, ']')
	}

	return append(out, '}')
}

func appendJSONString(out []byte, s string) []byte {
	encoded, err := json.Marshal(s)
	if err != nil {
		// A string always marshals; this keeps the document valid regardless.
		return append(out, `""`...)
	}

	return append(out, encoded...)
}
