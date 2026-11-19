package rest_test

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	xmlgen "github.com/kylecannon/gostack/internal/protocol/restxml/fixturegen_test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_RestXML_Input(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-xml", "input", checkInput)
}

func Test_RestJSON_Input(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-json", "input", checkInput)
}

// checkInput serves the case's serialized request and compares the input the
// generated reader decoded with params.
func checkInput(t *testing.T, m model, c *fixtureCase, gen xmlgen.Group) {
	t.Helper()

	rec, in := serveGenerated(gen, buildRequest(t, c), "", nil, nil)
	require.NotNil(t, in, "the generated service answered %d: %s", rec.Code, rec.Body)

	got := map[string]any{}
	if c.Given.Input != nil {
		got, _ = fromGo(t, m, m.resolve("", c.Given.Input), reflect.ValueOf(in)).(map[string]any)
	}

	want, _ := jsonDynamic(t, c.Params).(map[string]any)
	if want == nil {
		want = map[string]any{}
	}

	if c.Given.Input != nil {
		queryMapsMatch(t, m, m.resolve("", c.Given.Input), want, got)
	}

	assert.Equal(t, canonical(want, dropEmpty), canonical(got, dropEmpty))
}

func Test_RestXML_Output(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-xml", "output", func(t *testing.T, m model, c *fixtureCase, gen xmlgen.Group) {
		checkOutput(t, m, c, gen, func(out member, h http.Header, status int, body []byte) map[string]any {
			return parseXMLResponse(t, m, out, h, status, body)
		})
	})
}

func Test_RestJSON_Output(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-json", "output", func(t *testing.T, m model, c *fixtureCase, gen xmlgen.Group) {
		checkOutput(t, m, c, gen, func(out member, h http.Header, status int, body []byte) map[string]any {
			got := parseHeaders(t, m, out, h, status)
			maps.Copy(got, decodeJSONBody(t, m, out, body, inBody))

			return got
		})
	})
}

// responseParser reads a response as botocore does.
type responseParser func(out member, h http.Header, status int, body []byte) map[string]any

// checkOutput fills the generated output from the case's result, or makes its
// error, has the generated writer send it, and compares what a client parses
// from that with result, and with what it parses from the fixture's response.
func checkOutput(t *testing.T, m model, c *fixtureCase, gen xmlgen.Group, parse responseParser) {
	t.Helper()

	if len(c.Given.Errors) > 0 {
		checkError(t, m, c, gen)

		return
	}

	out := gen.NewOutput()
	result, _ := jsonDynamic(t, c.Result).(map[string]any)

	if c.Given.Output != nil {
		toGo(t, m, m.resolve("", c.Given.Output), result, reflect.ValueOf(out).Elem())
	}

	rec, _ := serveGenerated(gen, outputRequest(t, c), c.Given.Name, out, nil)
	got, want := map[string]any{}, map[string]any{}

	if c.Given.Output != nil {
		om := m.resolve("", c.Given.Output)
		got = parse(om, rec.Header(), rec.Code, rec.Body.Bytes())
		want = parse(om, wireHeader(c.Response.Headers), c.Response.StatusCode, []byte(c.Response.Body))
	}

	require.Equal(t, c.Response.StatusCode, rec.Code)
	assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty), "what a client parses from the fixture")

	if result == nil {
		result = map[string]any{}
	}

	assert.Equal(t, canonical(result, keepEmpty), canonical(got, keepEmpty))
}

// checkError has the generated service write the case's error, its modeled
// members included, and reads it back as botocore does.
func checkError(t *testing.T, m model, c *fixtureCase, gen xmlgen.Group) {
	t.Helper()

	ref := &c.Given.Errors[0]
	for i := range c.Given.Errors {
		if c.Given.Errors[i].Shape == c.ErrorCode {
			ref = &c.Given.Errors[i]
		}
	}

	em := m.resolve("", ref)
	fields, _ := jsonDynamic(t, c.Error).(map[string]any)

	members, build := gen.NewError(ref.Shape)
	require.NotNil(t, build, "the generated service has no error %s", ref.Shape)

	if members != nil {
		toGo(t, m, em, fields, reflect.ValueOf(members).Elem())
	}

	rec, _ := serveGenerated(gen, outputRequest(t, c), c.Given.Name, nil, build(c.ErrorMessage))
	require.Equal(t, c.Response.StatusCode, rec.Code)

	parse := parseXMLError
	if gen.Metadata.Protocols[0] != xmlgen.Input[0].Metadata.Protocols[0] {
		parse = parseJSONError
	}

	want := map[string]any{"Error": map[string]any{codeKey: c.ErrorCode, messageKey: c.ErrorMessage}}
	maps.Copy(want, fields)

	got := parse(t, m, em, rec.Header(), rec.Body.Bytes())
	assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty))

	fromFixture := parse(t, m, em, wireHeader(c.Response.Headers), []byte(c.Response.Body))
	assert.Equal(t, canonical(want, keepEmpty), canonical(fromFixture, keepEmpty),
		"what a client parses from the fixture")
}

// outputRequest is the request an output case answers. The case names its
// operation, so the request only has to carry the method.
func outputRequest(t *testing.T, c *fixtureCase) *http.Request {
	t.Helper()

	return httptest.NewRequestWithContext(t.Context(), c.Given.HTTP.Method, "/", http.NoBody)
}
