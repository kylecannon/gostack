package dynamodbstreams_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
)

// FuzzParseIterator: the shard iterator parser never panics, and an iterator
// it accepts writes back as text it reads to the same iterator. The seed
// corpus is in testdata/fuzz/FuzzParseIterator.
func FuzzParseIterator(f *testing.F) {
	f.Add("arn:aws:dynamodb:us-east-1:000000000000:table/t/stream/2024-11-18T14:36:44.149|1|" +
		"AAAAAAAAAAEAAAGTO1VscBgJpOO9e6ZA")
	f.Add("|1|")
	f.Add("a|1|" + "////////////////////////////////")

	f.Fuzz(func(t *testing.T, s string) {
		text, ok := dynamodbstreams.RoundTripIterator(s)
		if !ok {
			return
		}

		again, ok := dynamodbstreams.RoundTripIterator(text)
		if !ok || again != text {
			t.Fatalf("%q parsed to %q, which reads back as %q, %v", s, text, again, ok)
		}
	})
}
