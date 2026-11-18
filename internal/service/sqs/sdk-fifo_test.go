package sqs_test

import (
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/sqs"
	sdktypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_SDK_FIFO_send_receive_delete(t *testing.T) {
	client, _ := newSDK(t)
	ctx := t.Context()

	created, err := client.CreateQueue(ctx, &sdk.CreateQueueInput{
		QueueName:  aws.String("orders.fifo"),
		Attributes: map[string]string{attrFifoQueue: valueTrue, attrContentDedu: valueTrue},
	})
	require.NoError(t, err)

	url := created.QueueUrl

	first, err := client.SendMessage(ctx, &sdk.SendMessageInput{
		QueueUrl: url, MessageBody: aws.String("order"), MessageGroupId: aws.String("customer"),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, aws.ToString(first.SequenceNumber))

	again, err := client.SendMessage(ctx, &sdk.SendMessageInput{
		QueueUrl: url, MessageBody: aws.String("order"), MessageGroupId: aws.String("customer"),
	})
	require.NoError(t, err)
	assert.Equal(t, aws.ToString(first.MessageId), aws.ToString(again.MessageId))

	got, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{
		QueueUrl:                    url,
		MessageSystemAttributeNames: []sdktypes.MessageSystemAttributeName{sdktypes.MessageSystemAttributeNameAll},
	})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "customer", got.Messages[0].Attributes["MessageGroupId"])
	assert.Equal(t, aws.ToString(first.SequenceNumber), got.Messages[0].Attributes["SequenceNumber"])

	_, err = client.DeleteMessage(ctx, &sdk.DeleteMessageInput{
		QueueUrl: url, ReceiptHandle: got.Messages[0].ReceiptHandle,
	})
	require.NoError(t, err)

	_, err = client.SendMessage(ctx, &sdk.SendMessageInput{QueueUrl: url, MessageBody: aws.String("no group")})

	var apiErr apiError

	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "MissingRequiredParameterException", apiErr.ErrorCode())
}

func Test_SDK_dead_letter_queue_and_move_task(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, _ := newSDK(t)
		ctx := t.Context()

		dead, err := client.CreateQueue(ctx, &sdk.CreateQueueInput{QueueName: aws.String("dead")})
		require.NoError(t, err)

		deadAttrs, err := client.GetQueueAttributes(ctx, &sdk.GetQueueAttributesInput{
			QueueUrl: dead.QueueUrl, AttributeNames: []sdktypes.QueueAttributeName{sdktypes.QueueAttributeNameQueueArn},
		})
		require.NoError(t, err)

		deadARN := deadAttrs.Attributes[attrQueueArn]
		policy, err := json.Marshal(map[string]any{"deadLetterTargetArn": deadARN, "maxReceiveCount": 1})
		require.NoError(t, err)

		source, err := client.CreateQueue(ctx, &sdk.CreateQueueInput{
			QueueName:  aws.String("source"),
			Attributes: map[string]string{attrRedrivePolicy: string(policy), attrVisibilityTimeout: "0"},
		})
		require.NoError(t, err)

		sources, err := client.ListDeadLetterSourceQueues(ctx, &sdk.ListDeadLetterSourceQueuesInput{QueueUrl: dead.QueueUrl})
		require.NoError(t, err)
		assert.Equal(t, []string{aws.ToString(source.QueueUrl)}, sources.QueueUrls)

		_, err = client.SendMessage(ctx, &sdk.SendMessageInput{QueueUrl: source.QueueUrl, MessageBody: aws.String("poison")})
		require.NoError(t, err)

		for range 2 {
			_, err = client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: source.QueueUrl})
			require.NoError(t, err)
		}

		started, err := client.StartMessageMoveTask(ctx, &sdk.StartMessageMoveTaskInput{SourceArn: aws.String(deadARN)})
		require.NoError(t, err)
		assert.NotEmpty(t, aws.ToString(started.TaskHandle))

		synctest.Wait()
		time.Sleep(time.Second)

		listed, err := client.ListMessageMoveTasks(ctx, &sdk.ListMessageMoveTasksInput{SourceArn: aws.String(deadARN)})
		require.NoError(t, err)
		require.Len(t, listed.Results, 1)
		assert.Equal(t, "COMPLETED", aws.ToString(listed.Results[0].Status))
		assert.EqualValues(t, 1, listed.Results[0].ApproximateNumberOfMessagesMoved)

		back, err := client.ReceiveMessage(ctx, &sdk.ReceiveMessageInput{QueueUrl: source.QueueUrl})
		require.NoError(t, err)
		require.Len(t, back.Messages, 1)
		assert.Equal(t, "poison", aws.ToString(back.Messages[0].Body))

		// A handle that is not one is the caller's mistake.
		_, err = client.CancelMessageMoveTask(ctx, &sdk.CancelMessageMoveTaskInput{TaskHandle: aws.String("foobared")})

		var apiErr apiError

		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, codeInvalidParameterException, apiErr.ErrorCode())
	})
}
