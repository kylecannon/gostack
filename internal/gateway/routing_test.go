package gateway_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
)

// echoService answers with the name of the service that got the call.
func echoService(w http.ResponseWriter, rc *request.Context) {
	_, _ = io.WriteString(w, rc.Service)
}

// Test_Gateway_unsignedQueryRequests is LocalStack's service_router.py step 5:
// a query request with no signature and no Version goes to the service that has
// the Action.
func Test_Gateway_unsignedQueryRequests(t *testing.T) {
	sqsReg := sqsRegistration(echoService)
	sqsReg.Metadata.Operations = []string{"ListQueues", "SendMessage"}

	stsReg := stsRegistration(echoService)
	stsReg.Metadata.Operations = []string{opGetCallerIdentity}

	g := newGateway(t, newSettings(), []service.Registration{sqsReg, stsReg})

	tests := []struct {
		name       string
		give       *http.Request
		wantStatus int
		wantBody   string
	}{
		{
			name: "ListQueues in the body goes to sqs",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "Action=ListQueues",
				map[string]string{headerType: formType}),
			wantStatus: http.StatusOK,
			wantBody:   nameSQS,
		},
		{
			name:       "ListQueues in the query string goes to sqs",
			give:       newRequest(t, http.MethodGet, "http://localhost:4566/?Action=ListQueues", "", nil),
			wantStatus: http.StatusOK,
			wantBody:   nameSQS,
		},
		{
			name: "GetCallerIdentity goes to sts",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "Action="+opGetCallerIdentity,
				map[string]string{headerType: formType}),
			wantStatus: http.StatusOK,
			wantBody:   nameSTS,
		},
		{
			name: "an Action nobody has is not found",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "Action=NoSuchOperation",
				map[string]string{headerType: formType}),
			wantStatus: http.StatusNotFound,
			wantBody:   `{"message": "Not Found"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := call(t, g.Handler(), tt.give)

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, tt.wantBody, body)
		})
	}
}

// Test_Gateway_serviceNotEnabled is the answer of LocalStack's ServiceLoader
// (aws/handlers/service_plugin.py) to a call for a service it has but does not
// run: 501 InternalFailure, in the protocol of that service.
func Test_Gateway_serviceNotEnabled(t *testing.T) {
	g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(okServe), stsRegistration(okServe)})

	notEnabled := func(name string) string {
		return "Service '" + name + "' is not enabled. Please check your 'SERVICES' configuration variable."
	}

	tests := []struct {
		name     string
		give     *http.Request
		wantType string
		wantBody string
	}{
		{
			name: "json service",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "{}", map[string]string{
				headerAuth:   signed("test", usEast1, "dynamodb"),
				headerTarget: "DynamoDB_20120810.ListTables",
				headerType:   jsonType,
			}),
			wantType: contentAmzJSON,
			wantBody: `{"__type":"InternalFailure","message":"` + notEnabled("dynamodb") + `"}`,
		},
		{
			name: "json service told apart by its target when the signing name is shared",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "{}", map[string]string{
				headerAuth:   signed("test", usEast1, "dynamodb"),
				headerTarget: "DynamoDBStreams_20120810.ListStreams",
				headerType:   jsonType,
			}),
			wantType: contentAmzJSON,
			wantBody: `{"__type":"InternalFailure","message":"` + notEnabled("dynamodbstreams") + `"}`,
		},
		{
			name: "rest-json service",
			give: newRequest(t, http.MethodGet, "http://localhost:4566/2015-03-31/functions", "", map[string]string{
				headerAuth: signed("test", usEast1, "lambda"),
			}),
			wantType: contentJSON,
			wantBody: `{"__type":"InternalFailure","message":"` + notEnabled("lambda") + `"}`,
		},
		{
			name: "query service",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "Action=ListUsers&Version=2010-05-08",
				map[string]string{headerAuth: signed("test", usEast1, "iam"), headerType: formType}),
			wantType: contentXML,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := call(t, g.Handler(), tt.give)

			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
			assert.Equal(t, tt.wantType, resp.Header.Get(headerType))
			assert.Equal(t, []string{valueTrue}, rawHeader(resp.Header, "x-localstack"))
			assert.NotEmpty(t, rawHeader(resp.Header, "x-amzn-RequestId"))

			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, body)
				assert.Equal(t, "InternalFailure", resp.Header.Get("X-Amzn-Errortype"))

				return
			}

			assert.Contains(t, body, "<Code>InternalFailure</Code><Message>"+notEnabled("iam")+"</Message>")
			assert.Contains(t, body, "<ErrorResponse>")
		})
	}
}
