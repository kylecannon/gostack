package rest_test

import (
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_RestJSON_Input(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-json", "input", func(t *testing.T, m model, c *fixtureCase) {
		got := map[string]any{}

		if c.Given.Input != nil {
			req := buildRequest(t, c)
			in := m.resolve("", c.Given.Input)

			labels, ok := rest.MustParsePattern(c.Given.HTTP.RequestURI).Match(req.URL.EscapedPath())
			require.True(t, ok, "%s does not match %s", req.URL.EscapedPath(), c.Given.HTTP.RequestURI)

			d := rest.NewDecoder(req, labels)
			got = decodeBindings(t, m, in, d)
			require.Nil(t, d.Err())

			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)

			maps.Copy(got, decodeJSONBody(t, m, in, body, inRequestBody))
		}

		want, _ := jsonDynamic(t, c.Params).(map[string]any)
		if want == nil {
			want = map[string]any{}
		}

		if c.Given.Input != nil {
			queryMapsMatch(t, m, m.resolve("", c.Given.Input), want, got)
		}

		assert.Equal(t, canonical(want, dropEmpty), canonical(got, dropEmpty))
	})
}

func Test_RestJSON_Output(t *testing.T) {
	t.Parallel()

	runCases(t, "rest-json", "output", func(t *testing.T, m model, c *fixtureCase) {
		if len(c.Given.Errors) > 0 {
			checkJSONError(t, m, c)

			return
		}

		rec := httptest.NewRecorder()
		got, want := map[string]any{}, map[string]any{}

		if c.Given.Output == nil {
			rec.WriteHeader(c.Given.HTTP.ResponseCode)
		} else {
			out := m.resolve("", c.Given.Output)
			result, _ := jsonDynamic(t, c.Result).(map[string]any)

			writeJSONResponse(t, m, out, rec, c.Given.HTTP.ResponseCode, result)

			got = parseHeaders(t, m, out, rec.Header(), rec.Code)
			maps.Copy(got, decodeJSONBody(t, m, out, rec.Body.Bytes(), inBody))

			want = parseHeaders(t, m, out, wireHeader(c.Response.Headers), c.Response.StatusCode)
			maps.Copy(want, decodeJSONBody(t, m, out, []byte(c.Response.Body), inBody))
		}

		require.Equal(t, c.Response.StatusCode, rec.Code)
		assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty), "what a client parses from the fixture")

		result, _ := jsonDynamic(t, c.Result).(map[string]any)
		if result == nil {
			result = map[string]any{}
		}

		assert.Equal(t, canonical(result, keepEmpty), canonical(got, keepEmpty))
	})
}

// inRequestBody reports whether an input member travels in the body.
func inRequestBody(mm member) bool {
	return mm.location == ""
}

// decodeJSONBody reads the body members of a structure: the payload member
// when the structure names one, otherwise every member body says travels in
// the body. The json is read with awsjson, the codec generated code uses.
func decodeJSONBody(t *testing.T, m model, s member, body []byte, isBody func(member) bool) map[string]any {
	t.Helper()

	got := map[string]any{}

	target := s
	if s.shape.Payload != "" {
		pm := m.resolve(s.shape.Payload, s.shape.Members[s.shape.Payload])

		switch pm.shape.Type {
		case typeBlob, typeString:
			if len(body) > 0 {
				got[pm.name] = string(body)
			}

			return got
		default:
			target = pm
		}
	}

	if strings.TrimSpace(string(body)) == "" {
		return got
	}

	var wire any

	require.NoError(t, awsjson.Unmarshal(body, &wire))

	if target.name != "" {
		got[target.name] = fromJSON(t, m, target, wire, isBody)

		return got
	}

	v, _ := fromJSON(t, m, target, wire, isBody).(map[string]any)

	return v
}

// fromJSON turns a decoded json value into the plain value its shape stands
// for, as generated types and encoding/json/v2 read it.
func fromJSON(t *testing.T, m model, mm member, v any, isBody func(member) bool) any {
	t.Helper()

	if v == nil || mm.shape.Document {
		return v
	}

	switch mm.shape.Type {
	case typeStructure:
		fields, _ := v.(map[string]any)
		out := map[string]any{}

		for _, f := range m.members(mm.shape) {
			if fv, ok := fields[f.serial()]; ok && fv != nil && isBody(f) {
				out[f.name] = fromJSON(t, m, f, fv, inRequestBody)
			}
		}

		return out
	case typeList:
		el := m.resolve("", mm.shape.Member)
		items := []any{}

		for _, item := range must[[]any](t, v) {
			items = append(items, fromJSON(t, m, el, item, inRequestBody))
		}

		return items
	case typeMap:
		value := m.resolve("", mm.shape.Value)
		entries := map[string]any{}

		for k, e := range must[map[string]any](t, v) {
			entries[k] = fromJSON(t, m, value, e, inRequestBody)
		}

		return entries
	case typeTimestamp:
		if s, ok := v.(string); ok {
			return parseScalar(t, mm, s, rest.TimeISO8601)
		}

		return v
	case typeBlob:
		b, err := base64.StdEncoding.DecodeString(must[string](t, v))
		require.NoError(t, err)

		return string(b)
	default:
		return v
	}
}

// toJSON turns a fixture value into what encoding/json/v2 writes for the
// generated type: members under their wire names, timestamps as epoch seconds
// unless the shape says otherwise, blobs in base64.
func toJSON(t *testing.T, m model, mm member, v any) any {
	t.Helper()

	if mm.shape.Document {
		return rawNumbers(v)
	}

	switch mm.shape.Type {
	case typeStructure:
		fields, _ := v.(map[string]any)
		out := map[string]any{}

		for _, f := range m.members(mm.shape) {
			if fv, ok := fields[f.name]; ok && fv != nil && inBody(f) {
				out[f.serial()] = toJSON(t, m, f, fv)
			}
		}

		return out
	case typeList:
		el := m.resolve("", mm.shape.Member)
		items := []any{}

		for _, item := range must[[]any](t, v) {
			items = append(items, toJSON(t, m, el, item))
		}

		return items
	case typeMap:
		value := m.resolve("", mm.shape.Value)
		entries := map[string]any{}

		for k, e := range must[map[string]any](t, v) {
			entries[k] = toJSON(t, m, value, e)
		}

		return entries
	case typeTimestamp:
		s := rest.FormatTime(timeOf(t, v), mm.tsfmt, rest.TimeUnix)
		if strings.EqualFold(orDefault(mm.tsfmt, rest.TimeUnix), rest.TimeUnix) {
			return jsontext.Value(s)
		}

		return s
	case typeBlob:
		return base64.StdEncoding.EncodeToString([]byte(must[string](t, v)))
	default:
		return rawNumbers(v)
	}
}

// rawNumbers swaps each json.Number in a fixture value for the raw json text,
// so encoding/json/v2 writes the number as the fixture spelled it.
func rawNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		return jsontext.Value(x.String())
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = rawNumbers(e)
		}

		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, rawNumbers(e))
		}

		return out
	default:
		return v
	}
}

// writeJSONResponse writes result as generated code will: header members with
// rest, the body as raw bytes for a blob or string payload, otherwise as json
// with awsjson.
func writeJSONResponse(t *testing.T, m model, out member, w http.ResponseWriter, def int, result map[string]any) {
	t.Helper()

	status := encodeBindings(t, m, out, w.Header(), result, def)

	var body []byte

	if p := out.shape.Payload; p != "" {
		pm := m.resolve(p, out.shape.Members[p])

		switch v, ok := result[p]; {
		case !ok || v == nil:
		case pm.shape.Type == typeBlob || pm.shape.Type == typeString:
			body = []byte(must[string](t, v))
		default:
			body = marshalJSON(t, toJSON(t, m, pm, v))
		}
	} else if wire, _ := toJSON(t, m, out, result).(map[string]any); len(wire) > 0 {
		body = marshalJSON(t, wire)
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := awsjson.Marshal(v)
	require.NoError(t, err)

	return b
}

// checkJSONError writes the case's error with awsjson.WriteError, its modeled
// members included, and reads it back as botocore does.
func checkJSONError(t *testing.T, m model, c *fixtureCase) {
	t.Helper()

	em := m.resolve("", &c.Given.Errors[0])
	fields, _ := jsonDynamic(t, c.Error).(map[string]any)

	rec := httptest.NewRecorder()
	encodeBindings(t, m, em, rec.Header(), fields, 0)
	awsjson.WriteError(rec, nil, requestID, &awserr.Error{
		Code: c.ErrorCode, Message: c.ErrorMessage, StatusCode: c.Response.StatusCode, Fault: awserr.FaultSender,
	}, awsjson.ErrorOptions{ContentType: "application/json", Members: toJSON(t, m, em, fields)})

	require.Equal(t, c.Response.StatusCode, rec.Code)

	want := map[string]any{"Error": map[string]any{codeKey: c.ErrorCode, messageKey: c.ErrorMessage}}
	maps.Copy(want, fields)

	got := parseJSONError(t, m, em, rec.Header(), rec.Body.Bytes())
	assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty))

	fromFixture := parseJSONError(t, m, em, wireHeader(c.Response.Headers), []byte(c.Response.Body))
	assert.Equal(t, canonical(want, keepEmpty), canonical(fromFixture, keepEmpty),
		"what a client parses from the fixture")
}

// parseJSONError finds the code as botocore's rest-json parser does: the
// X-Amzn-Errortype header first, then "code" or "__type" in the body, with any
// namespace and URI cut off.
func parseJSONError(t *testing.T, m model, em member, h http.Header, body []byte) map[string]any {
	t.Helper()

	var fields map[string]any
	if strings.TrimSpace(string(body)) != "" {
		require.NoError(t, json.Unmarshal(body, &fields))
	}

	code := ""
	if lines := headerLines(h, "X-Amzn-Errortype"); len(lines) > 0 {
		code = lines[0]
	}

	for _, key := range []string{"code", "__type"} {
		if s, ok := fields[key].(string); ok && code == "" {
			code = s
		}
	}

	code, _, _ = strings.Cut(code, ":")
	code = code[strings.LastIndexByte(code, '#')+1:]

	message := ""

	for _, key := range []string{"message", messageKey, "errorMessage"} {
		if s, ok := fields[key].(string); ok && message == "" {
			message = s
		}
	}

	got := parseHeaders(t, m, em, h, 0)
	maps.Copy(got, decodeJSONBody(t, m, em, body, inBody))

	got["Error"] = map[string]any{codeKey: code, messageKey: message}

	return got
}
