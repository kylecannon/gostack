package query_test

import (
	"net/url"
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/query"
)

const (
	batchEntryList = "SendMessageBatchRequestEntry"
	batchAttribute = "MessageAttribute"
)

// batchEntries is the list spec the generated SendMessageBatch decoder uses.
var batchEntries = query.ListSpec{
	Names:     []string{batchEntryList, "Entries"},
	Member:    batchEntryList,
	Flattened: true,
	Complex:   true,
}

// batchAttributes is the spec of an entry's MessageAttribute map.
var batchAttributes = query.MapSpec{
	Names:        []string{batchAttribute, "MessageAttributes"},
	Key:          nameMember,
	Value:        valueMember,
	Flattened:    true,
	ComplexValue: true,
}

// batchForm is the form of a SendMessageBatch of n entries, each with a body,
// a delay and one message attribute, the way the SDKs spell it.
func batchForm(n int) url.Values {
	vals := url.Values{"QueueUrl": {"http://localhost:4566/000000000000/q"}}

	for i := 1; i <= n; i++ {
		prefix := batchEntryList + "." + strconv.Itoa(i) + "."
		vals.Set(prefix+"Id", "id-"+strconv.Itoa(i))
		vals.Set(prefix+"MessageBody", "body")
		vals.Set(prefix+"DelaySeconds", "0")
		vals.Set(prefix+batchAttribute+".1.Name", "trace")
		vals.Set(prefix+batchAttribute+".1.Value.DataType", "String")
		vals.Set(prefix+batchAttribute+".1.Value.StringValue", "0123456789abcdef")
	}

	return vals
}

// decodeBatch reads a batch form the way the generated SendMessageBatch decoder
// does and returns the number of entries and the number of attributes.
func decodeBatch(vals url.Values) (entries, attributes int) {
	d := query.NewDecoder(vals)
	_ = d.String("QueueUrl")

	for _, k := range d.List("", &batchEntries) {
		_ = d.String(query.Key(k, "Id"))
		_ = d.String(query.Key(k, "MessageBody"))
		_ = d.Int32(query.Key(k, "DelaySeconds"))

		for _, ent := range d.Map(k, &batchAttributes) {
			_ = d.String(ent.Key)
			_ = d.Has(ent.Value)
			_ = d.String(query.Key(ent.Value, "DataType"))
			_ = d.String(query.Key(ent.Value, "StringValue"))
			attributes++
		}

		entries++
	}

	return entries, attributes
}

// BenchmarkDecodeBatch decodes SendMessageBatch forms of n entries. It checks
// that the decode stays linear in the entries: the Decoder looks up a key below
// another once per entry.
func BenchmarkDecodeBatch(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		vals := batchForm(n)

		b.Run("entries="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				if entries, _ := decodeBatch(vals); entries != n {
					b.Fatal("decoded", entries, "entries, want", n)
				}
			}
		})
	}
}
