package filter_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
)

// The matrix: policy size by top-level keys (1, 5) and value combinations
// (1, 150), message size for the body scope (small, 64 KiB, and SNS's 256 KiB
// maximum), and String against String.Array for the attribute scope. Every
// message matches on the last value of every key, so the matcher does the
// whole policy's work.

// benchKeys are the value counts per key whose product is the combination
// count, for each policy shape.
var benchKeys = []struct {
	name   string
	counts []int
}{
	{"keys=1/combinations=1", []int{1}},
	{"keys=1/combinations=150", []int{150}},
	{"keys=5/combinations=1", []int{1, 1, 1, 1, 1}},
	{"keys=5/combinations=150", []int{5, 5, 3, 2, 1}},
}

var benchPayloads = []struct {
	name string
	size int
}{
	{"payload=512B", 512},
	{"payload=64KiB", 64 << 10},
	{"payload=256KiB", 256 << 10},
}

// benchPolicySource is a policy with one key per count, each listing that
// many values.
func benchPolicySource(b *testing.B, counts []int) string {
	b.Helper()
	policy := map[string][]string{}
	for k, n := range counts {
		key := fmt.Sprintf("key%d", k)
		for v := range n {
			policy[key] = append(policy[key], fmt.Sprintf("value-%d", v))
		}
	}
	src, err := json.Marshal(policy)
	if err != nil {
		b.Fatal(err)
	}
	return string(src)
}

func benchPolicy(b *testing.B, counts []int, scope filter.Scope) *filter.Policy {
	b.Helper()
	p, err := filter.Parse(benchPolicySource(b, counts), scope)
	if err != nil {
		b.Fatal(err)
	}
	return p
}

// benchBody is a JSON object of about size bytes: the policy's keys with their
// last value, then records of filler the matcher has to step over.
func benchBody(counts []int, size int) string {
	var sb strings.Builder
	sb.Grow(size + 256)
	sb.WriteByte('{')
	for k, n := range counts {
		fmt.Fprintf(&sb, `"key%d":"value-%d",`, k, n-1)
	}
	sb.WriteString(`"records":[`)
	for i := 0; sb.Len() < size-64; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":%d,"name":"record-%d","tags":["a","b"],"nested":{"key0":"other"}}`, i, i)
	}
	sb.WriteString("]}")
	return sb.String()
}

func BenchmarkMatchBody(b *testing.B) {
	for _, shape := range benchKeys {
		p := benchPolicy(b, shape.counts, filter.ScopeMessageBody)
		for _, payload := range benchPayloads {
			body := filter.ParseBody(benchBody(shape.counts, payload.size))
			if !p.MatchBody(body) {
				b.Fatal("benchmark message does not match")
			}
			b.Run(shape.name+"/"+payload.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					p.MatchBody(body)
				}
			})
		}
	}
}

func BenchmarkParseBody(b *testing.B) {
	for _, payload := range benchPayloads {
		body := benchBody([]int{1}, payload.size)
		b.Run(payload.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				filter.ParseBody(body)
			}
		})
	}
}

func BenchmarkMatchAttributes(b *testing.B) {
	for _, shape := range benchKeys {
		p := benchPolicy(b, shape.counts, filter.ScopeMessageAttributes)
		for _, kind := range []string{typeString, typeStringArray} {
			// SNS allows 10 attributes; the rest are ones the policy ignores.
			attrs := map[string]filter.Attribute{}
			for k, n := range shape.counts {
				v := fmt.Sprintf("value-%d", n-1)
				if kind == typeStringArray {
					v = fmt.Sprintf(`["other-a", "other-b", %q]`, v)
				}
				attrs[fmt.Sprintf("key%d", k)] = filter.Attribute{DataType: kind, StringValue: v}
			}
			for i := len(attrs); i < 10; i++ {
				attrs[fmt.Sprintf("extra%d", i)] = filter.Attribute{DataType: typeString, StringValue: "x"}
			}
			if !p.MatchAttributes(attrs) {
				b.Fatal("benchmark attributes do not match")
			}
			b.Run(shape.name+"/attr="+kind, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					p.MatchAttributes(attrs)
				}
			})
		}
	}
}

func BenchmarkParse(b *testing.B) {
	for _, shape := range benchKeys {
		src := benchPolicySource(b, shape.counts)
		b.Run(shape.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, err := filter.Parse(src, filter.ScopeMessageBody)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
