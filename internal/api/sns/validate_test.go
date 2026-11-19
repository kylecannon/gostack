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

// validating stands in for a handler that runs the generated checks first, as a
// real one does where LocalStack makes them. Nothing else answers: a request
// that passes reaches Unimplemented.
type validating struct{ sns.Unimplemented }

func (v validating) CreatePlatformApplication(
	ctx context.Context, in *sns.CreatePlatformApplicationInput,
) (*sns.CreatePlatformApplicationOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.CreatePlatformApplication(ctx, in)
}

func (v validating) Publish(ctx context.Context, in *sns.PublishInput) (*sns.PublishOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.Publish(ctx, in)
}

func (v validating) PublishBatch(
	ctx context.Context, in *sns.PublishBatchInput,
) (*sns.PublishBatchOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.PublishBatch(ctx, in)
}

func (v validating) SetEndpointAttributes(
	ctx context.Context, in *sns.SetEndpointAttributesInput,
) (*sns.SetEndpointAttributesOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	return v.Unimplemented.SetEndpointAttributes(ctx, in)
}

// emptyMessageFirst is a Publish handler that answers an empty message before
// it runs the generated checks, as LocalStack does.
type emptyMessageFirst struct{ validating }

func (e emptyMessageFirst) Publish(ctx context.Context, in *sns.PublishInput) (*sns.PublishOutput, error) {
	if in.Message == nil || *in.Message == "" {
		return nil, sns.NewInvalidParameterException("Invalid parameter: Empty message")
	}

	return e.validating.Publish(ctx, in)
}

// Test_Validate_SnapshotMessages sends the input of each test_sns.py test
// whose snapshot pins a constraint message through a handler that calls
// Validate, and checks the generated checks answer that message. The key names
// the snapshot entry. An empty Attributes map sends no parameter at all, so it
// reads as absent.
func Test_Validate_SnapshotMessages(t *testing.T) {
	t.Parallel()

	const nullAttributes = "1 validation error detected: Value null at 'attributes' failed to satisfy constraint: " +
		"Member must not be null"

	tests := []struct {
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

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()

			code, got := serveQuery(t, validating{}, tt.op, tt.form)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "Sender", got.Type)
			assert.Equal(t, "ValidationError", got.Code)
			assert.Equal(t, tt.want, got.Message)
		})
	}
}

// Test_ServeValidated_NeverChecks sends the CreatePlatformApplication
// snapshot, with no Attributes, to a handler that does not call Validate:
// ServeValidated runs no check of its own, so it reaches the handler.
func Test_ServeValidated_NeverChecks(t *testing.T) {
	t.Parallel()

	code, got := serveQuery(t, sns.Unimplemented{}, "CreatePlatformApplication",
		"Action=CreatePlatformApplication&Name=app&Platform=GCM")
	assert.Equal(t, http.StatusNotImplemented, code)
	assert.Equal(t, "InternalFailure", got.Code, got.Message)
	assert.Contains(t, got.Message, "not yet implemented")
}

// Test_Validate_EmptyMessageFirst sends a Publish with an empty message and a
// message attribute that has no data type. The handler answers the empty
// message first, as LocalStack does; one that only calls Validate answers the
// data type.
func Test_Validate_EmptyMessageFirst(t *testing.T) {
	t.Parallel()

	const form = "Action=Publish&TopicArn=arn&Message=" +
		"&MessageAttributes.entry.1.Name=attr1&MessageAttributes.entry.1.Value.StringValue=value"

	code, got := serveQuery(t, emptyMessageFirst{}, "Publish", form)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "InvalidParameter", got.Code)
	assert.Equal(t, "Invalid parameter: Empty message", got.Message)

	_, got = serveQuery(t, validating{}, "Publish", form)
	assert.Equal(t, "ValidationError", got.Code)
	assert.Contains(t, got.Message, "messageAttributes.attr1.member.dataType")
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

// Test_Validate_HandlerRunsChecks sends the two
// SetPlatformApplicationAttributes snapshots with no Attributes to a handler
// that checks the ARN's form first and calls Validate after it. botocore sends
// nothing for Attributes={}.
func Test_Validate_HandlerRunsChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
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

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()

			code, got := serveQuery(t, arnFirst{}, "SetPlatformApplicationAttributes",
				"Action=SetPlatformApplicationAttributes&PlatformApplicationArn="+tt.arn)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, tt.code, got.Code)
			assert.Equal(t, tt.want, got.Message)
		})
	}

	// With Attributes set, the checks pass and the handler answers.
	_, got := serveQuery(t, arnFirst{}, "SetPlatformApplicationAttributes",
		"Action=SetPlatformApplicationAttributes&PlatformApplicationArn="+tests[1].arn+
			"&Attributes.entry.1.key=PlatformPrincipal&Attributes.entry.1.value=dummy")
	assert.Equal(t, "NotFound", got.Code)
}

// Test_Validate_UnlistedConstraintReachesHandler sends a TagResource
// with an empty tag key, which breaks the model's min of 1. No snapshot pins
// that, and LocalStack only checks for duplicate keys, so the handler answers.
func Test_Validate_UnlistedConstraintReachesHandler(t *testing.T) {
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
