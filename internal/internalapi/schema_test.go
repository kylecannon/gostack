package internalapi_test

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/api/openapi"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const (
	localstackSpecPath = "../../specs/localstack/openapi.yaml"
	specResource       = "openapi.json"
)

// spec is an OpenAPI 3.1 document whose response and request schemas can check a
// body. OpenAPI 3.1 schemas are JSON Schema 2020-12, so a JSON Schema library
// resolves the $refs into components.
type spec struct {
	compiler *jsonschema.Compiler
}

// loadSpec reads an OpenAPI document in YAML.
func loadSpec(t *testing.T, data []byte) *spec {
	t.Helper()
	var document any
	require.NoError(t, yaml.Unmarshal(data, &document))
	asJSON, err := json.Marshal(document)
	require.NoError(t, err)
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource(specResource, parsed))

	return &spec{compiler: compiler}
}

func loadLocalstackSpec(t *testing.T) *spec {
	t.Helper()
	data, err := os.ReadFile(localstackSpecPath)
	require.NoError(t, err)

	return loadSpec(t, data)
}

func loadGostackSpec(t *testing.T) *spec {
	t.Helper()

	return loadSpec(t, openapi.Spec)
}

// pointerToken escapes one JSON pointer token for use in a URL fragment.
func pointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")

	return url.PathEscape(token)
}

// schemaAt compiles the schema of a response ("responses/200/content/<type>") or
// a request body of an operation.
func (s *spec) schemaAt(t *testing.T, path, method, section, mediaType string) *jsonschema.Schema {
	t.Helper()
	pointer := specResource + "#/paths/" + pointerToken(path) + "/" + method + "/" + section +
		"/content/" + pointerToken(mediaType) + "/schema"
	schema, err := s.compiler.Compile(pointer)
	require.NoError(t, err, "no schema at %s", pointer)

	return schema
}

// validateResponse checks that body, a JSON document, is valid for the schema
// the spec gives the operation's JSON response with the status.
func (s *spec) validateResponse(t *testing.T, path, method, status string, body []byte) {
	t.Helper()
	validateAgainst(t, s.schemaAt(t, path, method, "responses/"+status, "application/json"), body)
}

// validateRequest checks a request body against the operation's JSON request
// schema.
func (s *spec) validateRequest(t *testing.T, path, method string, body []byte) {
	t.Helper()
	validateAgainst(t, s.schemaAt(t, path, method, "requestBody", "application/json"), body)
}

func validateAgainst(t *testing.T, schema *jsonschema.Schema, body []byte) {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	require.NoError(t, err, "the body is not JSON: %s", body)
	require.NoError(t, schema.Validate(instance), "body %s", body)
}

// Test_spec_rejectsWhatTheSchemaForbids shows that the validators the alias tests
// rely on can fail: a body with a wrong enum value, a missing key or an extra key
// is rejected.
func Test_spec_rejectsWhatTheSchemaForbids(t *testing.T) {
	schema := loadLocalstackSpec(t).schemaAt(t, "/_localstack/health", "get", "responses/200", "application/json")
	tests := []struct {
		name string
		give string
	}{
		{name: "edition outside the enum", give: `{"edition": "gold", "services": {}, "version": "1"}`},
		{name: "missing version", give: `{"edition": "community", "services": {}}`},
		{name: "extra top-level key", give: `{"edition": "community", "services": {}, "version": "1", "extra": 1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance, err := jsonschema.UnmarshalJSON(strings.NewReader(tt.give))
			require.NoError(t, err)
			require.Error(t, schema.Validate(instance))
		})
	}
}

// specOperations lists the operations of the document as "METHOD path".
func specOperations(t *testing.T, data []byte) []string {
	t.Helper()
	var document struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(data, &document))
	var operations []string
	for path, item := range document.Paths {
		for method := range item {
			operations = append(operations, strings.ToUpper(method)+" "+path)
		}
	}

	return operations
}
