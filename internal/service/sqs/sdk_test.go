package sqs_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	sdk "github.com/aws/aws-sdk-go-v2/service/sqs"
	sdktypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiError is what the SDK's errors of a service have in common.
type apiError interface {
	error
	ErrorCode() string
}

// handlerTransport sends a request of the SDK straight into an http.Handler, so
// no socket is involved and a synctest bubble can run the whole exchange.
type handlerTransport struct{ handler http.Handler }

func (rt handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)

	return rec.Result(), nil
}

// newSDK returns the SDK's own SQS client, which speaks the json protocol, wired
// to the provider behind the real gateway. The SDK checks the MD5 of every body
// and attribute set it sends and receives, so a wrong checksum fails a call.
func newSDK(t *testing.T) (*sdk.Client, string) {
	t.Helper()

	s := newStack(t)
	client := sdk.New(sdk.Options{
		Region:           usEast1,
		Credentials:      credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint:     aws.String("http://localhost:4566"),
		HTTPClient:       &http.Client{Transport: handlerTransport{handler: s.handler}},
		RetryMaxAttempts: 1,
	})

	created, err := client.CreateQueue(t.Context(), &sdk.CreateQueueInput{QueueName: aws.String("sdk")})
	require.NoError(t, err)

	return client, aws.ToString(created.QueueUrl)
}

func Test_SDK_send_receive_delete(t *testing.T) {
	client, url := newSDK(t)
	ctx := t.Context()

	binary := []byte("traceparent\x1e00-774062d6c37081a5a0b9b5b88e30627c-2d2482211f6489da-01")
	sent, err := client.SendMessage(ctx, &sdk.SendMessageInput{
		QueueUrl:    &url,
		MessageBody: aws.String("hello"),
		MessageAttributes: map[string]sdktypes.MessageAttributeValue{
			"text":   {DataType: aws.String("String"), StringValue: aws.String("x")},
			"number": {DataType: aws.String("Number"), StringValue: aws.String("1493147359900")},
			"binary": {DataType: aws.String("Binary"), BinaryValue: binary},
		},
		MessageSystemAttributes: map[string]sdktypes.MessageSystemAttributeValue{
			"AWSTraceHeader": {DataType: aws.String("String"), StringValue: aws.String(traceHeaderValue)},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, md5Trace, aws.ToString(sent.MD5OfMessageSystemAttributes))

	received, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{
		QueueUrl:                    &url,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []sdktypes.MessageSystemAttributeName{sdktypes.MessageSystemAttributeNameAll},
	})
	require.NoError(t, err)
	require.Len(t, received.Messages, 1)

	m := received.Messages[0]
	assert.Equal(t, aws.ToString(sent.MessageId), aws.ToString(m.MessageId))
	assert.Equal(t, "hello", aws.ToString(m.Body))
	assert.Equal(t, aws.ToString(sent.MD5OfMessageAttributes), aws.ToString(m.MD5OfMessageAttributes))
	assert.Equal(t, binary, m.MessageAttributes["binary"].BinaryValue)
	assert.Equal(t, "1", m.Attributes["ApproximateReceiveCount"])
	assert.Equal(t, traceHeaderValue, m.Attributes["AWSTraceHeader"])

	_, err = client.DeleteMessage(ctx, &sdk.DeleteMessageInput{QueueUrl: &url, ReceiptHandle: m.ReceiptHandle})
	require.NoError(t, err)

	_, err = client.ChangeMessageVisibility(ctx, &sdk.ChangeMessageVisibilityInput{
		QueueUrl: &url, ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 10,
	})

	var apiErr apiError

	require.ErrorAs(t, err, &apiErr, "the message is gone")
	assert.Equal(t, codeInvalidParameterException, apiErr.ErrorCode())

	attrs, err := client.GetQueueAttributes(ctx, &sdk.GetQueueAttributesInput{
		QueueUrl: &url, AttributeNames: []sdktypes.QueueAttributeName{sdktypes.QueueAttributeNameAll},
	})
	require.NoError(t, err)
	assert.Equal(t, "0", attrs.Attributes["ApproximateNumberOfMessages"])
}

func Test_SDK_errors(t *testing.T) {
	client, url := newSDK(t)
	ctx := t.Context()

	_, err := client.DeleteMessage(ctx, &sdk.DeleteMessageInput{QueueUrl: &url, ReceiptHandle: aws.String(garbageHandle)})

	var badHandle *sdktypes.ReceiptHandleIsInvalid
	require.ErrorAs(t, err, &badHandle)
	assert.Equal(t, notAValidHandle(garbageHandle), badHandle.ErrorMessage())

	missing := url + "-missing"
	_, err = client.SendMessage(ctx, &sdk.SendMessageInput{QueueUrl: &missing, MessageBody: aws.String("x")})

	var noQueue *sdktypes.QueueDoesNotExist
	require.ErrorAs(t, err, &noQueue)

	_, err = client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: &url, MaxNumberOfMessages: 11})

	var apiErr apiError

	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, codeInvalidParameterException, apiErr.ErrorCode())
}

func Test_SDK_batches(t *testing.T) {
	client, url := newSDK(t)
	ctx := t.Context()

	sent, err := client.SendMessageBatch(ctx, &sdk.SendMessageBatchInput{
		QueueUrl: &url,
		Entries: []sdktypes.SendMessageBatchRequestEntry{
			{Id: aws.String("ok"), MessageBody: aws.String("a")},
			{Id: aws.String("bad"), MessageBody: aws.String("b\x01")},
			{Id: aws.String("also-ok"), MessageBody: aws.String("c")},
		},
	})
	require.NoError(t, err)
	require.Len(t, sent.Successful, 2)
	require.Len(t, sent.Failed, 1)
	assert.Equal(t, "bad", aws.ToString(sent.Failed[0].Id))
	assert.Equal(t, "InvalidMessageContents", aws.ToString(sent.Failed[0].Code))

	received, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: &url, MaxNumberOfMessages: 10})
	require.NoError(t, err)
	require.Len(t, received.Messages, 2)

	deleted, err := client.DeleteMessageBatch(ctx, &sdk.DeleteMessageBatchInput{
		QueueUrl: &url,
		Entries: []sdktypes.DeleteMessageBatchRequestEntry{
			{Id: aws.String("first"), ReceiptHandle: received.Messages[0].ReceiptHandle},
			{Id: aws.String("junk"), ReceiptHandle: aws.String(garbageHandle)},
		},
	})
	require.NoError(t, err)
	require.Len(t, deleted.Successful, 1)
	require.Len(t, deleted.Failed, 1)
	assert.Equal(t, "ReceiptHandleIsInvalid", aws.ToString(deleted.Failed[0].Code))
	assert.False(t, deleted.Failed[0].SenderFault)

	changed, err := client.ChangeMessageVisibilityBatch(ctx, &sdk.ChangeMessageVisibilityBatchInput{
		QueueUrl: &url, Entries: []sdktypes.ChangeMessageVisibilityBatchRequestEntry{
			{Id: aws.String("second"), ReceiptHandle: received.Messages[1].ReceiptHandle, VisibilityTimeout: 0},
		},
	})
	require.NoError(t, err)
	assert.Len(t, changed.Successful, 1)

	_, err = client.SendMessageBatch(ctx, &sdk.SendMessageBatchInput{
		QueueUrl: &url, Entries: []sdktypes.SendMessageBatchRequestEntry{},
	})

	var empty *sdktypes.EmptyBatchRequest
	require.ErrorAs(t, err, &empty)
}

func Test_SDK_long_poll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, url := newSDK(t)
		ctx := t.Context()

		var (
			got     *sdk.ReceiveMessageOutput
			callErr error
		)

		start := time.Now()
		done := make(chan struct{})

		go func() {
			defer close(done)

			got, callErr = client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: &url, WaitTimeSeconds: 20})
		}()

		synctest.Wait()
		time.Sleep(4 * time.Second)

		_, err := client.SendMessage(ctx, &sdk.SendMessageInput{QueueUrl: &url, MessageBody: aws.String("wake")})
		require.NoError(t, err)
		<-done

		require.NoError(t, callErr)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, "wake", aws.ToString(got.Messages[0].Body))
		assert.Equal(t, 4*time.Second, time.Since(start))

		// A poll with nothing to deliver ends with the wait.
		start = time.Now()
		empty, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: &url, WaitTimeSeconds: 7})
		require.NoError(t, err)
		assert.Empty(t, empty.Messages)
		assert.Equal(t, 7*time.Second, time.Since(start))
	})
}

func Test_SDK_visibility_expiry_redelivers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, url := newSDK(t)
		ctx := t.Context()

		_, err := client.SendMessage(ctx, &sdk.SendMessageInput{QueueUrl: &url, MessageBody: aws.String("again")})
		require.NoError(t, err)

		first, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: &url, VisibilityTimeout: 5})
		require.NoError(t, err)
		require.Len(t, first.Messages, 1)

		time.Sleep(5 * time.Second)

		second, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{
			QueueUrl: &url, MessageSystemAttributeNames: []sdktypes.MessageSystemAttributeName{
				sdktypes.MessageSystemAttributeNameApproximateReceiveCount,
			},
		})
		require.NoError(t, err)
		require.Len(t, second.Messages, 1)
		assert.Equal(t, aws.ToString(first.Messages[0].MessageId), aws.ToString(second.Messages[0].MessageId))
		assert.Equal(t, "2", second.Messages[0].Attributes["ApproximateReceiveCount"])
	})
}
