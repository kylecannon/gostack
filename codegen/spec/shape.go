package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Ref points at a shape and may add traits of its own. A trait on the Ref
// overrides the same trait on the shape.
type Ref struct {
	Shape           string `json:"shape"`
	LocationName    string `json:"locationName"`
	Flattened       bool   `json:"flattened"`
	TimestampFormat string `json:"timestampFormat"`
	JSONValue       bool   `json:"jsonvalue"`
	ResultWrapper   string `json:"resultWrapper"`
}

// ErrorTrait is the "error" trait of an exception shape.
type ErrorTrait struct {
	Code           string `json:"code"`
	HTTPStatusCode int    `json:"httpStatusCode"`
	SenderFault    bool   `json:"senderFault"`
}

// Shape is one entry of "shapes".
type Shape struct {
	Type            string      `json:"type"`
	Members         Members     `json:"members"`
	Required        []string    `json:"required"`
	Member          *Ref        `json:"member"`
	Key             *Ref        `json:"key"`
	Value           *Ref        `json:"value"`
	LocationName    string      `json:"locationName"`
	Flattened       bool        `json:"flattened"`
	TimestampFormat string      `json:"timestampFormat"`
	JSONValue       bool        `json:"jsonvalue"`
	Exception       bool        `json:"exception"`
	Error           *ErrorTrait `json:"error"`
	Enum            []string    `json:"enum"`
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
