package dynamodb_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// The encoder benchmarks write a read's output into a reused buffer, its items
// once typed and once pre-encoded (docs/architecture.md, "Pre-encoded
// values"):
//
//	BenchmarkEncodeGetItem/form=encoded/payload=399KiB
//	BenchmarkEncodeQuery/form=typed/items=100/payload=4KiB
//
// Each item is a key and one string attribute of the payload's size. The list
// outputs leave out 399KiB: one item of it is GetItem's cell, and 100 of them
// are past a Query page (1 MB) and BatchGetItem's 16 MB. The slowest cells,
// typed at items=100/payload=4KiB, take about 0.08 ms on an M1 Max.

type encodePayload struct {
	name string
	size int
}

var (
	encodeForms        = []string{"typed", "encoded"}
	encodePayloads    = []encodePayload{{"100B", 100}, {"4KiB", 4 << 10}, {"399KiB", 399 << 10}}
	encodeListPayloads = encodePayloads[:2]
	encodeItemCounts   = []int{1, 10, 100}
)

// encodeOutput builds one output from typed items, or from the same items
// already encoded when encoded is not nil.
type encodeOutput func(typed []map[string]dynamodb.AttributeValue, encoded []awsjson.Encoded) awsjson.Appender

func BenchmarkEncodeGetItem(b *testing.B) {
	out := func(typed []map[string]dynamodb.AttributeValue, encoded []awsjson.Encoded) awsjson.Appender {
		if encoded != nil {
			return &dynamodb.GetItemOutput{ItemJSON: encoded[0]}
		}

		return &dynamodb.GetItemOutput{Item: typed[0]}
	}

	for _, form := range encodeForms {
		for _, payload := range encodePayloads {
			b.Run(fmt.Sprintf("form=%s/payload=%s", form, payload.name), func(b *testing.B) {
				benchEncode(b, out, form, 1, payload.size)
			})
		}
	}
}

func BenchmarkEncodeQuery(b *testing.B) {
	encodeListMatrix(b, func(typed []map[string]dynamodb.AttributeValue, encoded []awsjson.Encoded) awsjson.Appender {
		count := new(int32(max(len(typed), len(encoded))))
		if encoded != nil {
			return &dynamodb.QueryOutput{ItemsJSON: encoded, Count: count, ScannedCount: count}
		}

		return &dynamodb.QueryOutput{Items: typed, Count: count, ScannedCount: count}
	})
}

// BenchmarkEncodeBatchGetItem writes every item under one table.
func BenchmarkEncodeBatchGetItem(b *testing.B) {
	encodeListMatrix(b, func(typed []map[string]dynamodb.AttributeValue, encoded []awsjson.Encoded) awsjson.Appender {
		if encoded != nil {
			return &dynamodb.BatchGetItemOutput{ResponsesJSON: map[string][]awsjson.Encoded{"bench": encoded}}
		}

		return &dynamodb.BatchGetItemOutput{Responses: map[string][]map[string]dynamodb.AttributeValue{"bench": typed}}
	})
}

func BenchmarkEncodeTransactGetItems(b *testing.B) {
	encodeListMatrix(b, func(typed []map[string]dynamodb.AttributeValue, encoded []awsjson.Encoded) awsjson.Appender {
		out := &dynamodb.TransactGetItemsOutput{Responses: make([]dynamodb.ItemResponse, max(len(typed), len(encoded)))}

		for i := range out.Responses {
			if encoded != nil {
				out.Responses[i].ItemJSON = encoded[i]
			} else {
				out.Responses[i].Item = typed[i]
			}
		}

		return out
	})
}

func encodeListMatrix(b *testing.B, out encodeOutput) {
	b.Helper()

	for _, form := range encodeForms {
		for _, n := range encodeItemCounts {
			for _, payload := range encodeListPayloads {
				b.Run(fmt.Sprintf("form=%s/items=%d/payload=%s", form, n, payload.name), func(b *testing.B) {
					benchEncode(b, out, form, n, payload.size)
				})
			}
		}
	}
}

// benchEncode appends the output of n items of the payload's size, in the
// given form, to one reused buffer.
func benchEncode(b *testing.B, out encodeOutput, form string, n, size int) {
	b.Helper()

	typed := make([]map[string]dynamodb.AttributeValue, n)
	for i := range typed {
		typed[i] = map[string]dynamodb.AttributeValue{
			"pk":   {S: new(fmt.Sprintf("item-%04d", i))},
			"data": {S: new(strings.Repeat("x", size))},
		}
	}

	var encoded []awsjson.Encoded

	if form == "encoded" {
		encoded = make([]awsjson.Encoded, n)

		for i, item := range typed {
			raw, err := awsjson.MarshalReflect(item)
			if err != nil {
				b.Fatal(err)
			}

			encoded[i] = raw
		}
	}

	o := out(typed, encoded)

	buf, err := o.AppendJSON(nil)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(buf)))

	for b.Loop() {
		buf, err = o.AppendJSON(buf[:0])
		if err != nil {
			b.Fatal(err)
		}
	}
}
