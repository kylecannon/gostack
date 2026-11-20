package expression_test

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// keyBetween reads a range of the probe table's range key.
const keyBetween = "pk = :p AND sk BETWEEN :lo AND :hi"

// probeKey is the key schema of the table the probes queried: hash pk (S),
// range sk (N).
var probeKey = table.KeySchema{
	Hash:  table.KeyAttribute{Name: "pk", Type: value.TypeS},
	Range: table.KeyAttribute{Name: "sk", Type: value.TypeN},
}

//nolint:ireturn,wrapcheck // returns what the parser under test returns
func parseKey(src string, subs *expression.Substitutions) (expression.KeyCondition, error) {
	s := autoSubs(src)
	if subs != nil {
		s = *subs
	}

	used := map[string]struct{}{}

	kc, err := expression.ParseKeyCondition(src, probeKey, s, used)
	if err != nil {
		return nil, err
	}

	return kc, expression.CheckUnused(s, used)
}

// TestParseKeyConditionErrors holds the key condition checks to the code and
// message the LocalStack image answered for each Query.
//
//nolint:lll,goconst // each row holds the exact text the LocalStack image answered; naming or splitting it hides what was probed
func TestParseKeyConditionErrors(t *testing.T) {
	t.Parallel()

	const prefix = "Invalid KeyConditionExpression: "

	tests := []struct {
		name string
		src  string
		subs *expression.Substitutions
		want string
	}{
		{"hash lt", "pk < :p", nil, "Query key condition not supported"},
		{"hash between", "pk BETWEEN :p AND :p", nil, "Query key condition not supported"},
		{"hash begins", "begins_with(pk, :p)", nil, "Query key condition not supported"},
		{"missing hash", "sk = :n", nil, "Query condition missed key schema element"},
		{"range only", "sk > :n", nil, "Query condition missed key schema element"},
		{"other attribute", "pk = :p AND zz = :n", nil, "Query condition missed key schema element"},
		{"other attribute first", "zz = :n AND pk = :p", nil, "Query condition missed key schema element"},
		{"other attribute range", "pk = :p AND zz > :n", nil, "Query condition missed key schema element"},
		{"names not key", "#pk = :p", &expression.Substitutions{Names: map[string]string{"#pk": "npk"}, Values: values(":p")}, "Query condition missed key schema element"},
		{"missing before type", "sk = :s", nil, "Query condition missed key schema element"},
		{"hash twice", "pk = :p AND pk = :p", nil, "KeyConditionExpressions must only contain one condition per key"},
		{"range twice", "pk = :p AND sk = :n AND sk > :n", nil, "KeyConditionExpressions must only contain one condition per key"},
		{"twice before missed", "pk = :p AND pk = :p AND zz = :n", nil, "KeyConditionExpressions must only contain one condition per key"},
		{"or", "pk = :p OR sk = :n", nil, "Invalid operator used in KeyConditionExpression: OR"},
		{"or first", "zz = :n OR pk = :p", nil, "Invalid operator used in KeyConditionExpression: OR"},
		{"ne", "pk = :p AND sk <> :n", nil, "Invalid operator used in KeyConditionExpression: <>"},
		{"ne before missed", "sk <> :n", nil, "Invalid operator used in KeyConditionExpression: <>"},
		{"not", "pk = :p AND NOT sk = :n", nil, "Invalid operator used in KeyConditionExpression: NOT"},
		{"exists", "pk = :p AND attribute_exists(sk)", nil, "Invalid operator used in KeyConditionExpression: attribute_exists"},
		{"in", "pk IN (:p)", nil, "Invalid operator used in KeyConditionExpression: IN"},
		{"contains", "contains(pk, :p)", nil, "Invalid operator used in KeyConditionExpression: contains"},
		{"size", "pk = :p AND size(sk) = :n", nil, "KeyConditionExpressions cannot contain nested operations"},
		{"nested hash", "pk.x = :p", nil, "Key attributes must be scalars; list random access '[]' and map lookup '.' are not allowed: Key: pk"},
		{"nested range", "pk = :p AND sk[0] = :n", nil, "Key attributes must be scalars; list random access '[]' and map lookup '.' are not allowed: Key: sk"},
		{"constants", ":p = :p", nil, "Invalid condition in KeyConditionExpression: No key attribute specified"},
		{"two keys", "pk = sk", nil, "Invalid condition in KeyConditionExpression: Multiple attribute names used in one condition"},
		{"key and path", "pk = :p AND sk = zz", nil, "Invalid condition in KeyConditionExpression: Multiple attribute names used in one condition"},
		{"between reversed", "pk = :p AND :n BETWEEN sk AND :n", nil, "Invalid condition in KeyConditionExpression: BETWEEN operator must have the key attribute as its first operand"},
		{"hash type", "pk = :n", nil, "One or more parameter values were invalid: Condition parameter type does not match schema type"},
		{"range type", "pk = :p AND sk = :s", nil, "One or more parameter values were invalid: Condition parameter type does not match schema type"},
		{"hash list", "pk = :l", nil, "One or more parameter values were invalid: Condition parameter type does not match schema type"},
		{"begins string on number", "pk = :p AND begins_with(sk, :s)", nil, "One or more parameter values were invalid: Condition parameter type does not match schema type"},
		{"between type", "pk = :p AND sk BETWEEN :s AND :s", nil, "One or more parameter values were invalid: Condition parameter type does not match schema type"},
		{"type before unsupported", "pk < :n", nil, "One or more parameter values were invalid: Condition parameter type does not match schema type"},
		{"begins number", "pk = :p AND begins_with(sk, :n)", nil, prefix + "Incorrect operand type for operator or function; operator or function: begins_with, operand type: N"},
		{"begins reversed number", "pk = :p AND begins_with(:n, sk)", nil, prefix + "Incorrect operand type for operator or function; operator or function: begins_with, operand type: N"},
		{"between bounds", "pk = :p AND sk BETWEEN :hi AND :lo", nil, prefix + "The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: {N:9}, upper bound operand: AttributeValue: {N:1}"},
		{"reserved", "pk = :p AND name = :n", nil, prefix + "Attribute name is a reserved keyword; reserved keyword: name"},
		{"reserved other", "pk = :p AND other = :n", nil, prefix + "Attribute name is a reserved keyword; reserved keyword: other"},
		{"redundant", "((pk = :p))", nil, prefix + "The expression has redundant parentheses;"},
		{"syntax", "pk = ", nil, prefix + `Syntax error; token: "<EOF>", near: "= "`},
		{"undefined", "pk = :zz", &expression.Substitutions{Values: values(":p")}, prefix + "An expression attribute value used in expression is not defined; attribute value: :zz"},
		{"too long", "pk = :p AND sk = :n" + spaces(4100), nil, prefix + "Expression size has exceeded the maximum allowed size; expression size: 4119"},
		{"operators", "pk = :p AND " + join(150, "sk = :n", " AND "), nil, prefix + "The expression contains too many operators; operator count: 301"},
		{"unused name", "#k = :p", &expression.Substitutions{Names: map[string]string{"#k": "pk", "#z": "z"}, Values: values(":p")}, "Value provided in ExpressionAttributeNames unused in expressions: keys: {#z}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseKey(tt.src, tt.subs)
			wantMessage(t, err, tt.want)
		})
	}
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}

	return string(b)
}

// TestParseKeyCondition checks the accepted forms compile to the range read
// the table engine takes.
func TestParseKeyCondition(t *testing.T) {
	t.Parallel()

	n := func(s string) []api.AttributeValue { return []api.AttributeValue{num(s)} }

	tests := []struct {
		src      string
		wantOp   table.SortOp
		wantVals []api.AttributeValue
		hasRange bool
	}{
		{"pk = :p", table.SortAny, nil, false},
		{":p = pk", table.SortAny, nil, false},
		{"(pk = :p) AND (sk = :n)", table.SortEQ, n("1"), true},
		{"(pk = :p AND sk = :n)", table.SortEQ, n("1"), true},
		{"sk > :n AND pk = :p", table.SortGT, n("1"), true},
		{"pk = :p AND sk < :n", table.SortLT, n("1"), true},
		{"pk = :p AND sk <= :n", table.SortLE, n("1"), true},
		{"pk = :p AND sk >= :n", table.SortGE, n("1"), true},
		{"pk = :p AND :n < sk", table.SortGT, n("1"), true},
		{"pk = :p AND :n >= sk", table.SortLE, n("1"), true},
		{keyBetween, table.SortBetween, []api.AttributeValue{num("1"), num("9")}, true},
		{"pk = :p AND sk = :n5x", table.SortEQ, n("5"), true},
		{"#k = :p AND #s > :n", table.SortGT, n("1"), true},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			t.Parallel()

			subs := autoSubs(tt.src)
			if subs.Names != nil {
				subs.Names = map[string]string{"#k": "pk", "#s": "sk"}
			}

			used := map[string]struct{}{}

			kc, err := expression.ParseKeyCondition(tt.src, probeKey, subs, used)
			if err != nil {
				t.Fatal(err)
			}

			name, v := kc.Hash()
			if name != "pk" || v.S == nil || *v.S != "x" {
				t.Fatalf("Hash() = %s %v", name, v)
			}

			rname, cond, ok := kc.Range()
			if ok != tt.hasRange || !ok {
				if ok != tt.hasRange {
					t.Fatalf("Range() ok = %v", ok)
				}

				return
			}

			if rname != "sk" || cond.Op != tt.wantOp || len(cond.Values) != len(tt.wantVals) {
				t.Fatalf("Range() = %s %+v", rname, cond)
			}

			for i := range cond.Values {
				if !value.Equal(&cond.Values[i], &tt.wantVals[i]) {
					t.Fatalf("Range() value %d = %v, want %v", i, *cond.Values[i].N, *tt.wantVals[i].N)
				}
			}
		})
	}
}

// TestParseKeyConditionBeginsWith reads a string range key by prefix.
func TestParseKeyConditionBeginsWith(t *testing.T) {
	t.Parallel()

	key := table.KeySchema{
		Hash:  table.KeyAttribute{Name: "pk", Type: value.TypeS},
		Range: table.KeyAttribute{Name: "sk", Type: value.TypeS},
	}
	subs := autoSubs("pk = :p AND begins_with(sk, :sab)")

	kc, err := expression.ParseKeyCondition("pk = :p AND begins_with(sk, :sab)", key, subs, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	if _, cond, ok := kc.Range(); !ok || cond.Op != table.SortBeginsWith || *cond.Values[0].S != "ab" {
		t.Fatalf("Range() = %+v, %v", cond, ok)
	}

	hashOnly := table.KeySchema{Hash: key.Hash}
	_, err = expression.ParseKeyCondition("pk = :p AND sk = :s", hashOnly, autoSubs(":p :s"), map[string]struct{}{})
	if err == nil {
		t.Fatal("a range condition on a table without a range key parsed")
	}
}
