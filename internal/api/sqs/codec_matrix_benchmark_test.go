package sqs

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// The codec matrix: how fast each wire protocol decodes a SendMessageBatch
// request and encodes a ReceiveMessage response, across the payload axis
// (docs/standards.md, "Benchmarks"). Both go through the functions the
// dispatcher calls (the decode reads the request body and, for query, parses
// the form), so a change of codec library shows up as a delta in these cells
// and nothing here names one.
//
//	go test -run '^$' -bench 'BenchmarkDecode|BenchmarkEncode' -count 10 ./internal/api/sqs
//
// Not in the matrix, because they add no codec path: the attribute-less
// SendMessage and ReceiveMessage request shapes (the batch decode contains
// every member of both) and the error body, which BenchmarkCodec* in
// internal/protocol/awsjson covers.

const (
	benchBatchEntries = 10
	// benchMaxBatchBytes is the service's limit on one batch request.
	benchMaxBatchBytes = 1 << 20
	benchPayloadChars  = "abcdefghijklmnopqrstuvwxyz0123456789"
	benchQueueURL      = "http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/bench-queue"
	benchHandle        = "MbZj6wDWli+JvwwJaBV+3dcjk2YW2vA3+STFFljTM8tJJg6HRG6PYSasuWXPJB+Cw"
)

// benchPayloads are the message sizes of the payload axis: small, medium and
// the service's maximum message size.
var benchPayloads = []struct {
	name  string
	bytes int
}{
	{"256B", 256},
	{"64KiB", 64 << 10},
	{"1MiB", 1 << 20},
}

// benchProtocols are the wire protocols SQS speaks.
var benchProtocols = []struct {
	name string
	kind protocol.Kind
}{
	{"json", protocol.KindJSON},
	{"query", protocol.KindQuery},
}

// benchBody returns n bytes of message body.
func benchBody(n int) string {
	return strings.Repeat(benchPayloadChars, n/len(benchPayloadChars)+1)[:n]
}

// benchEntryBytes is the body size of one entry of a batch carrying payload
// bytes per message: the payload, shrunk so ten entries stay inside the batch
// limit.
func benchEntryBytes(payload int) int {
	return min(payload, benchMaxBatchBytes/benchBatchEntries)
}

func benchBatchJSON(entry string) []byte {
	var b strings.Builder

	b.WriteString(`{"QueueUrl":"` + benchQueueURL + `","Entries":[`)

	for i := range benchBatchEntries {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(`{"Id":"entry-` + strconv.Itoa(i) + `","MessageBody":"` + entry + `","DelaySeconds":0,` +
			`"MessageAttributes":{"trace":{"DataType":"String","StringValue":"0123456789abcdef"}}}`)
	}

	b.WriteString(`]}`)

	return []byte(b.String())
}

func benchBatchForm(entry string) url.Values {
	vals := url.Values{"Action": {"SendMessageBatch"}, "Version": {"2012-11-05"}, "QueueUrl": {benchQueueURL}}

	for i := 1; i <= benchBatchEntries; i++ {
		prefix := "SendMessageBatchRequestEntry." + strconv.Itoa(i) + "."
		vals.Set(prefix+"Id", "entry-"+strconv.Itoa(i))
		vals.Set(prefix+"MessageBody", entry)
		vals.Set(prefix+"DelaySeconds", "0")
		vals.Set(prefix+"MessageAttribute.1.Name", "trace")
		vals.Set(prefix+"MessageAttribute.1.Value.DataType", "String")
		vals.Set(prefix+"MessageAttribute.1.Value.StringValue", "0123456789abcdef")
	}

	return vals
}

func BenchmarkDecodeSendMessageBatch(b *testing.B) {
	for _, proto := range benchProtocols {
		for _, payload := range benchPayloads {
			entry := benchBody(benchEntryBytes(payload.bytes))
			total := int64(len(entry) * benchBatchEntries)

			body := benchBatchJSON(entry)
			contentType := "application/x-amz-json-1.0"

			if proto.kind == protocol.KindQuery {
				body = []byte(benchBatchForm(entry).Encode())
				contentType = "application/x-www-form-urlencoded"
			}

			rc := &request.Context{Service: "sqs", Operation: "SendMessageBatch", Protocol: proto.kind, RequestID: "req-1"}

			b.Run("proto="+proto.name+"/payload="+payload.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(total)

				for b.Loop() {
					req, err := http.NewRequestWithContext(b.Context(), http.MethodPost, "/", bytes.NewReader(body))
					if err != nil {
						b.Fatal(err)
					}

					req.Header.Set("Content-Type", contentType)
					rc.Request = req

					in, readErr := readSendMessageBatch(rc)
					if readErr != nil || len(in.Entries) != benchBatchEntries {
						b.Fatal("decode failed", readErr)
					}
				}
			})
		}
	}
}

// benchReceived is a ReceiveMessage output of ten messages of body bytes each.
func benchReceived(body string) *ReceiveMessageOutput {
	out := &ReceiveMessageOutput{}

	for i := range 10 {
		out.Messages = append(out.Messages, Message{
			MessageId:              new("5fea7756-0ea4-451a-a703-a558b933e27" + strconv.Itoa(i)),
			ReceiptHandle:          new(benchHandle + strconv.Itoa(i)),
			MD5OfBody:              new("fafb00f5732ab283681e124bf8747ed1"),
			Body:                   new(body),
			MD5OfMessageAttributes: new("4c360f3fdafd970e05fae2f149d997f5"),
			Attributes: map[string]string{
				"SenderId": "000000000000", "SentTimestamp": "1700000000000", "ApproximateReceiveCount": "1",
			},
			MessageAttributes: map[string]MessageAttributeValue{
				"trace": {DataType: new("String"), StringValue: new("0123456789abcdef")},
			},
		})
	}

	return out
}

// BenchmarkEncodeReceiveMessage writes a ReceiveMessage response of ten
// messages, each of the payload's size. proto=query at payload=1MiB is left out
// under the 10 ms rule (docs/standards.md, "Benchmarks"): the 10 MiB response
// took 12.2 ms, the fastest of three runs at -benchtime=500ms on a darwin/arm64
// laptop. proto=query at 64KiB is its widest cell under the limit, and
// proto=json at 1MiB stays.
func BenchmarkEncodeReceiveMessage(b *testing.B) {
	for _, proto := range benchProtocols {
		for _, payload := range benchPayloads {
			if proto.name == "query" && payload.name == "1MiB" {
				continue
			}

			out := benchReceived(benchBody(payload.bytes))
			rc := &request.Context{Service: "sqs", Operation: "ReceiveMessage", Protocol: proto.kind, RequestID: "req-1"}

			b.Run("proto="+proto.name+"/payload="+payload.name, func(b *testing.B) {
				w := &discard{header: http.Header{}}

				b.ReportAllocs()
				b.SetBytes(int64(payload.bytes * len(out.Messages)))

				for b.Loop() {
					writeReceiveMessage(w, rc, out)
				}
			})
		}
	}
}
