package sqs

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/query"
)

// discard is a ResponseWriter that keeps nothing, so a benchmark measures the
// encoder and not a recorder.
type discard struct{ header http.Header }

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}

func BenchmarkSQSSendMessageQueryDecode(b *testing.B) {
	vals := url.Values{
		"Action":      {"SendMessage"},
		"Version":     {"2012-11-05"},
		"QueueUrl":    {"http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/bench-queue"},
		"MessageBody": {"a message body of a typical, unremarkable length for a test queue"},

		"DelaySeconds":                         {"0"},
		"MessageAttribute.1.Name":              {"trace"},
		"MessageAttribute.1.Value.DataType":    {"String"},
		"MessageAttribute.1.Value.StringValue": {"0123456789abcdef"},
		"MessageAttribute.2.Name":              {"attempt"},
		"MessageAttribute.2.Value.DataType":    {"Number"},
		"MessageAttribute.2.Value.StringValue": {"3"},
	}

	b.ReportAllocs()

	for b.Loop() {
		in, err := decodeSendMessageQuery(vals)
		if err != nil || in.MessageBody == nil {
			b.Fatal("decode failed")
		}
	}
}

func BenchmarkSQSSendMessageJSONDecode(b *testing.B) {
	body := []byte(`{"QueueUrl":"http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/bench-queue",` +
		`"MessageBody":"a message body of a typical, unremarkable length for a test queue","DelaySeconds":0,` +
		`"MessageAttributes":{"trace":{"DataType":"String","StringValue":"0123456789abcdef"},` +
		`"attempt":{"DataType":"Number","StringValue":"3"}}}`)

	b.ReportAllocs()

	for b.Loop() {
		in, err := decodeSendMessageJSON(body)
		if err != nil || in.MessageBody == nil {
			b.Fatal("decode failed")
		}
	}
}

func BenchmarkSQSReceiveMessageQueryEncode(b *testing.B) {
	out := &ReceiveMessageOutput{}

	for range 10 {
		out.Messages = append(out.Messages, Message{
			MessageId:     new("5fea7756-0ea4-451a-a703-a558b933e274"),
			ReceiptHandle: new("MbZj6wDWli+JvwwJaBV+3dcjk2YW2vA3+STFFljTM8tJJg6HRG6PYSasuWXPJB+Cw"),
			MD5OfBody:     new("fafb00f5732ab283681e124bf8747ed1"),
			Body:          new("a message body of a typical, unremarkable length for a test queue"),
			Attributes: map[string]string{
				"SenderId": "000000000000", "SentTimestamp": "1700000000000", "ApproximateReceiveCount": "1",
			},
			MessageAttributes: map[string]MessageAttributeValue{
				"trace": {DataType: new("String"), StringValue: new("0123456789abcdef")},
			},
		})
	}

	env := &query.Envelope{
		Action: "ReceiveMessage", Namespace: queryNamespace, Result: "ReceiveMessageResult",
		RequestID: "d2c8a1e0-6d1a-4a5b-9f3e-0a1b2c3d4e5f", EscapeQuotes: queryEscapeQuotes,
	}
	w := &discard{header: http.Header{}}

	b.ReportAllocs()

	for b.Loop() {
		query.WriteResponse(w, env, out.encodeQuery)
	}
}
