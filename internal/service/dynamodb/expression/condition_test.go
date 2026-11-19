package expression_test

import (
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
)

const filterPrefix = "Invalid FilterExpression: "

// filterRequest checks src as a Scan's FilterExpression: the substitutions,
// the expression, then the unused placeholders.
//
//nolint:ireturn,wrapcheck // returns what the parser under test returns
func filterRequest(src string, subs *expression.Substitutions) (expression.Condition, error) {
	s := autoSubs(src)
	if subs != nil {
		s = *subs
	}

	err := expression.CheckSubstitutions(s)
	if err != nil {
		return nil, err
	}

	used := map[string]struct{}{}

	c, err := expression.ParseCondition(expression.FilterExpression, src, s, used)
	if err != nil {
		return nil, err
	}

	return c, expression.CheckUnused(s, used)
}

func join(n int, term, sep string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = term
	}

	return strings.Join(parts, sep)
}

func values(kv ...string) map[string]api.AttributeValue {
	m := map[string]api.AttributeValue{}
	for _, k := range kv {
		m[k] = pool[k]
	}

	return m
}

// TestParseCondition holds the parser to the code and message the LocalStack
// image (gostack-parity-localstack:8b9a79f) answered for each expression, as a
// Scan FilterExpression. The probes' record is in the SQ-219 ticket evidence.
//
//nolint:lll,goconst,dupword // each row holds the exact text the LocalStack image answered; naming or splitting it hides what was probed
func TestParseCondition(t *testing.T) {
	t.Parallel()

	in := func(n int) (string, *expression.Substitutions) {
		vals := map[string]api.AttributeValue{}
		ops := make([]string, n)

		for i := range ops {
			ops[i] = ":i" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26))
			vals[ops[i]] = num("1")
		}

		return "a IN (" + strings.Join(ops, ", ") + ")", &expression.Substitutions{Values: vals}
	}
	in101, in101Subs := in(101)
	in100, in100Subs := in(100)

	tests := []struct {
		name string
		src  string
		subs *expression.Substitutions
		want string
	}{
		{"eof", "a =", nil, `Syntax error; token: "<EOF>", near: "="`},
		{"double equals", "a = = :v", nil, `Syntax error; token: "=", near: "= = :v"`},
		{"empty text", "", nil, "The expression can not be empty;"},
		{"whitespace", strings.Repeat(" ", 3), nil, `Syntax error; token: "<EOF>", near: "` + strings.Repeat(" ", 3) + `"`},
		{"reserved", "name = :v", nil, "Attribute name is a reserved keyword; reserved keyword: name"},
		{"reserved upper", "NAME = :v", nil, "Attribute name is a reserved keyword; reserved keyword: NAME"},
		{"reserved nested", "a.name = :v", nil, "Attribute name is a reserved keyword; reserved keyword: name"},
		{"reserved before undefined", "name = :zz", &expression.Substitutions{Values: values(":v")}, "Attribute name is a reserved keyword; reserved keyword: name"},
		{"reserved before undefined name", "#x = :v AND name = :v", &expression.Substitutions{Names: map[string]string{"#q": "q"}, Values: values(":v")}, "Attribute name is a reserved keyword; reserved keyword: name"},
		{"undefined value", "a = :zz", &expression.Substitutions{Values: values(":v")}, "An expression attribute value used in expression is not defined; attribute value: :zz"},
		{"undefined name", "#n = :v", &expression.Substitutions{Names: map[string]string{"#m": "x"}, Values: values(":v")}, "An expression attribute name used in the document path is not defined; attribute name: #n"},
		{"undefined name no map", "#n = :v", &expression.Substitutions{Values: values(":v")}, "An expression attribute name used in the document path is not defined; attribute name: #n"},
		{"name before value", "#x = :y", &expression.Substitutions{Names: map[string]string{"#q": "q"}, Values: values(":v")}, "An expression attribute name used in the document path is not defined; attribute name: #x"},
		{"value before name", ":y = #x", &expression.Substitutions{Names: map[string]string{"#q": "q"}, Values: values(":v")}, "An expression attribute value used in expression is not defined; attribute value: :y"},
		{"syntax before undefined", "#x = = :v", &expression.Substitutions{Names: map[string]string{"#q": "q"}, Values: values(":v")}, `Syntax error; token: "=", near: "= = :v"`},
		{"size of constant", "size(:s) > :n", nil, ""},
		{"exists two operands", "attribute_exists(a, b)", nil, "Incorrect number of operands for operator or function; operator or function: attribute_exists, number of operands: 2"},
		{"exists value", "attribute_exists(:v)", nil, "Operator or function requires a document path; operator or function: attribute_exists"},
		{"exists size", "attribute_exists(size(a))", nil, "Operator or function requires a document path; operator or function: attribute_exists"},
		{"not exists no operand", "attribute_not_exists()", nil, `Syntax error; token: ")", near: "()"`},
		{"type bad name", "attribute_type(a, :tXX)", nil, "Invalid attribute type name found; type: XX, valid types: {N,BS,L,B,NULL,M,S,SS,NS,BOOL}"},
		{"type number", "attribute_type(a, :n)", nil, "Incorrect operand type for operator or function; operator or function: attribute_type, operand type: N"},
		{"type set", "attribute_type(a, :ss)", nil, "Incorrect operand type for operator or function; operator or function: attribute_type, operand type: SS"},
		{"type size", "attribute_type(a, size(b))", nil, "Incorrect operand type for operator or function; operator or function: attribute_type, operand type: N"},
		{"type path", "attribute_type(a, b)", nil, "Incorrect operand type for operator or function; operator or function: attribute_type, operand type: {DOUBLE,B,L,NS,NULL,DICT,S,DECIMALSET,FLOAT,BS,INT,SS,M,BOOL,N,DOUBLESET,INTSET,FS,HD,HDS,DECIMAL}"},
		{"type constant first", "attribute_type(:v, :tS)", nil, ""},
		{"type one operand", "attribute_type(a)", nil, "Incorrect number of operands for operator or function; operator or function: attribute_type, number of operands: 1"},
		{"begins number", "begins_with(a, :n)", nil, "Incorrect operand type for operator or function; operator or function: begins_with, operand type: N"},
		{"begins set", "begins_with(a, :ss)", nil, "Incorrect operand type for operator or function; operator or function: begins_with, operand type: SS"},
		{"begins number first", "begins_with(:n, a)", nil, "Incorrect operand type for operator or function; operator or function: begins_with, operand type: N"},
		{"begins size", "begins_with(size(a), :s)", nil, "Incorrect operand type for operator or function; operator or function: begins_with, operand type: N"},
		{"begins path", "begins_with(a, b)", nil, ""},
		{"begins binary", "begins_with(a, :bin)", nil, ""},
		{"begins three", "begins_with(a, :s, :s)", nil, "Incorrect number of operands for operator or function; operator or function: begins_with, number of operands: 3"},
		{"contains one", "contains(a)", nil, "Incorrect number of operands for operator or function; operator or function: contains, number of operands: 1"},
		{"contains set", "contains(a, :ss)", nil, ""},
		{"contains map", "contains(a, :m)", nil, ""},
		{"contains size", "contains(size(a), :n)", nil, ""},
		{"unknown function", "foo(a)", nil, "Invalid function name; function: foo"},
		{"unknown function operand", "foo(a) = :v", nil, "Invalid function name; function: foo"},
		{"function case", "BEGINS_WITH(a, :s)", nil, "Invalid function name; function: BEGINS_WITH"},
		{"size case", "Size(a) > :n", nil, "Invalid function name; function: Size"},
		{"size as condition", "size(a)", nil, "The function is not allowed to be used this way in an expression; function: size"},
		{"exists as operand", "attribute_exists(a) = :v", nil, "The function is not allowed to be used this way in an expression; function: attribute_exists"},
		{"exists in exists", "attribute_exists(attribute_exists(a))", nil, "The function is not allowed to be used this way in an expression; function: attribute_exists"},
		{"exists in IN", "a IN (attribute_exists(b))", nil, "The function is not allowed to be used this way in an expression; function: attribute_exists"},
		{"begins compared", "begins_with(a, :s) = :b", nil, "The function is not allowed to be used this way in an expression; function: begins_with"},
		{"size of exists", "size(attribute_exists(a)) > :n", nil, "The function is not allowed to be used this way in an expression; function: attribute_exists"},
		{"size of size", "size(size(a)) > :n", nil, "Incorrect operand type for operator or function; operator or function: size, operand type: N"},
		{"size of number", "size(:n) > :n", nil, "Incorrect operand type for operator or function; operator or function: size, operand type: N"},
		{"size of bool", "size(:b) > :n", nil, "Incorrect operand type for operator or function; operator or function: size, operand type: BOOL"},
		{"size of null", "size(:nul) > :n", nil, "Incorrect operand type for operator or function; operator or function: size, operand type: NULL"},
		{"size of map", "size(:m) > :n", nil, ""},
		{"size two", "size(a, b) > :n", nil, "Incorrect number of operands for operator or function; operator or function: size, number of operands: 2"},
		{"size none", "size() > :n", nil, `Syntax error; token: ")", near: "() >"`},
		{"between reversed", "a BETWEEN :hi AND :lo", nil, "The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: {N:9}, upper bound operand: AttributeValue: {N:1}"},
		{"between reversed strings", "a BETWEEN :w AND :v", nil, "The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: {S:w}, upper bound operand: AttributeValue: {S:v}"},
		{"between raw numbers", "a BETWEEN :x AND :y", &expression.Substitutions{Values: map[string]api.AttributeValue{":x": num("1.50E2"), ":y": num("-0.5")}}, "The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: {N:1.50E2}, upper bound operand: AttributeValue: {N:-0.5}"},
		{"between reversed binary", "a BETWEEN :x AND :y", &expression.Substitutions{Values: map[string]api.AttributeValue{":x": {B: []byte("b")}, ":y": {B: []byte("a")}}}, "The BETWEEN operator requires upper bound to be greater than or equal to lower bound; lower bound operand: AttributeValue: {B:java.nio.HeapByteBuffer[pos=0 lim=1 cap=1]}, upper bound operand: AttributeValue: {B:java.nio.HeapByteBuffer[pos=0 lim=1 cap=1]}"},
		{"between mixed", "a BETWEEN :s AND :n", nil, "The BETWEEN operator requires same data type for lower and upper bounds; lower bound operand: AttributeValue: {S:a}, upper bound operand: AttributeValue: {N:1}"},
		{"between bool", "a BETWEEN :b AND :b", nil, "Incorrect operand type for operator or function; operator or function: BETWEEN, operand type: BOOL"},
		{"between map", "a BETWEEN :m AND :m", nil, "Incorrect operand type for operator or function; operator or function: BETWEEN, operand type: M"},
		{"between equal", "a BETWEEN :v AND :v", nil, ""},
		{"between size", "size(a) BETWEEN :lo AND :hi", nil, ""},
		{"between path bounds", "a BETWEEN b AND :v", nil, ""},
		{"in 101", in101, in101Subs, "The IN operator is provided with too many operands; number of operands: 101"},
		{"in 100", in100, in100Subs, ""},
		{"in empty", "a IN ()", nil, `Syntax error; token: ")", near: "()"`},
		{"in mixed types", "a IN (:m, :b)", nil, ""},
		{"too long", "a = :v" + strings.Repeat(" OR a = :v", 500), nil, "Expression size has exceeded the maximum allowed size; expression size: 5006"},
		{"too long before syntax", "a = = :v" + strings.Repeat(" OR a = :v", 500), nil, "Expression size has exceeded the maximum allowed size; expression size: 5008"},
		{"size 4097", "a = :v AND b" + strings.Repeat("b", 4097-12), nil, "Expression size has exceeded the maximum allowed size; expression size: 4097"},
		{"size counts UTF-16", strings.Repeat("é", 2100), nil, `Syntax error; token: "é", near: "éé"`},
		{"redundant parens", "((a = :v))", nil, "The expression has redundant parentheses;"},
		{"one parens", "(a = :v)", nil, ""},
		{"redundant before reserved", "name = :v AND ((a = :v))", nil, "The expression has redundant parentheses;"},
		{"redundant before syntax", "((a = :v)) = ", nil, "The expression has redundant parentheses;"},
		{"redundant deep", strings.Repeat("(", 300) + "a = :v" + strings.Repeat(")", 300), nil, "The expression has redundant parentheses;"},
		{"redundant operand", "((a)) = :v", nil, "The expression has redundant parentheses;"},
		{"redundant under not", "NOT ((a = :v))", nil, "The expression has redundant parentheses;"},
		{"operand parens", "(a) = :v", nil, ""},
		{"value parens", "a = (:v)", nil, ""},
		{"function parens", "(attribute_exists(a))", nil, ""},
		{"size parens", "(size(a)) = :n", nil, ""},
		{"path in function parens", "attribute_exists((a))", nil, ""},
		{"operand and", "(a AND b = :v)", nil, `Syntax error; token: "AND", near: "a AND b"`},
		{"constants", ":v = :v", nil, ""},
		{"same path", "a = a", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: =, first operand: [a]"},
		{"same path lt", "a < a", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: <, first operand: [a]"},
		{"same path ne", "a <> a", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: <>, first operand: [a]"},
		{"same path between", "a BETWEEN :v AND a", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: BETWEEN, first operand: [a]"},
		{"same bounds", "a BETWEEN b AND b", nil, ""},
		{"same path in", "a IN (:v, a)", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: IN, first operand: [a]"},
		{"same in list", "a IN (b, b)", nil, ""},
		{"same path begins", "begins_with(a, a)", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: begins_with, first operand: [a]"},
		{"same path contains", "contains(a, a)", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: contains, first operand: [a]"},
		{"same size", "size(a) = size(a)", nil, ""},
		{"same nested", "a.b = a.b", nil, "The first operand must be distinct from the remaining operands for this operator or function; operator: =, first operand: [a, b]"},
		{"same substituted", "#a = a", &expression.Substitutions{Names: map[string]string{"#a": "a"}}, "The first operand must be distinct from the remaining operands for this operator or function; operator: =, first operand: [a]"},
		{"distinct before undefined", "a = a AND b = :zz", &expression.Substitutions{Values: values(":v")}, "The first operand must be distinct from the remaining operands for this operator or function; operator: =, first operand: [a]"},
		{"undefined before distinct", "b = :zz AND a = a", &expression.Substitutions{Values: values(":v")}, "An expression attribute value used in expression is not defined; attribute value: :zz"},
		{"type before undefined", "begins_with(a, :n) AND b = :zz", &expression.Substitutions{Values: values(":n")}, "Incorrect operand type for operator or function; operator or function: begins_with, operand type: N"},
		{"undefined before type", "b = :zz AND begins_with(a, :n)", &expression.Substitutions{Values: values(":n")}, "An expression attribute value used in expression is not defined; attribute value: :zz"},
		{"lt bool", "a < :b", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: BOOL"},
		{"lt map", "a < :m", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: M"},
		{"lt set", "a < :ss", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: SS"},
		{"lt number set", "a < :ns", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: NS"},
		{"lt list", "a < :l", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: L"},
		{"lt null", "a < :nul", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: NULL"},
		{"size lt bool", "size(a) < :b", nil, "Incorrect operand type for operator or function; operator or function: <, operand type: BOOL"},
		{"lt binary", "a < :bin", nil, ""},
		{"eq map", "a = :m", nil, ""},
		{"nested path", "a.b[1].c = :v", nil, ""},
		{"negative index", "a[-1] = :v", nil, `Syntax error; token: "-", near: "[-1"`},
		{"name index", "a[x] = :v", nil, `Syntax error; token: "x", near: "[x]"`},
		{"leading zero index", "a[01] = :v", nil, `Syntax error; token: "1", near: "01]"`},
		{"index max", "a[2147483647] = :v", nil, ""},
		{"index over", "a[2147483648] = :v", nil, "List index is not within the allowable range; index: [2147483648]"},
		{"index far over", "a[99999999999999999999999] = :v", nil, "List index is not within the allowable range; index: [99999999999999999999999]"},
		{"syntax before index", "a[99999999999] = = :v", nil, `Syntax error; token: "=", near: "= = :v"`},
		{"depth 33", join(33, "a", ".") + " = :v", nil, "The document path has too many nesting levels; nesting levels: 33"},
		{"depth 32", join(32, "a", ".") + " = :v", nil, ""},
		{"depth indexes", "a" + strings.Repeat("[0]", 32) + " = :v", nil, "The document path has too many nesting levels; nesting levels: 33"},
		{"syntax before depth", join(33, "a", ".") + " = = :v", nil, `Syntax error; token: "=", near: "= = :v"`},
		{"and eof", "a = :v AND", nil, `Syntax error; token: "<EOF>", near: "AND"`},
		{"not alone", "NOT", nil, `Syntax error; token: "<EOF>", near: "NOT"`},
		{"not path", "NOT a", nil, `Syntax error; token: "<EOF>", near: "a"`},
		{"dollar", "a = :v $", nil, `Syntax error; token: "$", near: ":v $"`},
		{"double equals joined", "a == :v", nil, `Syntax error; token: "=", near: "== :v"`},
		{"two conditions", "a = :v b = :v", nil, `Syntax error; token: "b", near: ":v b ="`},
		{"path alone", "a", nil, `Syntax error; token: "<EOF>", near: "a"`},
		{"double negation", "NOT NOT a = :v", nil, ""},
		{"lower keywords", "a between :lo and :hi or not a in (:v)", nil, ""},
		{"dot first", ".a = :v", nil, `Syntax error; token: ".", near: ".a"`},
		{"dot last", "a. = :v", nil, `Syntax error; token: "=", near: ". = :v"`},
		{"keyword in path", "a.between = :v", nil, `Syntax error; token: "between", near: ".between ="`},
		{"remove keyword", "remove = :v", nil, `Syntax error; token: "remove", near: "remove ="`},
		{"not keyword", "not = :v", nil, `Syntax error; token: "=", near: "not = :v"`},
		{"colon alone", "a = :", nil, `Syntax error; token: ":", near: "= :"`},
		{"hash alone", "# = :v", nil, `Syntax error; token: "#", near: "# ="`},
		{"unicode name", "é = :v", nil, `Syntax error; token: "é", near: "é ="`},
		{"number literal", "a = 5", nil, `Syntax error; token: "5", near: "= 5"`},
		{"string literal", "a = 'x'", nil, `Syntax error; token: "'", near: "= 'x"`},
		{"leading underscore", "_a = :v", nil, `Syntax error; token: "_", near: "_a"`},
		{"leading digit", "1a = :v", nil, `Syntax error; token: "1", near: "1a"`},
		{"dash", "a-b = :v", nil, `Syntax error; token: "-", near: "a-b"`},
		{"underscore", "a_b = :v", nil, ""},
		{"value underscore", "a = :v_1", &expression.Substitutions{Values: map[string]api.AttributeValue{":v_1": str("v")}}, ""},
		{"spaced path", "a [ 1 ] . b = :v", nil, ""},
		{"tab newline", "a\t=\n:v", nil, ""},
		{"size name", "size = :v", nil, ""},
		{"function names as paths", "begins_with = :v AND attribute_exists = :v", nil, ""},
		{"function space", "size (a) > :n", nil, ""},
		{"name with index", "#a[1].#b = :v", nil, ""},
		{"name with dot", "#a = :v", &expression.Substitutions{Names: map[string]string{"#a": "a.b"}, Values: values(":v")}, ""},
		{"operators 301", join(151, "a = :v", " AND "), nil, "The expression contains too many operators; operator count: 301"},
		{"operators 299", join(150, "a = :v", " AND "), nil, ""},
		{"functions count", join(151, "attribute_exists(a)", " OR "), nil, "The expression contains too many operators; operator count: 301"},
		{"functions 299", join(150, "attribute_exists(a)", " OR "), nil, ""},
		{"between counts", join(151, "a BETWEEN :v AND :v", " AND "), nil, "The expression contains too many operators; operator count: 301"},
		{"in counts", join(151, "a IN (:v)", " AND "), nil, "The expression contains too many operators; operator count: 301"},
		{"not counts", strings.Repeat("NOT ", 300) + "a = :v", nil, "The expression contains too many operators; operator count: 301"},
		{"size counts", join(101, "size(a) = :n", " AND "), nil, "The expression contains too many operators; operator count: 301"},
		{"parens do not count", join(150, "(a = :v)", " AND "), nil, ""},
		{"syntax before count", join(301, "a = :v", " AND ") + " = ", nil, `Syntax error; token: "=", near: ":v = "`},
		{"reserved before count", join(301, "a = :v", " AND ") + " AND name = :v", nil, "Attribute name is a reserved keyword; reserved keyword: name"},
		{"in count before later undefined", "a IN (" + join(101, ":v", ", ") + ") AND b = :zz", &expression.Substitutions{Values: values(":v")}, "The IN operator is provided with too many operands; number of operands: 101"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := filterRequest(tt.src, tt.subs)

			want := tt.want
			if want != "" {
				want = filterPrefix + want
			}

			wantMessage(t, err, want)
		})
	}
}

// TestCheckSubstitutions holds the request-level checks to the image's
// messages and order: names before values, both before any expression.
//
//nolint:lll,goconst // each row holds the exact text the LocalStack image answered; naming or splitting it hides what was probed
func TestCheckSubstitutions(t *testing.T) {
	t.Parallel()

	bad := num("abc")
	tests := []struct {
		name string
		src  string
		subs expression.Substitutions
		want string
	}{
		{"names empty", "a = = :v", expression.Substitutions{Names: map[string]string{}, Values: values(":v")}, "ExpressionAttributeNames must not be empty"},
		{"values empty", "attribute_exists(a)", expression.Substitutions{Values: map[string]api.AttributeValue{}}, "ExpressionAttributeValues must not be empty"},
		{"name key", "a = = :v", expression.Substitutions{Names: map[string]string{"abc": "x"}, Values: values(":v")}, `ExpressionAttributeNames contains invalid key: Syntax error; key: "abc"`},
		{"name key dash", "#a = :v", expression.Substitutions{Names: map[string]string{"#a": "a", "#a-b": "x"}, Values: values(":v")}, `ExpressionAttributeNames contains invalid key: Syntax error; key: "#a-b"`},
		{"value key", "a = :v", expression.Substitutions{Values: map[string]api.AttributeValue{":v": str("v"), "abc": str("x")}}, `ExpressionAttributeValues contains invalid key: Syntax error; key: "abc"`},
		{"value key dash", "a = :v-b", expression.Substitutions{Values: map[string]api.AttributeValue{":v-b": str("v")}}, `ExpressionAttributeValues contains invalid key: Syntax error; key: ":v-b"`},
		{"empty name", "#a = :v", expression.Substitutions{Names: map[string]string{"#a": ""}, Values: values(":v")}, "ExpressionAttributeNames contains invalid value: Empty attribute name for key #a"},
		{"bad number", "a = = :bad", expression.Substitutions{Values: map[string]api.AttributeValue{":bad": bad}}, "ExpressionAttributeValues contains invalid value: A value provided cannot be converted into a number for key :bad"},
		{"empty value", "a = :empty", expression.Substitutions{Values: map[string]api.AttributeValue{":empty": {}}}, "ExpressionAttributeValues contains invalid value: Supplied AttributeValue is empty, must contain exactly one of the supported datatypes for key :empty"},
		{"names before values", "a = :bad", expression.Substitutions{Names: map[string]string{"abc": "x"}, Values: map[string]api.AttributeValue{":bad": bad}}, `ExpressionAttributeNames contains invalid key: Syntax error; key: "abc"`},
		{"unused value", "a = :v", expression.Substitutions{Values: values(":v", ":w")}, "Value provided in ExpressionAttributeValues unused in expressions: keys: {:w}"},
		{"unused values", "a = :v", expression.Substitutions{Values: values(":v", ":w", ":s")}, "Value provided in ExpressionAttributeValues unused in expressions: keys: {:s, :w}"},
		{"unused name", "#a = :v", expression.Substitutions{Names: map[string]string{"#a": "a", "#b": "b"}, Values: values(":v")}, "Value provided in ExpressionAttributeNames unused in expressions: keys: {#b}"},
		{"unused names before values", "a = :v", expression.Substitutions{Names: map[string]string{"#b": "b"}, Values: values(":v", ":w")}, "Value provided in ExpressionAttributeNames unused in expressions: keys: {#b}"},
		{"syntax before unused", "a = = :v", expression.Substitutions{Values: values(":v", ":w")}, filterPrefix + `Syntax error; token: "=", near: "= = :v"`},
		{"unused in hash order", "a = :v", expression.Substitutions{Values: values(":v", ":a", ":h", ":p", ":zz")}, "Value provided in ExpressionAttributeValues unused in expressions: keys: {:p, :a, :zz, :h}"},
		{"value used twice", "a = :v AND b = :v", expression.Substitutions{Values: values(":v")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := *bad.N

			_, err := filterRequest(tt.src, &tt.subs)
			wantMessage(t, err, tt.want)

			if *bad.N != before {
				t.Fatalf("request value changed to %q", *bad.N)
			}
		})
	}
}

// TestEval holds evaluation to what the image's Scan returned for the same
// filter over the same item: true where it counted the item.
func TestEval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src  string
		want bool
	}{
		{"zz <> :v", true},
		{"zz = :v", false},
		{"NOT zz = :v", true},
		{"zz < :v", false},
		{"a <> :n1", true},
		{"a < :n1", false},
		{"n = :n5x", true},
		{"contains(ss, :s)", true},
		{"contains(ns, :n1)", true},
		{"contains(l, :s)", true},
		{"contains(a, :s2)", true},
		{"contains(bin, :binb)", true},
		{"contains(l, :mz)", true},
		{"contains(ss, :n1)", false},
		{"contains(m, :sz)", false},
		{"contains(n, :n5x)", false},
		{"size(a) = :n3", true},
		{"size(ss) = :n2", true},
		{"size(l) = :n3", true},
		{"size(m) = :n2", true},
		{"size(bin) = :n3", true},
		{"size(n) = :n1", false},
		{"size(t) = :n1", false},
		{"size(nul) = :n1", false},
		{"size(zz) = :n0", false},
		{"NOT size(zz) = :n0", true},
		{"size(u) = :n2", true},
		{"attribute_type(n, :tN)", true},
		{"attribute_type(zz, :tN)", false},
		{"begins_with(a, :sab)", true},
		{"begins_with(n, :s)", false},
		{"begins_with(bin, :binab)", true},
		{"begins_with(ss, :s)", false},
		{"m.b = :sz", true},
		{"l[2].b = :sz", true},
		{"l[5] = :s", false},
		{"a[0] = :s", false},
		{"m.c[0] = :n1", true},
		{"NOT attribute_exists(zz)", true},
		{"a IN (:v, :sabc)", true},
		{"n BETWEEN :n1 AND :n9", true},
		{"a BETWEEN :n1 AND :n9", false},
		{"a > :n1", false},
		{"NOT a > :n1", true},
		{"ss = :ssba", true},
		{"a = :sabc OR a = :v AND n = :n9", true},
		{"NOT a = :v AND n = :n9", false},
		{"size(a) > size(ss)", true},
		{"n = n2", false},
		{"zz = zy", false},
		{"zz <> zy", true},
		{"NOT zz <> :v", false},
		{"attribute_exists(m.b)", true},
		{"attribute_exists(l[2].b)", true},
		{"attribute_exists(l[9])", false},
		{"attribute_not_exists(a.b)", true},
		{"a IN (:n1)", false},
		{":v = :v", true},
		{":v <> :v", false},
		{":n1 < :n9", true},
		{"contains(:sabc, :s)", true},
		{"size(:sabc) = :n3", true},
		{"t = :b", true},
		{"nul = :nul", true},
		{"m = :mz", false},
		{"size(:l) = :n0", true},
		{"(a) = :sabc", true},
		{"a BETWEEN b AND :v", false},
		{"pk = :p AND :n9 > sk", true},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			t.Parallel()

			c, err := filterRequest(tt.src, nil)
			if err != nil {
				t.Fatal(err)
			}

			got, err := c.Eval(richItem())
			if err != nil || got != tt.want {
				t.Fatalf("Eval = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// TestEvalMissingItem evaluates against no item, as a conditional write to an
// empty key does: every path is missing.
func TestEvalMissingItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src  string
		want bool
	}{
		{"attribute_not_exists(pk)", true},
		{"attribute_exists(pk)", false},
		{"a <> :v", true},
		{"a = :v", false},
		{"NOT a < :v", true},
		{"size(a) = :n0", false},
		{":v = :v", true},
	}
	for _, tt := range tests {
		c, err := filterRequest(tt.src, nil)
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.Eval(nil)
		if err != nil || got != tt.want {
			t.Errorf("%s: Eval(nil) = %v, %v; want %v", tt.src, got, err, tt.want)
		}
	}
}
