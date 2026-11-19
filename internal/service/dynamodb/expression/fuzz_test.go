package expression_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
)

// fuzzSeeds are expressions from the tests and from the AWS documentation's
// condition and filter examples; testdata/fuzz holds more.
//
//nolint:lll,goconst // seed expressions, several shared with the table tests
var fuzzSeeds = []string{
	"attribute_not_exists(Price)", "attribute_exists(#pr.FiveStar)",
	"attribute_type(ProductReviews.FiveStar, :v_sub)", "begins_with(Pictures.FrontView, :v_sub)",
	"contains(Color, :v_sub)", "size(Brand) <= :v_sub", "size(VideoClip) > :v_sub",
	"Price BETWEEN :lo AND :hi", "ProductCategory IN (:cat1, :cat2)", "(#P between :lo and :hi) and (#PC in (:cat1, :cat2))",
	"#pr.OneStar = :n AND NOT a <> :v OR m.c[1] >= :n", "((a = :v))", "a = = :v", "a[2147483648] = :v",
	"a IN ()", "size()", "NOT", "a.b[1].c, d", "(a) = (:v)", "Id = :id AND Price > :p",
}

// fuzzSubs gives every placeholder the fuzz seeds use a value.
func fuzzSubs() expression.Substitutions {
	subs := autoSubs(":v :n :s :ss :m :l :b :hi :lo")
	for _, k := range []string{":v_sub", ":cat1", ":cat2", ":id", ":p"} {
		subs.Values[k] = str("x")
	}

	subs.Names = map[string]string{"#pr": "ProductReviews", "#P": "Price", "#PC": "ProductCategory", "#a": "a"}

	return subs
}

// requireValidation fails t unless err is nil or a ValidationException: a
// parser refuses input, it never fails another way.
func requireValidation(t *testing.T, src string, err error) {
	t.Helper()

	if err == nil {
		return
	}

	aerr, ok := errors.AsType[*awserr.Error](err)
	if !ok || aerr.Code != "ValidationException" || aerr.StatusCode != http.StatusBadRequest {
		t.Fatalf("%q: error %v is not a ValidationException", src, err)
	}
}

// FuzzParseCondition holds the condition parser and evaluator to their
// contract on any text: no panic, no hang, a ValidationException or a
// condition that evaluates against an item and against no item.
func FuzzParseCondition(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}

	subs := fuzzSubs()
	item := richItem()

	f.Fuzz(func(t *testing.T, src string) {
		c, err := expression.ParseCondition(expression.ConditionExpression, src, subs, map[string]struct{}{})
		requireValidation(t, src, err)

		if err != nil {
			return
		}

		_, err = c.Eval(item)
		if err != nil {
			t.Fatalf("%q: Eval: %v", src, err)
		}

		_, err = c.Eval(nil)
		if err != nil {
			t.Fatalf("%q: Eval(nil): %v", src, err)
		}
	})
}

// FuzzParseKeyCondition holds the key condition parser to its contract: a
// ValidationException, or a hash condition and an optional range condition
// with the operand count the engine reads.
func FuzzParseKeyCondition(f *testing.F) {
	keySeeds := []string{keyBetween, "pk = :p AND begins_with(sk, :s)", ":p = pk AND :n < sk"}
	for _, s := range append(fuzzSeeds, keySeeds...) {
		f.Add(s)
	}

	subs := fuzzSubs()
	subs.Values[":p"] = str("x")

	f.Fuzz(func(t *testing.T, src string) {
		kc, err := expression.ParseKeyCondition(src, probeKey, subs, map[string]struct{}{})
		requireValidation(t, src, err)

		if err != nil {
			return
		}

		if name, _ := kc.Hash(); name != "pk" {
			t.Fatalf("%q: hash %q", src, name)
		}

		if _, cond, ok := kc.Range(); ok && len(cond.Values) == 0 {
			t.Fatalf("%q: range condition without values", src)
		}
	})
}

// FuzzParseProjection holds the projection parser to its contract: a
// ValidationException, or paths that Apply can take from any item.
func FuzzParseProjection(f *testing.F) {
	for _, s := range append(fuzzSeeds, "a, pk, b.c[0]", "l[2].c, l[0]", "#pr.FiveStar, #a[3]") {
		f.Add(s)
	}

	subs := fuzzSubs()
	item := richItem()

	f.Fuzz(func(t *testing.T, src string) {
		pr, err := expression.ParseProjection(src, subs, map[string]struct{}{})
		requireValidation(t, src, err)

		if err != nil {
			return
		}

		if len(pr.Paths()) == 0 {
			t.Fatalf("%q: no paths", src)
		}

		pr.Apply(item)
	})
}
