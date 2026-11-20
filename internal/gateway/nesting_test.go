package gateway_test

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	ddbapi "github.com/kylecannon/gostack/internal/api/dynamodb"
	kmsapi "github.com/kylecannon/gostack/internal/api/kms"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	stsapi "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// nestingAlloc is the most a deeply nested body at the cap may allocate
	// while the gateway answers it. The worst measured is about 300 MiB, for a
	// query body of two million keys.
	nestingAlloc = 1 << 30

	notImplemented = "not yet implemented"
	localhost      = "http://localhost:4566/"
)

// nestTo is open, mid and close nested as many times as fits in size bytes,
// less the bytes the caller wraps around it.
func nestTo(size int, open, mid, closing string) string {
	n := (size - len(mid)) / (len(open) + len(closing))

	return strings.Repeat(open, n) + mid + strings.Repeat(closing, n)
}

const (
	// jsonNesting is how deeply a json body may nest, counting its own object:
	// encoding/json/v2's limit, which holds inside a member the type does not
	// have as well.
	jsonNesting = 10000

	sqsHead = `{"QueueUrl":"q","MessageBody":"b",`
	kmsHead = `{"KeyId":"k","Plaintext":"cA==",`
)

// inUnknown is head, then a member the input does not have whose arrays nest
// until the body is depth levels deep, counting the body's own object.
func inUnknown(head string, depth int) string {
	return head + `"Unknown":` + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + "}"
}

// generatedRegistration serves name's generated API with its Unimplemented
// handler: a body that decodes reaches the handler and is answered 501, and one
// the decoder refuses is answered with the protocol's malformed-body error.
func generatedRegistration(
	meta service.Metadata, serve func(http.ResponseWriter, *request.Context),
) service.Registration {
	return service.Registration{Metadata: meta, Serve: serve, SigningDefault: meta.Name == nameS3}
}

func jsonCall(svc, target, version string) map[string]string {
	return map[string]string{
		headerAuth:   signed("test", usEast1, svc),
		headerType:   "application/x-amz-json-" + version,
		headerTarget: target,
	}
}

// nestingHandler is a gateway whose generated services answer a body that
// decodes with their Unimplemented handler.
func nestingHandler(t *testing.T) http.Handler {
	t.Helper()

	regs := []service.Registration{
		generatedRegistration(sqsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			sqsapi.Serve(sqsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(stsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			stsapi.Serve(stsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(ddbapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			ddbapi.Serve(ddbapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(kmsapi.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			kmsapi.Serve(kmsapi.Unimplemented{}, w, rc)
		}),
		generatedRegistration(s3api.Metadata, func(w http.ResponseWriter, rc *request.Context) {
			s3api.Serve(s3api.Unimplemented{}, w, rc)
		}),
	}
	return newGateway(t, newSettings(), regs).Handler()
}

// Test_Gateway_DeeplyNestedBodies: a body built to nest as deeply as its syntax
// allows, at the 16 MiB body cap, is refused with the protocol's malformed-body
// error where the decoder bounds depth (json at 10,000 levels, rest-xml at
// restxml.MaxDepth), or decodes where the shape bounds it (query). Either way
// it answers within nestingAlloc, waits for nothing, and the gateway serves the
// next request.
//
// Each case runs in a synctest bubble, with no socket and no sleep of its own,
// so a call that waited, on a timer or a sleep, would move the fake clock, and
// each case asserts it did not. What a call costs in CPU is not seen there: the clock
// stands still while it runs, and Test_Gateway_NestedBodiesScaleWithDepth
// measures that.
func Test_Gateway_DeeplyNestedBodies(t *testing.T) {
	const (
		bodyCap   = int(protocol.DefaultMaxBody)
		wrap      = 200 // room for the members around a nested value
		malformed = "SerializationException"
		badXML    = "<Code>MalformedXML</Code>"
		ddbPut    = "DynamoDB_20120810.PutItem"
	)

	sqsJSON := jsonCall(nameSQS, targetSendMessage, "1.0")
	kmsJSON := jsonCall("kms", "TrentService.Encrypt", "1.1")
	s3Call := map[string]string{headerAuth: signed("test", usEast1, nameS3)}
	form := func(svc string) map[string]string {
		return map[string]string{headerAuth: signed("test", usEast1, svc), headerType: formType}
	}
	item := func(levels int, open, closing string) string {
		return `{"TableName":"t","Item":{"k":` + strings.Repeat(open, levels) + `{"S":"x"}` +
			strings.Repeat(closing, levels) + `}}`
	}

	tests := []struct {
		name, method, target, body string
		headers                    map[string]string
		wantStatus                 int
		wantBody                   string
	}{
		{
			name: "json 1.0 arrays in an unknown member to the bound decode", method: http.MethodPost,
			target: localhost, body: inUnknown(sqsHead, jsonNesting), headers: sqsJSON,
			wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "json 1.0 arrays in an unknown member one level past the bound", method: http.MethodPost,
			target: localhost, body: inUnknown(sqsHead, jsonNesting+1), headers: sqsJSON,
			wantStatus: http.StatusBadRequest, wantBody: malformed,
		},
		{
			name: "json 1.0 objects", method: http.MethodPost, target: localhost,
			body: nestTo(bodyCap, `{"a":`, "1", "}"), headers: sqsJSON,
			wantStatus: http.StatusBadRequest, wantBody: malformed,
		},
		{
			name: "json 1.0 arrays in an unknown member to the cap", method: http.MethodPost, target: localhost,
			body:    inUnknown(sqsHead, (bodyCap-wrap)/2),
			headers: sqsJSON, wantStatus: http.StatusBadRequest, wantBody: malformed,
		},
		{
			name: "json 1.1 arrays in an unknown member to the bound decode", method: http.MethodPost,
			target: localhost, body: inUnknown(kmsHead, jsonNesting), headers: kmsJSON,
			wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "json 1.1 arrays in an unknown member one level past the bound", method: http.MethodPost,
			target: localhost, body: inUnknown(kmsHead, jsonNesting+1), headers: kmsJSON,
			wantStatus: http.StatusBadRequest, wantBody: malformed,
		},
		{
			name: "a recursive member: DynamoDB M to the cap", method: http.MethodPost, target: localhost,
			body:    item((bodyCap-wrap)/len(`{"M":{"a":}}`), `{"M":{"a":`, `}}`),
			headers: jsonCall("dynamodb", ddbPut, "1.0"), wantStatus: http.StatusBadRequest, wantBody: malformed,
		},
		{
			name: "a recursive member: DynamoDB L to the cap", method: http.MethodPost, target: localhost,
			body:    item((bodyCap-wrap)/len(`{"L":[]}`), `{"L":[`, `]}`),
			headers: jsonCall("dynamodb", ddbPut, "1.0"), wantStatus: http.StatusBadRequest, wantBody: malformed,
		},
		{
			name: "a recursive member: DynamoDB's own limit of 32 levels decodes", method: http.MethodPost,
			target: localhost, body: item(32, `{"M":{"a":`, `}}`),
			headers:    jsonCall("dynamodb", ddbPut, "1.0"),
			wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "rest-xml PutBucketLifecycleConfiguration", method: http.MethodPut,
			target: localhost + "bucket-a?lifecycle",
			body: "<LifecycleConfiguration><Rule>" + nestTo(bodyCap-wrap, "<a>", "", "</a>") +
				"</Rule></LifecycleConfiguration>",
			headers: s3Call, wantStatus: http.StatusBadRequest, wantBody: badXML,
		},
		{
			name: "rest-xml unknown elements under the bound are skipped", method: http.MethodPut,
			target: localhost + "bucket-a?lifecycle",
			body: "<LifecycleConfiguration><Rule>" + strings.Repeat("<a>", 32) + strings.Repeat("</a>", 32) +
				"</Rule></LifecycleConfiguration>",
			headers: s3Call, wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "rest-xml DeleteObjects", method: http.MethodPost, target: localhost + "bucket-a?delete",
			body:    "<Delete><Object>" + nestTo(bodyCap-wrap, "<Key>", "", "</Key>") + "</Object></Delete>",
			headers: s3Call, wantStatus: http.StatusBadRequest, wantBody: badXML,
		},
		{
			name: "rest-xml CompleteMultipartUpload", method: http.MethodPost,
			target: localhost + "bucket-a/key?uploadId=u",
			body: "<CompleteMultipartUpload>" + nestTo(bodyCap-wrap, "<Part>", "", "</Part>") +
				"</CompleteMultipartUpload>",
			headers: s3Call, wantStatus: http.StatusBadRequest, wantBody: badXML,
		},
		{
			name: "query SQS: a key nested past the shape is ignored", method: http.MethodPost, target: localhost,
			body: "Action=SendMessage&QueueUrl=q&MessageBody=b&MessageAttribute.1.Value" +
				strings.Repeat(".Value", (bodyCap-wrap)/len(".Value")) + "=1",
			headers: form(nameSQS), wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
		{
			name: "query STS: a list member nested by dotted keys", method: http.MethodPost, target: localhost,
			body: "Action=AssumeRole&Version=2011-06-15&RoleArn=r&RoleSessionName=s&" +
				strings.Repeat("Tags.member.1.", (bodyCap-wrap)/len("Tags.member.1.")) + "Key=1",
			headers: form(nameSTS), wantStatus: http.StatusNotImplemented, wantBody: notImplemented,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				handler := nestingHandler(t)

				require.LessOrEqual(t, len(tt.body), bodyCap, "the body must be under the cap, or the cap answers")

				var before, after runtime.MemStats

				runtime.ReadMemStats(&before)

				begin := time.Now()

				resp, body := call(t, handler, newRequest(t, tt.method, tt.target, tt.body, tt.headers))

				waited := time.Since(begin)

				runtime.ReadMemStats(&after)

				// Work is bounded by the allocation below, which a load cannot move.
				assert.Zero(t, waited, "the call waited for something: the fake clock moved")
				assert.Equal(t, tt.wantStatus, resp.StatusCode, body)
				assert.Contains(t, body, tt.wantBody)
				assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(nestingAlloc))

				// The gateway still serves: a plain call decodes and reaches the handler.
				resp, body = call(t, handler, sendMessage(t))
				assert.Equal(t, http.StatusNotImplemented, resp.StatusCode, body)
			})
		})
	}
}
