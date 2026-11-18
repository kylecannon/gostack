package sqs

import (
	"crypto/md5"
	"encoding/hex"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
)

const typeStringName = "String"

func Test_attributesMD5(t *testing.T) {
	trace := "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1"
	binary := []byte("traceparent\x1e00-774062d6c37081a5a0b9b5b88e30627c-2d2482211f6489da-01")

	tests := []struct {
		name string
		give map[string]sqsapi.MessageAttributeValue
		// want is a vector from LocalStack's snapshot files.
		want string
	}{
		{
			name: "one string attribute",
			give: map[string]sqsapi.MessageAttributeValue{"k": {DataType: new(typeStringName), StringValue: new("x")}},
			want: "a45daa70926828a2f0a1db3418e6feb4",
		},
		{
			name: "three string attributes, hashed in name order",
			give: map[string]sqsapi.MessageAttributeValue{
				"Help.Me": {DataType: new(typeStringName), StringValue: new("Me")},
				"Hello":   {DataType: new(typeStringName), StringValue: new("There")},
				"General": {DataType: new(typeStringName), StringValue: new("Kenobi")},
			},
			want: "4c360f3fdafd970e05fae2f149d997f5",
		},
		{
			name: "a binary attribute",
			give: map[string]sqsapi.MessageAttributeValue{"attr1": {DataType: new("Binary"), BinaryValue: binary}},
			want: "8cbe4db156db8a94db8b801b7addb984",
		},
		{
			name: "a system attribute",
			give: map[string]sqsapi.MessageAttributeValue{
				"AWSTraceHeader": {DataType: new(typeStringName), StringValue: &trace},
			},
			want: "5ae4d5d7636402d80f4eb6d213245a88",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, attributesMD5(tt.give))
		})
	}
}

// Test_attributesMD5_layout builds the bytes by hand: a four-byte big-endian
// length before the name, the data type and the value, and a transport byte of
// 1 for a string and 2 for binary in front of the value.
func Test_attributesMD5_layout(t *testing.T) {
	attrs := map[string]sqsapi.MessageAttributeValue{
		"b": {DataType: new("Binary"), BinaryValue: []byte{0xff, 0x00}},
		"a": {DataType: new("Number"), StringValue: new("12")},
	}

	want := []byte{
		0, 0, 0, 1, 'a', 0, 0, 0, 6, 'N', 'u', 'm', 'b', 'e', 'r', 1, 0, 0, 0, 2, '1', '2',
		0, 0, 0, 1, 'b', 0, 0, 0, 6, 'B', 'i', 'n', 'a', 'r', 'y', 2, 0, 0, 0, 2, 0xff, 0x00,
	}
	sum := md5.Sum(want)

	assert.Equal(t, hex.EncodeToString(sum[:]), attributesMD5(attrs))
}

func Test_attributesMD5_of_nothing(t *testing.T) {
	// MD5 of the empty input, which is what a map with no attributes hashes to.
	assert.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", attributesMD5(map[string]sqsapi.MessageAttributeValue{}))
}

func Test_entryHeap(t *testing.T) {
	h := entryHeap{
		less:  func(a, b *entry) bool { return a.seq < b.seq },
		index: func(e *entry) *int { return &e.visibleAt },
	}

	entries := make([]*entry, 0, 20)
	for _, seq := range []uint64{9, 3, 14, 1, 7, 12, 5, 18, 2, 11, 16, 4, 8, 13, 6, 19, 10, 15, 17, 20} {
		e := &entry{seq: seq, visibleAt: -1}
		entries = append(entries, e)
		h.push(e)
	}

	// Remove from the middle and change a key, as a deleted message and a
	// changed visibility timeout do.
	h.remove(entries[4])
	h.remove(entries[0])
	entries[2].seq = 100
	h.fix(entries[2])

	var got []uint64

	for e := h.pop(); e != nil; e = h.pop() {
		got = append(got, e.seq)
	}

	assert.Equal(t, []uint64{1, 2, 3, 4, 5, 6, 8, 10, 11, 12, 13, 15, 16, 17, 18, 19, 20, 100}, got)
	assert.Zero(t, h.len())
	assert.Equal(t, -1, entries[4].visibleAt, "an entry that left knows it")
}
