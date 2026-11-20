package expression_test

import (
	"errors"
	"regexp"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

func str(s string) api.AttributeValue   { return api.AttributeValue{S: &s} }
func num(s string) api.AttributeValue   { return api.AttributeValue{N: &s} }
func boolean(b bool) api.AttributeValue { return api.AttributeValue{BOOL: &b} }

func null() api.AttributeValue {
	t := true

	return api.AttributeValue{NULL: &t}
}

// pool holds the values the tests' :placeholders stand for; the probes of the
// LocalStack image used the same ones.
var pool = map[string]api.AttributeValue{
	":p": str("x"), ":n": num("1"), ":s": str("a"), ":b": boolean(true),
	":hi": num("9"), ":lo": num("1"), ":ss": {SS: []string{"a"}}, ":ns": {NS: []string{"1"}},
	":m": {M: map[string]api.AttributeValue{}}, ":l": {L: []api.AttributeValue{}}, ":nul": null(),
	":bin": {B: []byte("a")}, ":v": str("v"), ":w": str("w"), ":tS": str("S"), ":tXX": str("XX"),
	":tN": str("N"), ":n5x": num("5.0"), ":n0": num("0"), ":n1": num("1"), ":n2": num("2"),
	":n3": num("3"), ":n9": num("9"), ":s2": str("bc"), ":sab": str("ab"), ":sabc": str("abc"),
	":sz": str("z"), ":a": str("x"), ":h": str("x"), ":zz": str("x"),
	":binb": {B: []byte("b")}, ":binab": {B: []byte("ab")},
	":mz":    {M: map[string]api.AttributeValue{"b": str("z")}},
	":ssba":  {SS: []string{"b", "a"}},
	":lcopy": {L: []api.AttributeValue{str("a"), num("1"), {M: map[string]api.AttributeValue{"b": str("z")}}}},
}

var placeholder = regexp.MustCompile(`[:#][A-Za-z0-9_]+`)

// autoSubs returns substitutions for every placeholder src uses: each #name
// stands for the name without its #, each :value for its pool value.
func autoSubs(src string) expression.Substitutions {
	subs := expression.Substitutions{}

	for _, k := range placeholder.FindAllString(src, -1) {
		if k[0] == '#' {
			if subs.Names == nil {
				subs.Names = map[string]string{}
			}

			subs.Names[k] = k[1:]

			continue
		}

		if v, ok := pool[k]; ok {
			if subs.Values == nil {
				subs.Values = map[string]api.AttributeValue{}
			}

			subs.Values[k] = v
		}
	}

	return subs
}

// richItem is the item the evaluation tests read, as the probes stored it.
func richItem() value.Item {
	return value.Item{
		"pk": str("x"), "sk": num("1"), "a": str("abc"), "n": num("5"),
		"ss": {SS: []string{"a", "b"}}, "ns": {NS: []string{"1", "2"}},
		"l":   {L: []api.AttributeValue{str("a"), num("1"), {M: map[string]api.AttributeValue{"b": str("z")}}}},
		"m":   {M: map[string]api.AttributeValue{"b": str("z"), "c": {L: []api.AttributeValue{num("1"), num("2")}}}},
		"bin": {B: []byte("abc")}, "t": boolean(true), "nul": null(), "u": str("é€"),
	}
}

// wantMessage fails t unless err is the ValidationException with msg, or nil
// when msg is empty.
func wantMessage(t *testing.T, err error, msg string) {
	t.Helper()

	if msg == "" {
		if err != nil {
			t.Fatalf("error %v, want none", err)
		}

		return
	}

	aerr, ok := errors.AsType[*awserr.Error](err)
	if !ok {
		t.Fatalf("error %v, want ValidationException %q", err, msg)
	}

	if aerr.Code != "ValidationException" || aerr.Message != msg {
		t.Fatalf("error %s %q\nwant ValidationException %q", aerr.Code, aerr.Message, msg)
	}
}
