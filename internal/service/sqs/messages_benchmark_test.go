package sqs_test

import (
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/service/sqs"
)

// BenchmarkSendReceiveDelete measures a message's whole life in process: one
// SendMessage, one ReceiveMessage and one DeleteMessage on the provider, without
// the gateway or a codec.
func BenchmarkSendReceiveDelete(b *testing.B) {
	p := sqs.New(nil)
	ctx := direct(b.Context())

	created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("bench")})
	if err != nil {
		b.Fatal(err)
	}

	send := &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: new("hello")}
	receive := &sqsapi.ReceiveMessageInput{QueueUrl: created.QueueUrl}

	b.ReportAllocs()

	for b.Loop() {
		_, err = p.SendMessage(ctx, send)
		if err != nil {
			b.Fatal(err)
		}

		out, err := p.ReceiveMessage(ctx, receive)
		if err != nil || len(out.Messages) != 1 {
			b.Fatal("receive", err, out)
		}

		_, err = p.DeleteMessage(ctx, &sqsapi.DeleteMessageInput{
			QueueUrl: created.QueueUrl, ReceiptHandle: out.Messages[0].ReceiptHandle,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
