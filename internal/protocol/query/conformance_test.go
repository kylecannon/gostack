package query_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipped lists the fixture cases the query codec does not run, each with the
// reason. The key is the fixture file and the case id.
const compressed = "the client compresses the request body; the gateway decodes content encodings before a " +
	"protocol codec sees the body; botocore's own ignore list skips the suite"

var skipped = map[string]string{
	"input/query.json QueryProtocolIdempotencyTokenAutoFill": "the client adds the idempotency token, so the " +
		"params do not have it and the request does",
	"input/query.json SDKAppliedContentEncoding_awsQuery":                    compressed,
	"input/query.json SDKAppendsGzipAndIgnoresHttpProvidedEncoding_awsQuery": compressed,
	"output/query.json QueryXmlLists": "two flattened lists share the element name \"item\", so a client cannot " +
		"tell their items apart; botocore's own ignore list skips this case too",
	"input/query.json QueryFlattenedListArgWithXmlName": "LocalStack names a flattened list after the list " +
		"member's locationName, not the structure member's; botocore's own ignore list skips this case too",
	"output/query.json QueryComplexError": "a modeled error with members of its own; awserr.Error carries a " +
		"code and a message only; botocore's own ignore list skips this case too",
}

// skipReason returns why a case is skipped. GOSTACK_RUN_SKIPPED=1 runs the
// skipped cases anyway, to see which of them still fail.
func skipReason(file, id string) (string, bool) {
	if os.Getenv("GOSTACK_RUN_SKIPPED") != "" {
		return "", false
	}

	reason, ok := skipped[file+" "+id]

	return reason, ok
}

func Test_Conformance_Input(t *testing.T) {
	t.Parallel()

	const file = "input/query.json"

	for _, g := range loadFixtures(t, file) {
		for _, c := range g.Cases {
			t.Run(c.ID, func(t *testing.T) {
				t.Parallel()

				if reason, ok := skipReason(file, c.ID); ok {
					t.Skip(reason)
				}

				vals, err := url.ParseQuery(c.Serialized.Body)
				require.NoError(t, err)

				in := &interp{shapes: g.Shapes}
				d := query.NewDecoder(vals)

				var got map[string]any
				if c.Given.Input != nil {
					got = in.decodeStruct(d, in.resolve(c.Given.Input).shape, "")
				}

				require.Nil(t, d.Err())
				assert.Equal(t, canonical(decodeDynamic(t, c.Params), dropEmpty), canonical(got, dropEmpty))
			})
		}
	}
}

func Test_Conformance_Output(t *testing.T) {
	t.Parallel()

	const file = "output/query.json"

	for _, g := range loadFixtures(t, file) {
		for _, c := range g.Cases {
			t.Run(c.ID, func(t *testing.T) {
				t.Parallel()

				if reason, ok := skipReason(file, c.ID); ok {
					t.Skip(reason)
				}

				in := &interp{shapes: g.Shapes}

				if len(c.Given.Errors) > 0 {
					checkError(t, c)

					return
				}

				checkResponse(t, in, c)
			})
		}
	}
}

// checkResponse encodes the case's result with the Writer and compares what a
// client parses out of it with what it parses out of the fixture's response.
func checkResponse(t *testing.T, in *interp, c fixtureCase) {
	t.Helper()

	env := &query.Envelope{Action: c.Given.Name, RequestID: "foo-id"}

	var (
		out        *resolved // nil when the operation has no output
		encodeFunc func(*query.Writer)
		encodeErr  error
	)

	if c.Given.Output != nil {
		r := in.resolve(c.Given.Output)
		out = &r
		env.Result = c.Given.Name + "Result"
		params, _ := decodeDynamic(t, c.Result).(map[string]any)
		encodeFunc = func(w *query.Writer) { encodeErr = in.encodeStruct(w, r.shape, params) }
	}

	rec := httptest.NewRecorder()
	query.WriteResponse(rec, env, encodeFunc)
	require.NoError(t, encodeErr)
	require.Equal(t, http.StatusOK, rec.Code)

	got := parseResult(t, in, out, env.Result, rec.Body.Bytes())

	if c.Response.Body != "" {
		want := parseResult(t, in, out, env.Result, []byte(c.Response.Body))
		assert.Equal(t, canonical(want, keepEmpty), canonical(got, keepEmpty), "what a client parses from the fixture")
	}

	assert.Equal(t, canonical(decodeDynamic(t, c.Result), keepEmpty), canonical(got, keepEmpty))
}

func parseResult(t *testing.T, in *interp, out *resolved, wrapper string, body []byte) map[string]any {
	t.Helper()

	if out == nil {
		return map[string]any{}
	}

	root, err := parseXML(body)
	require.NoError(t, err)

	result := root.child(wrapper)
	require.NotNil(t, result, "no %s element in %s", wrapper, body)

	parsed, err := in.parseStruct(out.shape, result)
	require.NoError(t, err)

	return parsed
}

// checkError writes the case's error with WriteError and reads the code, the
// message and the status back.
func checkError(t *testing.T, c fixtureCase) {
	t.Helper()

	rec := httptest.NewRecorder()
	query.WriteError(rec, &query.Envelope{RequestID: "foo-id"}, &awserr.Error{
		Code: c.ErrorCode, Message: c.ErrorMessage, StatusCode: c.Response.StatusCode, Fault: awserr.FaultSender,
	})

	require.Equal(t, c.Response.StatusCode, rec.Code)

	root, err := parseXML(rec.Body.Bytes())
	require.NoError(t, err)

	got := root.child("Error")
	require.NotNil(t, got)
	assert.Equal(t, c.ErrorCode, got.child("Code").text)
	assert.Equal(t, "Sender", got.child("Type").text)
	assert.Equal(t, "foo-id", root.child("RequestId").text)

	if c.ErrorMessage != "" {
		assert.Equal(t, c.ErrorMessage, got.child("Message").text)
	}

	want, err := parseXML([]byte(c.Response.Body))
	require.NoError(t, err)
	assert.Equal(t, want.child("Error").child("Code").text, got.child("Code").text, "the fixture's own code")
}
