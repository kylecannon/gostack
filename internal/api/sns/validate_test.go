package sns_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_ServeValidated_SnapshotMessages sends the input of each test_sns.py
// test whose snapshot pins a constraint message, and checks the generated
// checks answer that message. The key names the snapshot entry. An empty
// Attributes map sends no parameter at all, so it reads as absent.
func Test_ServeValidated_SnapshotMessages(t *testing.T) {
	t.Parallel()

	const nullAttributes = "1 validation error detected: Value null at 'attributes' failed to satisfy constraint: " +
		"Member must not be null"

	cases := []struct {
		key  string
		op   string
		form string
		want string
	}{
		{
			key:  "test_create_platform_application_invalid_attributes[no-args] platform-application-no-attributes",
			op:   "CreatePlatformApplication",
			form: "Action=CreatePlatformApplication&Name=app&Platform=GCM",
			want: nullAttributes,
		},
		{
			key:  "test_set_platform_endpoint_attributes_invalid_attributes[Empty] set-platform-endpoint-invalid-attributes",
			op:   "SetEndpointAttributes",
			form: "Action=SetEndpointAttributes&EndpointArn=arn:aws:sns:us-east-1:000000000000:endpoint/GCM/app/e",
			want: nullAttributes,
		},
		{
			key: "test_empty_or_wrong_message_attributes fully_missing_data_type",
			op:  "Publish",
			form: "Action=Publish&TopicArn=arn&Message=m" +
				"&MessageAttributes.entry.1.Name=attr1&MessageAttributes.entry.1.Value.StringValue=value",
			want: "1 validation error detected: Value null at 'messageAttributes.attr1.member.dataType' " +
				"failed to satisfy constraint: Member must not be null",
		},
		{
			key: "test_empty_or_wrong_message_attributes batch-fully_missing_data_type",
			op:  "PublishBatch",
			form: "Action=PublishBatch&TopicArn=arn" +
				"&PublishBatchRequestEntries.member.1.Id=1&PublishBatchRequestEntries.member.1.Message=m" +
				"&PublishBatchRequestEntries.member.1.MessageAttributes.entry.1.Name=attr1" +
				"&PublishBatchRequestEntries.member.1.MessageAttributes.entry.1.Value.StringValue=value",
			want: "1 validation error detected: Value null at " +
				"'publishBatchRequestEntries.1.member.messageAttributes.attr1.member.dataType' " +
				"failed to satisfy constraint: Member must not be null",
		},
	}

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()

			code, got := serveQuery(t, &sns.Unimplemented{}, c.op, c.form)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "Sender", got.Type)
			assert.Equal(t, "ValidationError", got.Code)
			assert.Equal(t, c.want, got.Message)
		})
	}
}

// arnFirst is a SetPlatformApplicationAttributes handler that checks the
// ARN's form, then runs the generated checks, as AWS orders them.
type arnFirst struct{ sns.Unimplemented }

func (arnFirst) SetPlatformApplicationAttributes(
	_ context.Context, in *sns.SetPlatformApplicationAttributesInput,
) (*sns.SetPlatformApplicationAttributesOutput, error) {
	arn := ""
	if in.PlatformApplicationArn != nil {
		arn = *in.PlatformApplicationArn
	}

	if n := len(strings.Split(arn, ":")); n < 6 {
		return nil, sns.NewInvalidParameterException(fmt.Sprintf(
			"Invalid parameter: PlatformApplicationArn Reason: An ARN must have at least 6 elements, not %d", n))
	}

	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return nil, sns.NewNotFoundException("PlatformApplication does not exist")
}

// Test_ServeValidated_HandlerRunsChecks sends the two
// SetPlatformApplicationAttributes snapshots with no Attributes, which
// ServeValidated leaves to the handler: the ARN's form answers first, then the
// generated check. botocore sends nothing for Attributes={}.
func Test_ServeValidated_HandlerRunsChecks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key, arn, code, want string
	}{
		{
			key:  "test_set_platform_application_attributes_invalid_arn invalid-application-arn",
			arn:  "invalid-arn",
			code: "InvalidParameter",
			want: "Invalid parameter: PlatformApplicationArn Reason: An ARN must have at least 6 elements, not 1",
		},
		{
			key:  "test_set_platform_application_attributes_non_existing_app[no-args] non_existing-application-arn",
			arn:  "arn:aws:sns:us-east-1:000000000000:app/ADM/NonExistingApp",
			code: "ValidationError",
			want: "1 validation error detected: Value null at 'attributes' failed to satisfy constraint: " +
				"Member must not be null",
		},
	}

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()

			code, got := serveQuery(t, arnFirst{}, "SetPlatformApplicationAttributes",
				"Action=SetPlatformApplicationAttributes&PlatformApplicationArn="+c.arn)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, c.code, got.Code)
			assert.Equal(t, c.want, got.Message)
		})
	}

	// With Attributes set, the checks pass and the handler answers.
	_, got := serveQuery(t, arnFirst{}, "SetPlatformApplicationAttributes",
		"Action=SetPlatformApplicationAttributes&PlatformApplicationArn="+cases[1].arn+
			"&Attributes.entry.1.key=PlatformPrincipal&Attributes.entry.1.value=dummy")
	assert.Equal(t, "NotFound", got.Code)
}

// Test_ServeValidated_UnlistedConstraintReachesHandler sends a TagResource
// with an empty tag key, which breaks the model's min of 1. No snapshot pins
// that, and LocalStack only checks for duplicate keys, so the handler answers.
func Test_ServeValidated_UnlistedConstraintReachesHandler(t *testing.T) {
	t.Parallel()

	_, got := serveQuery(t, &sns.Unimplemented{}, "TagResource",
		"Action=TagResource&ResourceArn=arn:aws:sns:us-east-1:000000000000:t"+
			"&Tags.member.1.Key=&Tags.member.1.Value=v")
	assert.Equal(t, "InternalFailure", got.Code, got.Message)
	assert.Contains(t, got.Message, "not yet implemented")
}

type queryError struct {
	Type    string
	Code    string
	Message string
}

func serveQuery(t *testing.T, h sns.Handler, op, form string) (int, queryError) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	sns.ServeValidated(h, rec, &request.Context{
		Service: "sns", Protocol: protocol.KindQuery, Operation: op, RequestID: "req-1", Request: req,
	})

	var resp struct {
		Error queryError
	}

	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())

	return rec.Code, resp.Error
}
