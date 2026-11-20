package sns_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/api/openapi"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const (
	localstackSpecPath  = "../../../specs/localstack/openapi.yaml"
	specResource        = "openapi.json"
	endpointMessagesURL = "/sns/platform-endpoint-messages"
	smsMessagesURL      = "/sns/sms-messages"
	optOutsURL          = "/sns/phone-opt-outs"
	tokensURL           = "/sns/subscription-tokens/"
	tokenSpecPath       = tokensURL + "{subscription_arn}"
)

// openAPI is an OpenAPI 3.1 document whose JSON response schemas can validate a
// body: LocalStack's, for the /_aws names, and gostack's own, for the canonical
// ones.
type openAPI struct {
	compiler *jsonschema.Compiler
}

func loadOpenAPI(t *testing.T, data []byte) *openAPI {
	t.Helper()

	var document any
	require.NoError(t, yaml.Unmarshal(data, &document))

	asJSON, err := json.Marshal(document)
	require.NoError(t, err)

	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	require.NoError(t, err)

	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource(specResource, parsed))

	return &openAPI{compiler: compiler}
}

func pointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")

	return url.PathEscape(token)
}

// schema compiles the JSON schema of the operation's response with status, and
// fails the test when the document has none.
func (o *openAPI) schema(t *testing.T, path, method, status string) *jsonschema.Schema {
	t.Helper()

	pointer := specResource + "#/paths/" + pointerToken(path) + "/" + method + "/responses/" + status +
		"/content/application~1json/schema"

	schema, err := o.compiler.Compile(pointer)
	require.NoError(t, err, "no schema at %s", pointer)

	return schema
}

// valid checks body against the operation's response schema.
func (o *openAPI) valid(t *testing.T, path, method, status, body string) {
	t.Helper()

	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	require.NoError(t, err, body)
	require.NoError(t, o.schema(t, path, method, status).Validate(instance), body)
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()

	values, err := url.ParseQuery(raw)
	require.NoError(t, err)

	return values
}

// Test_DeveloperAPI_matchesSpecs: each developer route answers in the shape of
// LocalStack's openapi.yaml under /_aws, and in the shape api/openapi/gostack.yaml
// documents under /_gostack/services.
func Test_DeveloperAPI_matchesSpecs(t *testing.T) {
	data, err := os.ReadFile(localstackSpecPath)
	require.NoError(t, err)

	localstack := loadOpenAPI(t, data)
	gostack := loadOpenAPI(t, openapi.Spec)

	s := newStack(t, nil)
	app := s.createApp("spec")
	direct := s.createEndpoint(app, "direct")
	topic := s.createTopic(paramName, "spec")
	viaTopic := s.createEndpoint(app, "topic")
	s.subscribe(paramTopicArn, topic, paramProtocol, protocolApplication, paramEndpoint, viaTopic)
	s.subscribe(paramTopicArn, topic, paramProtocol, "sms", paramEndpoint, number)
	// Every subscription has a token, whether or not it is pending; the SQS one
	// answers with its real ARN where a pending subscription answers with a
	// placeholder.
	subARN := s.subscribeSQS(topic)

	// LocalStack's schema has no null for the attributes and the structure,
	// though its code writes null when a message has none, so the listing it is
	// checked against holds a message that has both. The unfiltered listing,
	// with the message that came through the topic, is checked against gostack's.
	s.publish(paramTargetARN, direct, paramMessage, `{"default":"d"}`, paramStructure, valueStructure,
		"MessageAttributes.entry.1.Name", "a", "MessageAttributes.entry.1.Value.DataType", typeString,
		"MessageAttributes.entry.1.Value.StringValue", "b")
	s.publish(paramTopicArn, topic, paramMessage, "via topic", paramSubject, "subject",
		"MessageAttributes.entry.1.Name", "a", "MessageAttributes.entry.1.Value.DataType", typeString,
		"MessageAttributes.entry.1.Value.StringValue", "b")
	s.publish(paramPhone, number, paramMessage, "direct sms", "MessageAttributes.entry.1.Name", "a",
		"MessageAttributes.entry.1.Value.DataType", typeString, "MessageAttributes.entry.1.Value.StringValue", "b")

	// Both documents describe the same listings, so one body must be valid for
	// each.
	for _, tt := range []struct {
		spec   *openAPI
		prefix string
	}{{localstack, pathAWS}, {gostack, pathGostack}} {
		for _, route := range []struct{ path, query string }{
			{endpointMessagesURL, queryEndpointARN + "=" + url.QueryEscape(direct)},
			{smsMessagesURL, ""},
			{smsMessagesURL, queryPhone + "=" + url.QueryEscape(number)},
			{smsMessagesURL, queryPhone + "=%2B1999"},
		} {
			status, body := s.devCall(http.MethodGet, tt.prefix+route.path, mustQuery(t, route.query), "")
			require.Equal(t, http.StatusOK, status, body)
			tt.spec.valid(t, tt.prefix+route.path, "get", "200", body)
		}

		status, body := s.devCall(http.MethodGet, tt.prefix+tokensURL+subARN, nil, "")
		require.Equal(t, http.StatusOK, status, body)
		tt.spec.valid(t, tt.prefix+tokenSpecPath, "get", "200", body)

		status, body = s.devCall(http.MethodGet, tt.prefix+tokensURL+"randomarnhere", nil, "")
		require.Equal(t, http.StatusBadRequest, status, body)
		tt.spec.valid(t, tt.prefix+tokenSpecPath, "get", "400", body)

		missing := strings.Replace(subARN, "us-east-1", "il-central-1", 1)
		status, body = s.devCall(http.MethodGet, tt.prefix+tokensURL+missing, nil, "")
		require.Equal(t, http.StatusNotFound, status, body)
		tt.spec.valid(t, tt.prefix+tokenSpecPath, "get", "404", body)
	}

	status, body := s.devCall(http.MethodGet, pathGostack+endpointMessagesURL, nil, "")
	require.Equal(t, http.StatusOK, status, body)
	gostack.valid(t, pathGostack+endpointMessagesURL, "get", "200", body)

	// The 400 of an endpointArn that is not an ARN is documented for the
	// canonical path only; LocalStack's document has none.
	status, body = s.devCall(http.MethodGet, pathGostack+endpointMessagesURL, url.Values{queryEndpointARN: {"bad"}}, "")
	require.Equal(t, http.StatusBadRequest, status, body)
	gostack.valid(t, pathGostack+endpointMessagesURL, "get", "400", body)

	t.Run("the schemas can fail", func(t *testing.T) {
		schema := gostack.schema(t, pathGostack+smsMessagesURL, "get", "200")

		for _, bad := range []string{
			`{"region":"us-east-1"}`,
			`{"region":"us-east-1","sms_messages":{"+1":[{"PhoneNumber":"+1"}]}}`,
			`{"region":"us-east-1","sms_messages":{"+1":"x"}}`,
			`{"region":"us-east-1","sms_messages":{},"extra":1}`,
		} {
			instance, err := jsonschema.UnmarshalJSON(strings.NewReader(bad))
			require.NoError(t, err)
			assert.Error(t, schema.Validate(instance), bad)
		}
	})
}

// Test_DeveloperAPI_optOutDocumented: LocalStack's document has no schema for
// the opt-out route, so the canonical document is what describes its answers.
func Test_DeveloperAPI_optOutDocumented(t *testing.T) {
	s := newStack(t, nil)
	gostack := loadOpenAPI(t, openapi.Spec)

	status, body := s.devCall(http.MethodPost, pathGostack+optOutsURL, nil, `{"phoneNumber":"`+number+`"}`)
	require.Equal(t, http.StatusOK, status, body)
	assert.Empty(t, body)

	status, body = s.devCall(http.MethodPost, pathGostack+optOutsURL, nil, `[]`)
	require.Equal(t, http.StatusBadRequest, status, body)
	gostack.valid(t, pathGostack+optOutsURL, "post", "400", body)
}
