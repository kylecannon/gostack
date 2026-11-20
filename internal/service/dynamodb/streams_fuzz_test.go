package dynamodb_test

import (
	"fmt"
	"testing"
)

// FuzzStreamByARN: the stream ARN parser never panics, and an ARN it finds a
// stream for is that stream's ARN exactly. The seed corpus is in
// testdata/fuzz/FuzzStreamByARN.
func FuzzStreamByARN(f *testing.F) {
	s := newStack(f)
	s.ok(f, "CreateTable", fmt.Sprintf(streamTable, "KEYS_ONLY"))
	arn := s.latestStreamARN(f, "streamed")

	f.Add(arn)
	f.Add("arn:aws:dynamodb:us-east-1:000000000000:table/streamed/stream/")
	f.Add(":table//stream/")

	f.Fuzz(func(t *testing.T, in string) {
		info, ok := s.provider.StreamByARN(streamCtx(t), in)
		if ok && info.ARN != in {
			t.Fatalf("%q found the stream %q", in, info.ARN)
		}
	})
}
