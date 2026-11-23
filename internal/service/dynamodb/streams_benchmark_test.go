package dynamodb_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// The streamed write matrix (docs/standards.md, "Benchmarks"): PutItem
// through the real gateway on a table whose NEW_AND_OLD_IMAGES stream records
// every write, across the shape of the item and its size. Each put replaces
// the one item with one that differs, so each leaves a MODIFY record holding
// both images, and a long run fills the shard to its byte bound and trims it
// from then on. Sub-benchmark names carry the axes, for example
//
//	BenchmarkPutItemStream/shape=attributes/payload=4KiB
//
// The shapes are one string, one binary, many small number attributes, and a
// list of maps. payload is the item's DynamoDB size, about. The attributes and
// list shapes stop at 64KiB: at 399KiB the attributes item decodes past the
// request budget and is refused, and a put of the list item took 9.8 ms, at
// the 10 ms limit.

// streamShape is a point on the shape axis: the attributes beside the key of
// an item of about size bytes, built from variant so two variants differ, up
// to the payload widest.
type streamShape struct {
	name   string
	widest itemPayload
	attrs  func(size int, variant byte) string
}

// streamPayloads are the payload axis: small, medium, about 64 KiB, and
// DynamoDB's maximum.
var streamPayloads = []itemPayload{itemPayloads[0], itemPayloads[1], stream64KiB, itemPayloads[2]}

// stream64KiB is the widest payload the attributes and list shapes reach.
var stream64KiB = itemPayload{"64KiB", 64 << 10}

// streamAttribute is the payload one small number attribute stands for: its
// name and value take about 11 bytes of DynamoDB size, and the request about
// 25.
const streamAttribute = 16

// streamElement is one element of the list shape: about 64 bytes.
const streamElement = 64

var streamShapes = []streamShape{
	{"string", itemPayloads[2], func(size int, variant byte) string {
		return fmt.Sprintf(`"d":{"S":%q}`, strings.Repeat(string(variant), size))
	}},
	{"binary", itemPayloads[2], func(size int, variant byte) string {
		return fmt.Sprintf(`"d":{"B":%q}`, base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(variant), size))))
	}},
	{"attributes", stream64KiB, func(size int, variant byte) string {
		attrs := make([]string, max(size/streamAttribute, 1))
		for i := range attrs {
			attrs[i] = fmt.Sprintf(`"a%05d":{"N":"%d%05d"}`, i, variant, i)
		}

		return strings.Join(attrs, ",")
	}},
	{"list", stream64KiB, func(size int, variant byte) string {
		elements := make([]string, max(size/streamElement, 1))
		for i := range elements {
			elements[i] = fmt.Sprintf(`{"M":{"s":{"S":"%c%046d"}}}`, variant, i)
		}

		return `"d":{"L":[` + strings.Join(elements, ",") + `]}`
	}},
}

func BenchmarkPutItemStream(b *testing.B) {
	for _, shape := range streamShapes {
		for _, payload := range streamPayloads {
			b.Run(fmt.Sprintf("shape=%s/payload=%s", shape.name, payload.name), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, fmt.Sprintf(streamTable, "NEW_AND_OLD_IMAGES"))
				// The shard's retention timer would hold its records past the
				// cell, and every later cell would run on a larger heap.
				b.Cleanup(dynamodb.Shard(s.provider, usEast1, "streamed").Stop)

				bodies := [2]string{}
				for i, variant := range []byte{'x', 'y'} {
					bodies[i] = fmt.Sprintf(`{"TableName":"streamed","Item":{"id":{"S":"k"},%s}}`,
						shape.attrs(payload.size, variant))
				}

				b.SetBytes(int64(payload.size))
				b.ReportAllocs()

				i := 0
				for b.Loop() {
					got := s.call(b, usEast1, opPut, bodies[i%2])
					require.Equal(b, http.StatusOK, got.status, got.body)

					i++
				}
			})

			if payload == shape.widest {
				break
			}
		}
	}
}
