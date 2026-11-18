package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Where a rest protocol binds a member, the values of its "location" trait. A
// member without one is in the body.
const (
	// LocationHeader is one header, named by locationName.
	LocationHeader = "header"
	// LocationHeaders is a map member holding every header whose name starts
	// with its locationName, x-amz-meta- in S3.
	LocationHeaders = "headers"
	// LocationQuery is one query parameter, named by locationName.
	LocationQuery = "querystring"
	// LocationURI is a label of the request URI path, named by locationName.
	LocationURI = "uri"
	// LocationStatusCode is the status code of the response.
	LocationStatusCode = "statusCode"
)

// Ref points at a shape and may add traits of its own. A trait on the Ref
// overrides the same trait on the shape.
type Ref struct {
	Shape           string        `json:"shape"`
	Location        string        `json:"location"`
	LocationName    string        `json:"locationName"`
	XMLNamespace    *XMLNamespace `json:"xmlNamespace"`
	XMLAttribute    bool          `json:"xmlAttribute"`
	Flattened       bool          `json:"flattened"`
	TimestampFormat string        `json:"timestampFormat"`
	JSONValue       bool          `json:"jsonvalue"`
	ResultWrapper   string        `json:"resultWrapper"`
	Streaming       bool          `json:"streaming"`
	HostLabel       bool          `json:"hostLabel"`
	EventPayload    bool          `json:"eventpayload"`
	EventHeader     bool          `json:"eventheader"`
	ContextParam    *ContextParam `json:"contextParam"`
}

// XMLNamespace is the "xmlNamespace" trait: the namespace an XML element
// declares, with the prefix it declares it under, if any. Service models write
// it as an object; botocore's protocol fixtures also write the bare URI.
type XMLNamespace struct {
	Prefix string `json:"prefix"`
	URI    string `json:"uri"`
}

// UnmarshalJSON reads the object form and the bare URI form.
func (n *XMLNamespace) UnmarshalJSON(data []byte) error {
	var uri string

	if json.Unmarshal(data, &uri) == nil {
		*n = XMLNamespace{URI: uri}

		return nil
	}

	type plain XMLNamespace

	err := json.Unmarshal(data, (*plain)(n))
	if err != nil {
		return fmt.Errorf("read xmlNamespace: %w", err)
	}

	return nil
}

// ErrorTrait is the "error" trait of an exception shape.
type ErrorTrait struct {
	Code           string `json:"code"`
	HTTPStatusCode int    `json:"httpStatusCode"`
	SenderFault    bool   `json:"senderFault"`
}

// Shape is one entry of "shapes".
type Shape struct {
	Type            string        `json:"type"`
	Members         Members       `json:"members"`
	Required        []string      `json:"required"`
	Member          *Ref          `json:"member"`
	Key             *Ref          `json:"key"`
	Value           *Ref          `json:"value"`
	LocationName    string        `json:"locationName"`
	XMLNamespace    *XMLNamespace `json:"xmlNamespace"`
	Flattened       bool          `json:"flattened"`
	TimestampFormat string        `json:"timestampFormat"`
	JSONValue       bool          `json:"jsonvalue"`
	Exception       bool          `json:"exception"`
	Error           *ErrorTrait   `json:"error"`
	Enum            []string      `json:"enum"`

	// Payload names the member a rest protocol sends as the whole body, instead
	// of the body members wrapped in the structure.
	Payload string `json:"payload"`
	// Streaming marks a blob or string sent as a stream of unknown length;
	// RequiresLength says that stream still needs a Content-Length.
	Streaming      bool `json:"streaming"`
	RequiresLength bool `json:"requiresLength"`
	// EventStream marks a structure whose members are the events of an event
	// stream, and Event each event's structure.
	EventStream bool `json:"eventstream"`
	Event       bool `json:"event"`
}

// IsRequired reports whether the structure lists name as required.
func (s *Shape) IsRequired(name string) bool {
	return slices.Contains(s.Required, name)
}

// Member is one member of a structure shape.
type Member struct {
	Name string
	Ref  Ref
}

// Members lists the members of a structure in the order the spec writes them.
type Members []Member

// UnmarshalJSON reads a json object keeping the order of its keys.
func (m *Members) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))

	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("read members: %w", err)
	}

	if tok == nil {
		return nil
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errors.New("members is not an object")
	}

	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return fmt.Errorf("read member name: %w", err)
		}

		name, ok := key.(string)
		if !ok {
			return errors.New("member name is not a string")
		}

		var ref Ref

		err = dec.Decode(&ref)
		if err != nil {
			return fmt.Errorf("read member %s: %w", name, err)
		}

		*m = append(*m, Member{Name: name, Ref: ref})
	}

	return nil
}

// Lookup returns the member with the given name.
func (m *Members) Lookup(name string) (Member, bool) {
	for _, member := range *m {
		if member.Name == name {
			return member, true
		}
	}

	return Member{}, false
}
