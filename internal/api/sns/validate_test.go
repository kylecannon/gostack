package sns_test

import (
	"encoding/xml"
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
			key: "test_set_platform_application_attributes_non_existing_app non_existing-application-arn",
			op:  "SetPlatformApplicationAttributes",
			form: "Action=SetPlatformApplicationAttributes" +
				"&PlatformApplicationArn=arn:aws:sns:us-east-1:000000000000:app/GCM/none",
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

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(c.form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			rec := httptest.NewRecorder()
			sns.ServeValidated(&sns.Unimplemented{}, rec, &request.Context{
				Service: "sns", Protocol: protocol.KindQuery, Operation: c.op, RequestID: "req-1", Request: req,
			})

			var resp struct {
				Error struct {
					Type    string
					Code    string
					Message string
				}
			}

			require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "Sender", resp.Error.Type)
			assert.Equal(t, "ValidationError", resp.Error.Code)
			assert.Equal(t, c.want, resp.Error.Message)
		})
	}
}
