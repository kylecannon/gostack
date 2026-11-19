package sns_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publisher keeps the input of the Publish and PublishBatch calls it gets.
type publisher struct {
	sns.Unimplemented

	publish *sns.PublishInput
	batch   *sns.PublishBatchInput
}

func (p *publisher) Publish(_ context.Context, in *sns.PublishInput) (*sns.PublishOutput, error) {
	p.publish = in

	return &sns.PublishOutput{}, nil
}

func (p *publisher) PublishBatch(_ context.Context, in *sns.PublishBatchInput) (*sns.PublishBatchOutput, error) {
	p.batch = in

	return &sns.PublishBatchOutput{}, nil
}

func serveForm(t *testing.T, h sns.Handler, op, form string) int {
	t.Helper()

	req := httptest.NewRequest("POST", "/", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	sns.Serve(h, rec, &request.Context{Service: "sns", Protocol: protocol.KindQuery, Operation: op,
		RequestID: "req-1", Request: req})

	return rec.Code
}

// Test_Serve_MessageAttributeMap covers the generated decoding of sns's
// MessageAttributeMap, a map that is not flattened and whose value is a
// structure with a blob: MessageAttributes.entry.N.Name and .Value.<member>,
// on Publish and inside each PublishBatch entry.
func Test_Serve_MessageAttributeMap(t *testing.T) {
	t.Parallel()

	h := &publisher{}

	code := serveForm(t, h, "Publish", "Action=Publish&TopicArn=arn&Message=hi"+
		"&MessageAttributes.entry.1.Name=bin&MessageAttributes.entry.1.Value.DataType=Binary"+
		"&MessageAttributes.entry.1.Value.BinaryValue=AQID"+
		"&MessageAttributes.entry.2.Name=str&MessageAttributes.entry.2.Value.DataType=String"+
		"&MessageAttributes.entry.2.Value.StringValue=v")
	require.Equal(t, 200, code)
	require.NotNil(t, h.publish)

	attrs := h.publish.MessageAttributes
	require.Len(t, attrs, 2)
	assert.Equal(t, "Binary", *attrs["bin"].DataType)
	assert.Equal(t, []byte{1, 2, 3}, attrs["bin"].BinaryValue)
	assert.Nil(t, attrs["bin"].StringValue)
	assert.Equal(t, "String", *attrs["str"].DataType)
	assert.Equal(t, "v", *attrs["str"].StringValue)
	assert.Nil(t, attrs["str"].BinaryValue)

	code = serveForm(t, h, "PublishBatch", "Action=PublishBatch&TopicArn=arn"+
		"&PublishBatchRequestEntries.member.1.Id=one&PublishBatchRequestEntries.member.1.Message=m"+
		"&PublishBatchRequestEntries.member.1.MessageAttributes.entry.1.Name=bin"+
		"&PublishBatchRequestEntries.member.1.MessageAttributes.entry.1.Value.DataType=Binary"+
		"&PublishBatchRequestEntries.member.1.MessageAttributes.entry.1.Value.BinaryValue=BAU%3D")
	require.Equal(t, 200, code)
	require.NotNil(t, h.batch)
	require.Len(t, h.batch.PublishBatchRequestEntries, 1)

	entry := h.batch.PublishBatchRequestEntries[0]
	assert.Equal(t, "one", *entry.Id)
	assert.Equal(t, []byte{4, 5}, entry.MessageAttributes["bin"].BinaryValue)
}
