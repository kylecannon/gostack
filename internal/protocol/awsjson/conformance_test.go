package awsjson_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	clientErrorCode = "the case is about how a client finds the error code in a response; a server writes it one way"
	compressed      = "the client compresses the request body; the gateway decodes content encodings before a " +
		"protocol codec sees the body; botocore's own ignore list skips the suite"
	documents = "the json document type is not modeled by the generated types"
)

// skipped lists the fixture cases the json codec does not run, each with the
// reason. The key is the fixture file and the case id.
var skipped = map[string]string{
	"input/json.json AwsJson11StructuresDontSerializeNullValues": "the params hold nulls a client leaves out; " +
		"botocore's own ignore list skips this case too",
	"input/json.json SDKAppliedContentEncoding_awsJson1_1":                    compressed,
	"input/json.json SDKAppendsGzipAndIgnoresHttpProvidedEncoding_awsJson1_1": compressed,
	"input/json.json PutAndGetInlineDocumentsInput":                           documents,
	"output/json.json PutAndGetInlineDocumentsInput":                          documents,

	"output/json.json AwsJson11FooErrorUsingXAmznErrorType":                    clientErrorCode,
	"output/json.json AwsJson11FooErrorUsingXAmznErrorTypeWithUri":             clientErrorCode,
	"output/json.json AwsJson11FooErrorUsingXAmznErrorTypeWithUriAndNamespace": clientErrorCode,
	"output/json.json AwsJson11FooErrorUsingCode":                              clientErrorCode,
	"output/json.json AwsJson11FooErrorUsingCodeAndNamespace":                  clientErrorCode,
	"output/json.json AwsJson11FooErrorUsingCodeUriAndNamespace":               clientErrorCode,
	"output/json.json AwsJson11FooErrorWithDunderTypeAndNamespace":             clientErrorCode,
	"output/json.json AwsJson11FooErrorWithDunderTypeUriAndNamespace":          clientErrorCode,
	"output/json.json AwsJson11ComplexError": "a modeled error with members of its own; awserr.Error carries a " +
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

	const file = "input/json.json"

	for _, g := range loadFixtures(t, file) {
		for _, c := range g.Cases {
			t.Run(c.ID, func(t *testing.T) {
				t.Parallel()

				if reason, ok := skipReason(file, c.ID); ok {
					t.Skip(reason)
				}

				in := &interp{shapes: g.Shapes}
				got := map[string]any{}

				if c.Given.Input != nil {
					got = decodeBody(t, in, in.resolve(c.Given.Input), []byte(c.Serialized.Body))
				}

				want := decodeDynamic(t, c.Params)
				if want == nil {
					want = map[string]any{}
				}

				assert.Equal(t, canonical(want), canonical(got))
			})
		}
	}
}

func Test_Conformance_Output(t *testing.T) {
	t.Parallel()

	const file = "output/json.json"

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

// decodeBody reads a json body with the awsjson codec and turns it into plain values
// through the shape. An empty body is an empty structure.
func decodeBody(t *testing.T, in *interp, r resolved, body []byte) map[string]any {
	t.Helper()

	if strings.TrimSpace(string(body)) == "" {
		return map[string]any{}
	}

	var wire any

	require.NoError(t, awsjson.Unmarshal(body, &wire))

	got, err := in.fromWire(r, wire)
	require.NoError(t, err)

	out, ok := got.(map[string]any)
	require.True(t, ok)

	return out
}

// checkResponse encodes the case's result and compares what a client parses
// out of it with what it parses out of the fixture's response.
func checkResponse(t *testing.T, in *interp, c fixtureCase) {
	t.Helper()

	got := map[string]any{}

	if c.Given.Output != nil {
		r := in.resolve(c.Given.Output)

		wire, err := in.toWire(r, decodeDynamic(t, c.Result))
		require.NoError(t, err)

		body, err := awsjson.Marshal(wire)
		require.NoError(t, err)

		got = decodeBody(t, in, r, body)

		if c.Response.Body != "" {
			want := decodeBody(t, in, r, []byte(c.Response.Body))
			assert.Equal(t, canonical(want), canonical(got), "what a client parses from the fixture")
		}
	}

	want := decodeDynamic(t, c.Result)
	if want == nil {
		want = map[string]any{}
	}

	assert.Equal(t, canonical(want), canonical(got))
}

// checkError writes the case's error with WriteError and reads the code, the
// message and the status back.
func checkError(t *testing.T, c fixtureCase) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	awsjson.WriteError(rec, req, "foo-id", &awserr.Error{
		Code: c.ErrorCode, Message: c.ErrorMessage, StatusCode: c.Response.StatusCode, Fault: awserr.FaultSender,
	}, awsjson.ErrorOptions{})

	require.Equal(t, c.Response.StatusCode, rec.Code)

	var body map[string]any

	require.NoError(t, awsjson.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, c.ErrorCode, body["__type"])
	assert.Equal(t, []string{c.ErrorCode}, map[string][]string(rec.Header())["X-Amzn-Errortype"])

	if c.ErrorMessage != "" {
		assert.Equal(t, c.ErrorMessage, body["message"])
	}

	if c.Response.Body != "" {
		var want map[string]any

		require.NoError(t, awsjson.Unmarshal([]byte(c.Response.Body), &want))
		assert.Equal(t, want["__type"], body["__type"], "the fixture's own code")
	}
}
