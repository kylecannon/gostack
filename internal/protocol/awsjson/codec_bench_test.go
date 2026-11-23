package awsjson_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// These benchmarks measure the awsjson codec on the generated SQS and STS
// types, so a codec change is decided by measurement. Run them with
//
//	go test -run '^$' -bench 'Codec' -count 10 ./internal/protocol/awsjson
//
// and compare runs with `benchstat`. The values are shaped after LocalStack's
// tests/aws/services/{sqs,sts} snapshots.

type benchCodec struct {
	name      string
	marshal   func(v any) ([]byte, error)
	unmarshal func(data []byte, v any) error
}

// benchCodecs holds the codec awsjson uses.
var benchCodecs = []benchCodec{
	{name: "awsjson", marshal: awsjson.Marshal, unmarshal: awsjson.Unmarshal},
}

const (
	benchQueueURL = "http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/bench-queue"
	benchBody     = "a message body of a typical, unremarkable length for a test queue"
)

// benchJSONMessage is a message body that is json, with nulls in it.
const benchJSONMessage = `{"event":"order.created","id":null,"items":[null,{"sku":"a-1","qty":2}],"note":null}`

// benchBigJSONMessage is a json message body of about 64 KiB, with nulls in it.
func benchBigJSONMessage() string {
	return `{"rows":[` + strings.Repeat(`{"k":null,"v":"0123456789abcdef"},`, 64<<10/36) + `null]}`
}

type decodeFixture struct {
	name string
	body []byte
	newV func() any
}

type encodeFixture struct {
	name string
	v    any
}

func benchAttrsJSON(i int) string {
	return fmt.Sprintf(`{"General":{"DataType":"String","StringValue":"Kenobi-%d"},`+
		`"Hello":{"DataType":"String","StringValue":"There"},`+
		`"Help.Me":{"DataType":"Number","StringValue":"%d"}}`, i, i)
}

func benchDecodeFixtures() []decodeFixture {
	entries := make([]string, 10)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"Id":"msg-%d","MessageBody":%q,"DelaySeconds":%d,"MessageAttributes":%s}`,
			i, benchBody, i%3, benchAttrsJSON(i))
	}

	return []decodeFixture{
		{
			name: "SendMessage",
			body: []byte(`{"QueueUrl":"` + benchQueueURL + `","MessageBody":"` + benchBody + `","DelaySeconds":0,` +
				`"MessageAttributes":` + benchAttrsJSON(0) + `}`),
			newV: func() any { return new(sqs.SendMessageInput) },
		},
		// A message body that is json itself holds the text null, and the request
		// holds no json null: the decode must not pay for dropping one
		// (hasNullToken).
		{
			name: "SendMessageJSONBody",
			body: []byte(`{"QueueUrl":"` + benchQueueURL + `","MessageBody":` + strconv.Quote(benchJSONMessage) +
				`,"DelaySeconds":0,"MessageAttributes":` + benchAttrsJSON(0) + `}`),
			newV: func() any { return new(sqs.SendMessageInput) },
		},
		{
			name: "SendMessageJSONBody64KiB",
			body: []byte(`{"QueueUrl":"` + benchQueueURL + `","MessageBody":` + strconv.Quote(benchBigJSONMessage()) +
				`,"DelaySeconds":0}`),
			newV: func() any { return new(sqs.SendMessageInput) },
		},
		{
			name: "SendMessageBatch10",
			body: []byte(`{"QueueUrl":"` + benchQueueURL + `","Entries":[` + strings.Join(entries, ",") + `]}`),
			newV: func() any { return new(sqs.SendMessageBatchInput) },
		},
		{
			name: "ReceiveMessage",
			body: []byte(`{"QueueUrl":"` + benchQueueURL + `","MaxNumberOfMessages":10,"VisibilityTimeout":30,` +
				`"WaitTimeSeconds":20,"MessageSystemAttributeNames":["All"],"MessageAttributeNames":["All"]}`),
			newV: func() any { return new(sqs.ReceiveMessageInput) },
		},
		{
			name: "STSAssumeRole",
			body: []byte(`{"RoleArn":"arn:aws:iam::000000000000:role/role-1a2b3c4d","RoleSessionName":"session-1a2b3c4d",` +
				`"DurationSeconds":900,"ExternalId":"external-id-1a2b3c4d",` +
				`"PolicyArns":[{"arn":"arn:aws:iam::aws:policy/ReadOnlyAccess"}],` +
				`"Tags":[{"Key":"Team","Value":"platform"},{"Key":"Env","Value":"test"}],"TransitiveTagKeys":["Team"]}`),
			newV: func() any { return new(sts.AssumeRoleInput) },
		},
	}
}

func benchMessage(i int) sqs.Message {
	s := func(v string) *string { return &v }

	return sqs.Message{
		MessageId:              s(fmt.Sprintf("5fea7756-0ea4-451a-a703-a558b933e%03d", i)),
		ReceiptHandle:          s("MbZj6wDWli+JvwwJaBV+3dcjk2YW2vA3+STFFljTM8tJJg6HRG6PYSasuWXPJB+Cw" + strconv.Itoa(i)),
		MD5OfBody:              s("6e2baaf3b97dbeef01c0043275f9a0e7"),
		Body:                   s(benchBody),
		MD5OfMessageAttributes: s("4c360f3fdafd970e05fae2f149d997f5"),
		Attributes: map[string]string{
			"ApproximateFirstReceiveTimestamp": "1700000000123",
			"ApproximateReceiveCount":          "1",
			"MessageDeduplicationId":           "dedup-1",
			"MessageGroupId":                   "group-1",
			"SenderId":                         "000000000000",
			"SentTimestamp":                    "1700000000000",
			"SequenceNumber":                   "18849496460467696128",
		},
		MessageAttributes: map[string]sqs.MessageAttributeValue{
			"General": {DataType: s("String"), StringValue: s("Kenobi")},
			"Hello":   {DataType: s("String"), StringValue: s("There")},
			"Help.Me": {DataType: s("Number"), StringValue: s(strconv.Itoa(i))},
		},
	}
}

// benchErrorBody has the members of the error body WriteError writes.
type benchErrorBody struct {
	Type    string `json:"__type"`
	Message string `json:"message,omitempty"`
}

func benchEncodeFixtures() []encodeFixture {
	ten := &sqs.ReceiveMessageOutput{}
	for i := range 10 {
		ten.Messages = append(ten.Messages, benchMessage(i))
	}

	return []encodeFixture{
		{name: "ReceiveMessage1", v: &sqs.ReceiveMessageOutput{Messages: []sqs.Message{benchMessage(0)}}},
		{name: "ReceiveMessage10", v: ten},
		{name: "Error", v: &benchErrorBody{
			Type:    "InvalidParameterValue",
			Message: "Value 11 for parameter MaxNumberOfMessages is invalid. Reason: Must be between 1 and 10, if provided.",
		}},
	}
}

func BenchmarkCodecDecode(b *testing.B) {
	for _, f := range benchDecodeFixtures() {
		for _, c := range benchCodecs {
			b.Run(f.name+"/codec="+c.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(f.body)))

				for b.Loop() {
					err := c.unmarshal(f.body, f.newV())
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkCodecEncode(b *testing.B) {
	for _, f := range benchEncodeFixtures() {
		for _, c := range benchCodecs {
			b.Run(f.name+"/codec="+c.name, func(b *testing.B) {
				b.ReportAllocs()

				for b.Loop() {
					_, err := c.marshal(f.v)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
