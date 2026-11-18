package rest_test

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/require"
)

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

func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := awsjson.Marshal(v)
	require.NoError(t, err)

	return b
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
