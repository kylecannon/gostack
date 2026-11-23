package stream_test

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// liveHeap is the bytes of heap objects still reachable: two collections
// first, so what the last one left unswept is swept.
func liveHeap() int64 {
	runtime.GC() //nolint:revive // the measurement is of the heap a collection leaves
	runtime.GC() //nolint:revive // and of what it swept

	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)

	return int64(sample[0].Value.Uint64())
}

// chargeShape is a kind of record: count records whose images have attrs
// attributes besides the key, attribute j of record i the json attr(i, j)
// writes, and an old image beside the new one when old is set.
type chargeShape struct {
	name  string
	count int
	attrs int
	old   bool
	attr  func(i, j int) string
}

// chargeShapes span what a record can pin: scalars, nesting, one large
// binary, many small attributes, and a key alone. Every string differs from
// record to record, so no two records share what the decoder interns.
var chargeShapes = []chargeShape{
	{"scalars", 2000, 12, true, func(i, j int) string {
		switch j % 4 {
		case 0:
			return fmt.Sprintf(`"s%d":{"S":"%09d-%s"}`, j, i, strings.Repeat("s", 22))
		case 1:
			return fmt.Sprintf(`"n%d":{"N":"%d.%d"}`, j, i, j)
		case 2:
			return fmt.Sprintf(`"b%d":{"BOOL":true}`, j)
		default:
			return fmt.Sprintf(`"z%d":{"NULL":true}`, j)
		}
	}},
	{"nested", 1000, 3, true, func(i, j int) string {
		return fmt.Sprintf(`"m%d":{"M":{"list":{"L":[{"S":"a%d"},{"N":"%d"},{"M":{"deep":{"S":"d%d"}}},`+
			`{"L":[{"BOOL":false},{"S":"e%d"}]},{"SS":["x%d","y%d"]}]},"inner":{"M":{"k%d":{"N":"%d"},"t":{"S":"t%d"}}}}}`,
			j, i, i, i, i, i, i, i, i, i)
	}},
	{"binary", 100, 1, false, func(i, j int) string {
		blob := []byte(fmt.Sprintf("%09d", i) + strings.Repeat("b", 256<<10))

		return fmt.Sprintf(`"blob%d":{"B":%q}`, j, base64.StdEncoding.EncodeToString(blob))
	}},
	{"small attributes", 200, 200, true, func(i, j int) string {
		return fmt.Sprintf(`"a%d":{"N":"%d"}`, j, i*1000+j)
	}},
	{"keys only", 4000, 0, false, nil},
}

// image decodes the item of record i of shape as PutItem's body brings it in.
func (c chargeShape) image(tb testing.TB, i int) value.Item {
	tb.Helper()

	attrs := make([]string, 0, 1+c.attrs)
	attrs = append(attrs, fmt.Sprintf(`"id":{"S":"key-%09d"}`, i))
	for j := range c.attrs {
		attrs = append(attrs, c.attr(i, j))
	}

	in, err := awsjson.Decode[api.PutItemInput]([]byte(`{"TableName":"t","Item":{` + strings.Join(attrs, ",") + `}}`))
	if err != nil {
		tb.Fatal(err)
	}

	return in.Item
}

// Test_charge_coversHeap: a shard holding records of each shape takes at most
// what Charge counts for them, measured as the live heap before and after
// appending them, and Charge counts at most one and a half times that. The
// images are decoded per record, unshared, so the heap they take is the
// records' alone.
func Test_charge_coversHeap(t *testing.T) {
	for _, shape := range chargeShapes {
		t.Run(shape.name, func(t *testing.T) {
			now := time.Now()
			shard := stream.New(now, func() time.Time { return now })
			shard.SetLimits(stream.Retention, 1<<62)
			defer shard.Stop()

			// Each record's images are decoded as it is appended, so the shard
			// is all that holds them, and what decoding left besides is garbage
			// the measurement collects.
			before := liveHeap()

			for i := range shape.count {
				newImage := shape.image(t, i)
				r := stream.Record{Event: stream.Modify, Keys: value.Item{"id": newImage["id"]}, New: newImage, At: now}

				if shape.old {
					r.Old = shape.image(t, shape.count+i)
				}

				shard.Append(r)
			}

			after := liveHeap()

			var charged int64

			records := shard.Records()
			for i := range records {
				charged += stream.Charge(&records[i])
			}

			measured := after - before

			t.Logf("%s: %d records, measured %d bytes, charged %d (%.2fx)",
				shape.name, shape.count, measured, charged, float64(charged)/float64(measured))
			require.GreaterOrEqual(t, charged, measured, "the charge covers the heap the records take")
			require.LessOrEqual(t, float64(charged), 1.5*float64(measured), "the charge is within 1.5x the heap")
		})
	}
}
