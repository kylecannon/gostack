// Package spec reads the service models gostack generates code from: botocore
// service-2.json files, the JSON patches LocalStack applies to them, and
// LocalStack's own sqs-query model.
//
// It is the model of the generator, and the protocol conformance tests reuse it
// to read the shape definitions in botocore's protocol fixtures, which use the
// same format.
package spec

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Shape types, as botocore spells them.
const (
	TypeStructure = "structure"
	TypeList      = "list"
	TypeMap       = "map"
	TypeString    = "string"
	TypeInteger   = "integer"
	TypeLong      = "long"
	TypeFloat     = "float"
	TypeDouble    = "double"
	TypeBoolean   = "boolean"
	TypeTimestamp = "timestamp"
	TypeBlob      = "blob"
)

// Service is one service-2.json document.
type Service struct {
	Metadata   Metadata              `json:"metadata"`
	Operations map[string]*Operation `json:"operations"`
	Shapes     map[string]*Shape     `json:"shapes"`
}

// Metadata is the "metadata" object of a service.
type Metadata struct {
	APIVersion     string   `json:"apiVersion"`
	EndpointPrefix string   `json:"endpointPrefix"`
	SigningName    string   `json:"signingName"`
	TargetPrefix   string   `json:"targetPrefix"`
	JSONVersion    string   `json:"jsonVersion"`
	Protocol       string   `json:"protocol"`
	Protocols      []string `json:"protocols"`
	XMLNamespace   string   `json:"xmlNamespace"`

	// QueryCompatible is the awsQueryCompatible marker. It is an empty object in
	// the spec, so its presence is what counts.
	QueryCompatible json.RawMessage `json:"awsQueryCompatible"`
}

// IsQueryCompatible reports whether the spec carries awsQueryCompatible.
func (m *Metadata) IsQueryCompatible() bool {
	return len(m.QueryCompatible) > 0
}

// Operation is one entry of "operations".
type Operation struct {
	Name     string `json:"name"`
	AuthType string `json:"authtype"`
	Input    *Ref   `json:"input"`
	Output   *Ref   `json:"output"`
	Errors   []Ref  `json:"errors"`
}

// Parse reads a service model from json.
func Parse(r io.Reader) (*Service, error) {
	var svc Service

	err := json.NewDecoder(r).Decode(&svc)
	if err != nil {
		return nil, fmt.Errorf("parse service model: %w", err)
	}

	return &svc, nil
}

// ParseBytes reads a service model from json bytes.
func ParseBytes(data []byte) (*Service, error) {
	return Parse(strings.NewReader(string(data)))
}
