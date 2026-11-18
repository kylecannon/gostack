package spec

import (
	"encoding/json"
	"fmt"
)

// FixtureGroup is one group of botocore's protocol fixtures
// (tests/unit/protocols/{input,output}/<protocol>.json): a small service
// model, its shapes, and the cases that exercise it.
type FixtureGroup struct {
	Description string            `json:"description"`
	Metadata    Metadata          `json:"metadata"`
	Shapes      map[string]*Shape `json:"shapes"`
	Cases       []FixtureCase     `json:"cases"`
}

// FixtureCase is one case of a group. An input case gives the params a client
// was handed and the request it serializes; an output case gives the response
// a service sent and the result, or the error, a client parses from it.
type FixtureCase struct {
	ID           string          `json:"id"`
	Given        Operation       `json:"given"`
	Params       json.RawMessage `json:"params"`
	Serialized   FixtureRequest  `json:"serialized"`
	Response     FixtureResponse `json:"response"`
	Result       json.RawMessage `json:"result"`
	Error        json.RawMessage `json:"error"`
	ErrorCode    string          `json:"errorCode"`
	ErrorMessage string          `json:"errorMessage"`
}

// FixtureRequest is the request an input case serializes.
type FixtureRequest struct {
	Method  string            `json:"method"`
	URI     string            `json:"uri"`
	Body    string            `json:"body"`
	Host    string            `json:"host"`
	Headers map[string]string `json:"headers"`
}

// FixtureResponse is the response an output case parses.
type FixtureResponse struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// ParseFixtures reads the groups of one fixture file.
func ParseFixtures(data []byte) ([]FixtureGroup, error) {
	var groups []FixtureGroup

	err := json.Unmarshal(data, &groups)
	if err != nil {
		return nil, fmt.Errorf("parse fixtures: %w", err)
	}

	return groups, nil
}
