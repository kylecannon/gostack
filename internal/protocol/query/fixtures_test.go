package query_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/require"
)

// The botocore protocol fixtures are written from the client's side: an input
// case gives the params a client was handed and the request it serializes, and
// an output case gives the response a service sent and the result the client
// parses. The tests here run them the other way round, as a server does.

// group is a set of cases that share a service model.
type group struct {
	Description string                 `json:"description"`
	Metadata    groupMetadata          `json:"metadata"`
	Shapes      map[string]*spec.Shape `json:"shapes"`
	Cases       []fixtureCase          `json:"cases"`
}

type groupMetadata struct {
	Protocol     string `json:"protocol"`
	APIVersion   string `json:"apiVersion"`
	XMLNamespace string `json:"xmlNamespace"`
}

type fixtureCase struct {
	ID           string          `json:"id"`
	Description  string          `json:"description"`
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
	Name   string     `json:"name"`
	Input  *spec.Ref  `json:"input"`
	Output *spec.Ref  `json:"output"`
	Errors []spec.Ref `json:"errors"`
}

type serialized struct {
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

type response struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

// loadFixtures reads one fixture file, such as input/query.json.
func loadFixtures(t *testing.T, name string) []group {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "testdata", "botocore", name))
	require.NoError(t, err)

	var groups []group

	require.NoError(t, json.Unmarshal(data, &groups))

	return groups
}

// decodeDynamic reads a json document into maps, slices, strings, float64s and
// bools. A missing document is nil.
func decodeDynamic(t *testing.T, raw json.RawMessage) any {
	t.Helper()

	if len(raw) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any

	require.NoError(t, dec.Decode(&v))

	return v
}
