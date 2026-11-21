package expression_test

import (
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// benchTerms are the expression sizes the benchmarks run: 1, 10 and 100 terms.
var benchTerms = []int{1, 10, 100}

// benchCondition builds a filter of terms comparisons joined by AND, each on
// its own attribute, with the values it needs.
func benchCondition(terms int) (string, expression.Substitutions) {
	parts := make([]string, terms)
	subs := expression.Substitutions{Values: map[string]api.AttributeValue{}}

	for i := range parts {
		k := ":v" + strconv.Itoa(i)
		parts[i] = "a" + strconv.Itoa(i) + " >= " + k
		subs.Values[k] = num("0")
	}

	return strings.Join(parts, " AND "), subs
}

// benchItem builds an item of attrs number attributes a0..a<attrs-1>, each 1,
// and a padding string that brings it to about size bytes.
func benchItem(attrs, size int) value.Item {
	item := value.Item{}
	for i := range attrs {
		item["a"+strconv.Itoa(i)] = num("1")
	}

	item["pad"] = str(strings.Repeat("x", max(0, size-value.ItemSize(item))))

	return item
}

func BenchmarkParseCondition(b *testing.B) {
	for _, terms := range benchTerms {
		src, subs := benchCondition(terms)

		b.Run("terms="+strconv.Itoa(terms), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))

			for b.Loop() {
				_, err := expression.ParseCondition(expression.FilterExpression, src, subs, map[string]struct{}{})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkEvalCondition(b *testing.B) {
	for _, terms := range benchTerms {
		src, subs := benchCondition(terms)

		c, err := expression.ParseCondition(expression.FilterExpression, src, subs, map[string]struct{}{})
		if err != nil {
			b.Fatal(err)
		}

		for _, size := range []int{1 << 10, 64 << 10, value.MaxItemSize} {
			item := benchItem(terms, size)

			b.Run("terms="+strconv.Itoa(terms)+"/item="+strconv.Itoa(size>>10)+"KiB", func(b *testing.B) {
				b.ReportAllocs()

				for b.Loop() {
					if ok, _ := c.Eval(item); !ok {
						b.Fatal("condition false")
					}
				}
			})
		}
	}
}

func BenchmarkParseKeyCondition(b *testing.B) {
	src := keyBetween
	subs := autoSubs(src)

	b.ReportAllocs()

	for b.Loop() {
		_, err := expression.ParseKeyCondition(src, probeKey, subs, map[string]struct{}{})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseUpdate measures ParseUpdate by the number of paths a REMOVE
// names, up to the 700 or so that fit the 4 KiB expression cap; the overlap
// check is the part that grows with it.
func BenchmarkParseUpdate(b *testing.B) {
	for _, terms := range []int{1, 10, 100, 700} {
		paths := make([]string, terms)
		for i := range paths {
			paths[i] = "a" + strconv.Itoa(i)
		}

		src := "REMOVE " + strings.Join(paths, ", ")

		b.Run("terms="+strconv.Itoa(terms), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))

			for b.Loop() {
				_, err := expression.ParseUpdate(src, expression.Substitutions{}, nil)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkProjection(b *testing.B) {
	for _, terms := range benchTerms {
		paths := make([]string, terms)
		for i := range paths {
			paths[i] = "a" + strconv.Itoa(i)
		}

		src := strings.Join(paths, ", ")

		b.Run("parse/terms="+strconv.Itoa(terms), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_, err := expression.ParseProjection(src, expression.Substitutions{}, map[string]struct{}{})
				if err != nil {
					b.Fatal(err)
				}
			}
		})

		pr, err := expression.ParseProjection(src, expression.Substitutions{}, map[string]struct{}{})
		if err != nil {
			b.Fatal(err)
		}

		item := benchItem(terms, 64<<10)

		b.Run("apply/terms="+strconv.Itoa(terms)+"/item=64KiB", func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				pr.Apply(item)
			}
		})
	}
}
