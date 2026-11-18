package sqs_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provider is a Handler whose operations are set by the test.
type provider struct {
	sqs.Unimplemented

	sendMessage    func(context.Context, *sqs.SendMessageInput) (*sqs.SendMessageOutput, error)
	createQueue    func(context.Context, *sqs.CreateQueueInput) (*sqs.CreateQueueOutput, error)
	receiveMessage func(context.Context, *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error)
	getQueueURL    func(context.Context, *sqs.GetQueueUrlInput) (*sqs.GetQueueUrlOutput, error)
	listQueues     func(context.Context, *sqs.ListQueuesInput) (*sqs.ListQueuesOutput, error)
}

func (p *provider) SendMessage(ctx context.Context, in *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
	return p.sendMessage(ctx, in)
}

func (p *provider) CreateQueue(ctx context.Context, in *sqs.CreateQueueInput) (*sqs.CreateQueueOutput, error) {
	return p.createQueue(ctx, in)
}

func (p *provider) ReceiveMessage(
	ctx context.Context, in *sqs.ReceiveMessageInput,
) (*sqs.ReceiveMessageOutput, error) {
	return p.receiveMessage(ctx, in)
}

func (p *provider) GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput) (*sqs.GetQueueUrlOutput, error) {
	return p.getQueueURL(ctx, in)
}

func (p *provider) ListQueues(ctx context.Context, in *sqs.ListQueuesInput) (*sqs.ListQueuesOutput, error) {
	return p.listQueues(ctx, in)
}

const requestID = "req-0001"

func call(t *testing.T, serve func(sqs.Handler, http.ResponseWriter, *request.Context), h sqs.Handler,
	op string, kind protocol.Kind, body string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	if kind == protocol.KindQuery {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	rec := httptest.NewRecorder()
	serve(h, rec, &request.Context{
		Service: "sqs", Operation: op, Protocol: kind, AccountID: "000000000000",
		Region: "us-east-1", RequestID: requestID, Request: req,
	})

	return rec
}

func form(pairs ...string) string {
	v := url.Values{}
	for i := 0; i < len(pairs); i += 2 {
		v.Add(pairs[i], pairs[i+1])
	}

	return v.Encode()
}

func header(rec *httptest.ResponseRecorder, key string) []string {
	return map[string][]string(rec.Header())[key]
}

func Test_Metadata(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "sqs", sqs.Metadata.Name)
	assert.Equal(t, "sqs", sqs.Metadata.SigningName)
	assert.Equal(t, "AmazonSQS", sqs.Metadata.TargetPrefix)
	assert.Equal(t, "sqs", sqs.Metadata.EndpointPrefix)
	assert.Equal(t, "2012-11-05", sqs.Metadata.APIVersion)
	assert.ElementsMatch(t, []protocol.Kind{protocol.KindJSON, protocol.KindQuery}, sqs.Metadata.Protocols)
	assert.Len(t, sqs.Operations, 23)
	assert.Contains(t, sqs.Operations, "SendMessage")
}

func Test_Serve_QueryDecode(t *testing.T) {
	t.Parallel()

	var got *sqs.SendMessageInput

	h := &provider{sendMessage: func(_ context.Context, in *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
		got = in

		return &sqs.SendMessageOutput{MessageId: new("m-1"), MD5OfMessageBody: new(`md5 "q"`)}, nil
	}}

	body := form(
		"Action", "SendMessage", "Version", "2012-11-05",
		"QueueUrl", "http://localhost:4566/000000000000/q",
		"MessageBody", "hello",
		"DelaySeconds", "5",
		"MessageAttribute.1.Name", "color",
		"MessageAttribute.1.Value.DataType", "String",
		"MessageAttribute.1.Value.StringValue", "red",
		"MessageAttribute.2.Name", "blob",
		"MessageAttribute.2.Value.DataType", "Binary",
		"MessageAttribute.2.Value.BinaryValue", "aGk=",
		"MessageAttribute.2.Value.StringListValue.1", "a",
		"MessageAttribute.2.Value.StringListValue.2", "b",
		"MessageSystemAttribute.1.Name", "AWSTraceHeader",
		"MessageSystemAttribute.1.Value.DataType", "String",
		"MessageSystemAttribute.1.Value.StringValue", "Root=1",
	)

	rec := call(t, sqs.Serve, h, "SendMessage", protocol.KindQuery, body)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, got)
	assert.Equal(t, "hello", *got.MessageBody)
	assert.Equal(t, int32(5), *got.DelaySeconds)
	assert.Equal(t, "red", *got.MessageAttributes["color"].StringValue)
	assert.Equal(t, "String", *got.MessageAttributes["color"].DataType)
	assert.Equal(t, []byte("hi"), got.MessageAttributes["blob"].BinaryValue)
	assert.Equal(t, []string{"a", "b"}, got.MessageAttributes["blob"].StringListValues)
	assert.Equal(t, "Root=1", *got.MessageSystemAttributes["AWSTraceHeader"].StringValue)

	assert.Equal(t, []string{"text/xml"}, header(rec, "Content-Type"))
	assert.Equal(t, []string{requestID}, header(rec, "x-amzn-RequestId"))
	assert.Contains(t, rec.Body.String(), `<MD5OfMessageBody>md5 &quot;q&quot;</MD5OfMessageBody>`,
		"SQS writes quotes as entities")
	assert.True(t, strings.HasPrefix(rec.Body.String(),
		"<?xml version='1.0' encoding='utf-8'?>\n"+
			`<SendMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/"><SendMessageResult>`))
}

func Test_Serve_QueryAliases(t *testing.T) {
	t.Parallel()

	// CreateQueue names its tags member "tags", which is what its alias is; the
	// Java SDK's "Tags" spelling is the one TagQueue takes.
	tests := []struct {
		name     string
		giveForm []string
		wantAttr map[string]string
		wantTags map[string]string
	}{
		{
			name:     "the spec names",
			giveForm: []string{"Attribute.1.Name", "VisibilityTimeout", "Attribute.1.Value", "30", "Tag.1.Key", "env", "Tag.1.Value", "dev"},
		},
		{
			name:     "the member names",
			giveForm: []string{"Attributes.1.Name", "VisibilityTimeout", "Attributes.1.Value", "30", "tags.1.Key", "env", "tags.1.Value", "dev"},
		},
		{
			name:     "the other spelling of the tags member is not an alias here",
			giveForm: []string{"Attributes.1.Name", "VisibilityTimeout", "Attributes.1.Value", "30", "Tags.1.Key", "env", "Tags.1.Value", "dev"},
		},
	}

	for i := range tests {
		tests[i].wantAttr = map[string]string{"VisibilityTimeout": "30"}
		tests[i].wantTags = map[string]string{"env": "dev"}
	}

	tests[2].wantTags = nil

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got *sqs.CreateQueueInput

			h := &provider{createQueue: func(_ context.Context, in *sqs.CreateQueueInput) (*sqs.CreateQueueOutput, error) {
				got = in

				return &sqs.CreateQueueOutput{QueueUrl: new("http://localhost:4566/000000000000/q")}, nil
			}}

			body := form(append([]string{"Action", "CreateQueue", "QueueName", "q"}, tt.giveForm...)...)
			rec := call(t, sqs.Serve, h, "CreateQueue", protocol.KindQuery, body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantAttr, got.Attributes)
			assert.Equal(t, tt.wantTags, got.Tags)
		})
	}
}

func Test_Serve_TagQueueAlias(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{"Tag", "Tags"} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()

			var got *sqs.TagQueueInput

			h := &tagger{fn: func(in *sqs.TagQueueInput) { got = in }}
			body := form("QueueUrl", "q", spelling+".1.Key", "env", spelling+".1.Value", "dev")
			rec := call(t, sqs.Serve, h, "TagQueue", protocol.KindQuery, body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, map[string]string{"env": "dev"}, got.Tags)
			assert.True(t, strings.HasSuffix(rec.Body.String(), "<ResponseMetadata><RequestId>"+requestID+
				"</RequestId></ResponseMetadata></TagQueueResponse>"), "an operation without output has only metadata")
		})
	}
}

type tagger struct {
	sqs.Unimplemented

	fn func(*sqs.TagQueueInput)
}

func (p *tagger) TagQueue(_ context.Context, in *sqs.TagQueueInput) (*sqs.TagQueueOutput, error) {
	p.fn(in)

	return nil, nil //nolint:nilnil // an operation with no output returns an empty one, and nil stands for it
}

func Test_Serve_QueryFlattenedLists(t *testing.T) {
	t.Parallel()

	var got *sqs.ReceiveMessageInput

	h := &provider{receiveMessage: func(_ context.Context, in *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
		got = in

		return &sqs.ReceiveMessageOutput{Messages: []sqs.Message{
			{
				MessageId: new("m-1"), Body: new("a<b"),
				Attributes:        map[string]string{"SentTimestamp": "1", "ApproximateReceiveCount": "2"},
				MessageAttributes: map[string]sqs.MessageAttributeValue{"k": {DataType: new("String"), StringValue: new("v")}},
			},
			{MessageId: new("m-2"), Body: new("b")},
		}}, nil
	}}

	body := form(
		"Action", "ReceiveMessage", "QueueUrl", "q", "MaxNumberOfMessages", "2",
		"AttributeName.1", "All", "AttributeName.2", "SentTimestamp",
		"MessageAttributeName.1", "All",
	)
	rec := call(t, sqs.Serve, h, "ReceiveMessage", protocol.KindQuery, body)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int32(2), *got.MaxNumberOfMessages)
	assert.Equal(t, []string{"All", "SentTimestamp"}, got.MessageSystemAttributeNames)
	assert.Equal(t, []string{"All"}, got.MessageAttributeNames)

	var resp struct {
		Messages []struct {
			MessageID  string `xml:"MessageId"`
			Body       string
			Attributes []struct{ Name, Value string } `xml:"Attribute"`
			Custom     []struct {
				Name  string
				Value struct{ DataType, StringValue string }
			} `xml:"MessageAttribute"`
		} `xml:"ReceiveMessageResult>Message"`
		RequestID string `xml:"ResponseMetadata>RequestId"`
	}

	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Messages, 2)
	assert.Equal(t, "a<b", resp.Messages[0].Body)
	assert.Equal(t, "m-2", resp.Messages[1].MessageID)
	assert.Equal(t, requestID, resp.RequestID)
	assert.Equal(t, "ApproximateReceiveCount", resp.Messages[0].Attributes[0].Name, "map keys are written sorted")
	assert.Equal(t, "k", resp.Messages[0].Custom[0].Name)
	assert.Equal(t, "v", resp.Messages[0].Custom[0].Value.StringValue)
}

func Test_Serve_JSON(t *testing.T) {
	t.Parallel()

	var got *sqs.SendMessageInput

	h := &provider{sendMessage: func(_ context.Context, in *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
		got = in

		return &sqs.SendMessageOutput{MessageId: new("m-1")}, nil
	}}

	body := `{"QueueUrl":"q","MessageBody":"hi","DelaySeconds":0,
		"MessageAttributes":{"k":{"DataType":"Binary","BinaryValue":"aGk="}}}`
	rec := call(t, sqs.Serve, h, "SendMessage", protocol.KindJSON, body)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Zero(t, *got.DelaySeconds, "an explicit zero is kept")
	assert.Equal(t, []byte("hi"), got.MessageAttributes["k"].BinaryValue)
	assert.Equal(t, []string{"application/x-amz-json-1.0"}, header(rec, "Content-Type"))
	assert.Equal(t, []string{requestID}, header(rec, "x-amzn-RequestId"))
	assert.JSONEq(t, `{"MessageId":"m-1"}`, rec.Body.String())
}

func Test_Serve_JSONEmptyOutput(t *testing.T) {
	t.Parallel()

	h := &provider{listQueues: func(context.Context, *sqs.ListQueuesInput) (*sqs.ListQueuesOutput, error) {
		return &sqs.ListQueuesOutput{QueueUrls: []string{}}, nil
	}}

	rec := call(t, sqs.Serve, h, "ListQueues", protocol.KindJSON, `{}`)

	assert.JSONEq(t, `{"QueueUrls":[]}`, rec.Body.String(), "an empty list is written, a nil one is not")
}

func Test_Serve_Errors(t *testing.T) {
	t.Parallel()

	gone := func(context.Context, *sqs.GetQueueUrlInput) (*sqs.GetQueueUrlOutput, error) {
		return nil, sqs.NewQueueDoesNotExist("The specified queue does not exist.")
	}
	boom := func(context.Context, *sqs.GetQueueUrlInput) (*sqs.GetQueueUrlOutput, error) {
		return nil, errors.New("disk on fire")
	}

	t.Run("json writes the shape name and the query code header", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &provider{getQueueURL: gone}, "GetQueueUrl", protocol.KindJSON, `{"QueueName":"q"}`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, []string{"application/json"}, header(rec, "Content-Type"))
		assert.Equal(t, []string{"QueueDoesNotExist"}, header(rec, "X-Amzn-Errortype"))
		assert.Equal(t, []string{"AWS.SimpleQueueService.NonExistentQueue;Sender"}, header(rec, "x-amzn-query-error"))
		assert.JSONEq(t, `{"__type":"QueueDoesNotExist","message":"The specified queue does not exist."}`, rec.Body.String())
	})

	t.Run("query writes the query code and the sender type", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &provider{getQueueURL: gone}, "GetQueueUrl", protocol.KindQuery, form("QueueName", "q"))

		assert.Equal(t, http.StatusBadRequest, rec.Code)

		body := rec.Body.String()
		assert.Contains(t, body, "<Code>AWS.SimpleQueueService.NonExistentQueue</Code>")
		assert.Contains(t, body, "<Type>Sender</Type>")
		assert.Contains(t, body, "<RequestId>"+requestID+"</RequestId>")
	})

	t.Run("an error that is not an awserr is an internal error", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &provider{getQueueURL: boom}, "GetQueueUrl", protocol.KindJSON, `{}`)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.JSONEq(t, `{"__type":"InternalError","message":"disk on fire"}`, rec.Body.String())
	})

	t.Run("a malformed json body is a 400", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &provider{}, "GetQueueUrl", protocol.KindJSON, `{"QueueName":`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "SerializationException")
	})

	t.Run("a bad number in a query request is a 400", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &provider{}, "SendMessage", protocol.KindQuery, form("DelaySeconds", "soon"))

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "<Code>InvalidParameterValue</Code>")
	})

	t.Run("an unknown operation is a 400", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &provider{}, "Frobnicate", protocol.KindQuery, "")

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "<Code>InvalidAction</Code>")
	})

	t.Run("unimplemented operations answer 501", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sqs.Serve, &sqs.Unimplemented{}, "PurgeQueue", protocol.KindJSON, `{"QueueUrl":"q"}`)

		assert.Equal(t, http.StatusNotImplemented, rec.Code)
		assert.Contains(t, rec.Body.String(), "InternalFailure")
	})
}

func Test_ServeValidated(t *testing.T) {
	t.Parallel()

	ran := false
	h := &provider{sendMessage: func(context.Context, *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
		ran = true

		return &sqs.SendMessageOutput{}, nil
	}}

	t.Run("Serve leaves a missing member to the provider", func(t *testing.T) {
		rec := call(t, sqs.Serve, h, "SendMessage", protocol.KindQuery, form("QueueUrl", "q"))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, ran)
	})

	t.Run("ServeValidated rejects it first", func(t *testing.T) {
		ran = false
		rec := call(t, sqs.ServeValidated, h, "SendMessage", protocol.KindQuery, form("QueueUrl", "q"))

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.False(t, ran)
		assert.Contains(t, rec.Body.String(), "<Code>MissingParameter</Code>")
		assert.Contains(t, rec.Body.String(), "<Message>The request must contain the parameter MessageBody.</Message>")
	})

	t.Run("ServeValidated looks into nested members", func(t *testing.T) {
		rec := call(t, sqs.ServeValidated, h, "SendMessage", protocol.KindQuery, form(
			"QueueUrl", "q", "MessageBody", "b", "MessageAttribute.1.Name", "k", "MessageAttribute.1.Value.StringValue", "v"))

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "the parameter DataType.")
	})

	t.Run("ServeValidated passes a complete request", func(t *testing.T) {
		ran = false
		rec := call(t, sqs.ServeValidated, h, "SendMessage", protocol.KindQuery, form("QueueUrl", "q", "MessageBody", "b"))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, ran)
	})
}

func Test_Exceptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		give *awserr.Error
		want awserr.Error
	}{
		{
			name: "renamed query code",
			give: sqs.NewQueueNameExists("m"),
			want: awserr.Error{
				Code: "QueueNameExists", Message: "m", StatusCode: 400,
				Fault: awserr.FaultSender, QueryCode: "QueueAlreadyExists",
			},
		},
		{
			name: "prefixed query code",
			give: sqs.NewEmptyBatchRequest("m"),
			want: awserr.Error{
				Code: "EmptyBatchRequest", Message: "m", StatusCode: 400,
				Fault: awserr.FaultSender, QueryCode: "AWS.SimpleQueueService.EmptyBatchRequest",
			},
		},
		{
			name: "a code the query model keeps",
			give: sqs.NewOverLimit("m"),
			want: awserr.Error{
				Code: "OverLimit", Message: "m", StatusCode: 400,
				Fault: awserr.FaultSender, QueryCode: "OverLimit",
			},
		},
		{
			name: "no sender fault in the query model",
			give: sqs.NewInvalidMessageContents("m"),
			want: awserr.Error{
				Code: "InvalidMessageContents", Message: "m", StatusCode: 400,
				Fault: awserr.FaultReceiver, QueryCode: "InvalidMessageContents",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, *tt.give)
		})
	}
}
