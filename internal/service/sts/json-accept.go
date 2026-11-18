package sts

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A caller of a query service can ask for JSON with "Accept: application/json".
// LocalStack answers with the XML document converted by xmltodict: every value
// is a string, repeated elements become a list and the namespace is dropped.
// The one difference is a timestamp, which is epoch seconds as a whole number.
// STS is the first service where a test relies on it, and its only timestamp
// member is named Expiration.

const (
	mimeJSON = "application/json"
	// timestampMember is the name of the only timestamp in an STS response.
	timestampMember = "Expiration"
)

func acceptsJSON(r *http.Request) bool {
	return r != nil && strings.Contains(strings.ToLower(r.Header.Get("Accept")), mimeJSON)
}

// bufferedResponse holds a response until serveJSON has converted it.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)

	return b.body.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails.
}

// serveJSON runs respond against a buffer and writes what it produced to w as
// JSON. A response that is not XML is written as it is.
func serveJSON(w http.ResponseWriter, respond func(http.ResponseWriter)) {
	buffered := &bufferedResponse{header: make(http.Header)}
	respond(buffered)

	body := buffered.body.Bytes()

	converted, err := xmlToJSON(body)
	if err == nil {
		body = converted
		buffered.header.Set("Content-Type", mimeJSON)
	}

	maps.Copy(w.Header(), buffered.header)

	w.WriteHeader(max(buffered.status, http.StatusOK))
	_, _ = w.Write(body)
}

// element is one XML element: its text, and its children in document order.
type element struct {
	name     string
	text     strings.Builder
	children []*element
}

// xmlToJSON converts an XML document to {"<root>": <value>}.
func xmlToJSON(document []byte) ([]byte, error) {
	root, err := parseElements(document)
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]any{root.name: root.value()}) //nolint:wrapcheck // the caller only tests for failure.
}

func parseElements(document []byte) (*element, error) {
	decoder := xml.NewDecoder(bytes.NewReader(document))

	var (
		root  *element
		stack []*element
	)

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // the caller only tests for failure.
		}

		switch t := token.(type) {
		case xml.StartElement:
			el := &element{name: t.Name.Local}
			if len(stack) == 0 {
				root = el
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, el)
			}

			stack = append(stack, el)
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		default:
			// A declaration, comment or directive carries no data.
		}
	}

	if root == nil {
		return nil, io.ErrUnexpectedEOF
	}

	return root, nil
}

// value is what an element becomes in JSON: its text when it has no children
// (null when empty), otherwise an object of its children, with a list for a
// name that repeats.
func (e *element) value() any {
	if len(e.children) == 0 {
		text := strings.TrimSpace(e.text.String())
		if text == "" {
			return nil
		}

		if e.name == timestampMember {
			at, err := time.Parse(time.RFC3339Nano, text)
			if err == nil {
				return strconv.FormatInt(at.Unix(), 10)
			}
		}

		return text
	}

	members := make(map[string]any, len(e.children))

	for _, child := range e.children {
		member := child.value()

		existing, seen := members[child.name]
		if !seen {
			members[child.name] = member

			continue
		}

		if list, isList := existing.([]any); isList {
			members[child.name] = append(list, member)
		} else {
			members[child.name] = []any{existing, member}
		}
	}

	return members
}
