package dynamodb

import (
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages a statement's WHERE clause and key are refused with.
const (
	msgWhereKeys        = "Where clause does not contain a mandatory equality on all key attributes"
	msgKeyTwice         = "Multiple conditions on same key %s. Only single item Update/Insert/Delete are supported"
	msgStatementKeyType = "Key attribute's data type should match its data type in table's schema: Key "
	msgKeyAbsent        = "Key attribute should be present in the item: Key "
	msgOrderNoWhere     = "Must have WHERE clause in the statement when using ORDER BY clause."
	msgInvalidToken     = "Invalid NextToken"
	msgDuplicateItem    = "Duplicate primary key exists in table"
	msgDuplicateAlias   = "Duplicate identifiers in select clause: "
	msgStatementLong    = "Member must have length less than or equal to 8192"
	maxStatementBytes   = 8192

	msgStatementTable = "Table name should be within the length [3, 255] and only contain " +
		"'a-z', 'A-Z', '0-9', '-', '_', '.'."
	msgRangeType = "One or more parameter values were invalid: Condition parameter type does not match schema type"
)

// statementError is the ValidationException for a statement the parser
// refused, or err when it is another error.
func statementError(err error) error {
	if pe, ok := errors.AsType[*partiql.Error](err); ok {
		return value.Invalid(pe.Message)
	}

	return err
}

// exprPrefixes start the messages of the expressions a statement compiles to.
var exprPrefixes = []string{
	"Invalid ConditionExpression: ", "Invalid FilterExpression: ", "Invalid KeyConditionExpression: ",
	"Invalid ProjectionExpression: ", "Invalid UpdateExpression: ",
}

// statementFailure is err as a statement answers it. A statement holds no
// expression of its own, so DynamoDB Local leaves off the prefix that names
// one (verification/SQ-229 probe3).
func statementFailure(err error) error {
	ae := asAWSError(err)
	if ae == nil || ae.Code != codeValidation {
		return err
	}

	for _, prefix := range exprPrefixes {
		if msg, ok := strings.CutPrefix(ae.Message, prefix); ok {
			return value.Invalid(msg)
		}
	}

	return err
}

// statementTable looks a statement's table up. A name that is not a table
// name is refused in DynamoDB Local's PartiQL words.
func (c caller) statementTable(name *string) (string, *tableEntry, error) {
	resolved, e, err := c.lookup(name)

	return resolved, e, statementNameError(err)
}

// statementName checks a statement's table name alone, as statementTable does
// before it looks the table up.
func statementName(name string) error {
	resolved := tableName(name)

	return statementNameError(checkName(&resolved))
}

// statementNameError is err in DynamoDB Local's PartiQL words when it refuses
// the table name.
func statementNameError(err error) error {
	ae := asAWSError(err)
	if ae != nil && ae.Code == codeValidation && ae.Message == msgTableName {
		return value.Invalid(msgStatementTable)
	}

	return err
}

// aliasRefusal is the refusal of a statement that names an alias, nil when it
// names none.
func aliasRefusal(stmt *partiql.Statement) error {
	if stmt.Aliased {
		return value.Invalid(partiql.MsgAliasing)
	}

	return nil
}

// msgWhereBoolean refuses a WHERE clause that is no condition: a path, a value
// or size(), where the image wants an expression that resolves to a boolean.
const msgWhereBoolean = "WHERE clause must contain an expression that resolves to a boolean"

// msgSyntaxError starts the message of an expression the parser could not read.
const msgSyntaxError = "Syntax error;"

// nonCondition reports whether n, the whole of a WHERE clause, is an operand
// where the image wants a condition (SQ-418 p4b a-*, p4c q*).
func nonCondition(n *partiql.Node) bool {
	return n.Kind == partiql.KindPath || partiql.IsValue(n) || (n.Kind == partiql.KindCall && n.Name == "size")
}

// isSyntaxError reports whether err is the expression parser's refusal of text
// it could not read. A statement's text is read by its own parser, so text it is
// rendered to that the expression parser cannot read is gostack's own (a bare
// operand under AND), which no refusal of the image's is named for.
func isSyntaxError(err error) bool {
	ae := asAWSError(statementFailure(err))

	return ae != nil && strings.HasPrefix(ae.Message, msgSyntaxError)
}

// ownRefusal is the refusal the image names of a statement before it looks its
// table, key or index up: the statement's own mistakes, in the order it reads
// them (SQ-418 c2, c3, c5, p2a, p4b). A SELECT's table alias, table name, alias
// and list come first, then every statement's WHERE, an UPDATE's actions, and
// an alias an UPDATE or DELETE writes. nil when the statement has none; an
// INSERT holds no expression of its own. Only the classes the image names first
// answer: a function, an operand, an operator count, an overlap, an alias, an
// empty step and a WHERE that is no condition. A syntax error of the text it is
// rendered to is gostack's own, and leaves what failed in place.
//
// The statement is rendered whole and afresh here, a SELECT's list first as
// the read renders it, so that every path to the error quotes the same text,
// with the same placeholders and operator count, whether the table is there or
// not.
func ownRefusal(stmt *partiql.Statement, params []api.AttributeValue) error {
	if stmt.Kind == partiql.Insert {
		return aliasRefusal(stmt)
	}

	r := newRender(params)

	switch stmt.Kind {
	case partiql.Select:
		err := selectHead(stmt, r)
		if err != nil {
			return err
		}
	case partiql.Exists:
		// It reads its SELECT's list unread and refuses the table's alias before
		// anything else of it, the table's name next (SQ-418 p2a x6, p2b x).
		if stmt.TableAliased {
			return value.Invalid(partiql.MsgAliasing)
		}

		err := statementName(stmt.Table)
		if err != nil {
			return err
		}
	default:
	}

	err := ownExpressions(stmt, r)
	if err != nil {
		return err
	}

	if stmt.Kind == partiql.Select || stmt.Kind == partiql.Exists {
		return nil
	}

	return aliasRefusal(stmt)
}

// selectHead makes the checks the image makes of a SELECT before its WHERE: the
// table's alias and name, then an alias, then the paths of the list as one
// (SQ-418 c5). It renders the list into r.
func selectHead(stmt *partiql.Statement, r *render) error {
	if stmt.TableAliased {
		return value.Invalid(partiql.MsgAliasing)
	}

	err := statementName(stmt.Table)
	if err != nil {
		return err
	}

	err = aliasRefusal(stmt)
	if err != nil {
		return err
	}

	_, _, err = projection(stmt.Projection, r)

	return err
}

// ownExpressions renders a statement's WHERE and an UPDATE's actions into r and
// checks them as the expressions they compile to. A SELECT's list was rendered
// into r before.
func ownExpressions(stmt *partiql.Statement, r *render) error {
	if stmt.Where != nil && nonCondition(stmt.Where) {
		return value.Invalid(msgWhereBoolean)
	}

	cond, err := renderCond(stmt.Where, r)
	if err != nil {
		return err
	}

	var (
		update *string
		rank   []int
	)

	if stmt.Kind == partiql.Update {
		var text string

		text, rank, err = r.update(stmt.Actions)
		if err != nil {
			return err
		}

		update = &text
	}

	err = checkStatementExprs(r.attributeNames(), r.attributeValues(), cond, update, rank, 0)
	if isSyntaxError(err) {
		return nil
	}

	return err
}

// refusedInOrder runs body, the compile or the read of one statement of an
// entry point (ExecuteStatement, a batch, a transaction), and answers as the
// image does: a statement's own mistakes (ownRefusal) before whatever else is
// wrong with it, a missing table, a missing or ill-typed key, an index or a
// token. It is the one place that orders them, so no site of the body can leave
// one out. An alias is refused whatever else is right of the statement; the
// rest is looked for where body fails, so a statement that is right costs
// nothing more.
func refusedInOrder[T any](
	stmt *partiql.Statement, params []api.AttributeValue, body func() (T, error),
) (T, error) {
	var zero T

	if stmt.Aliased || stmt.TableAliased {
		own := ownRefusal(stmt, params)
		if own != nil {
			return zero, own
		}
	}

	v, err := body()
	if err == nil {
		return v, nil
	}

	own := ownRefusal(stmt, params)
	if own != nil {
		return zero, own
	}

	return zero, err
}

// The refusals of a path that goes below a key attribute, and of an ORDER BY of
// a path. The key's name follows: " <name>" in a write, ": <name>" in a SELECT.
const (
	msgKeyPath   = "Key attributes must be scalars; list random access '[]' and map lookup '.' are not allowed: Key"
	msgOrderPath = "Argument to ORDER BY must be of type VariableReference. Type: PATH"
)

// belowKey is the name of the first attribute of names that a path of n goes
// below, as in `id['x']`, `id.x` or `id[0]`, in the order n reads them; "" for
// none.
func belowKey(n *partiql.Node, names ...string) string {
	if n.Kind == partiql.KindPath && len(n.Path) > 1 && slices.Contains(names, n.Path[0].Name) {
		return n.Path[0].Name
	}

	for _, a := range n.Args {
		name := belowKey(a, names...)
		if name != "" {
			return name
		}
	}

	return ""
}

// writeKeyPath is the refusal of a write whose WHERE goes below an attribute of
// the table's key, which the image makes before it asks for the key's equality
// (SQ-418 p4b b-*), nil when it does not.
func writeKeyPath(where *partiql.Node, key table.KeySchema) error {
	if where == nil {
		return nil
	}

	parts := keyParts(key)
	names := make([]string, len(parts))

	for i, a := range parts {
		names[i] = a.Name
	}

	name := belowKey(where, names...)
	if name == "" {
		return nil
	}

	return value.Invalid(msgKeyPath + " " + name)
}

// selectKeyPaths is what the image refuses of a SELECT's paths once it has the
// table and the token: a WHERE that goes below an attribute of the table's key,
// the hash key's first, then an ORDER BY of a path and not of an attribute
// (SQ-418 p4a, p4b, p4g). It reads a key's index attributes too, which is
// left out (docs/compat.md).
func selectKeyPaths(stmt *partiql.Statement, key table.KeySchema) error {
	if stmt.Where != nil {
		for _, a := range keyParts(key) {
			if belowKey(stmt.Where, a.Name) != "" {
				return value.Invalid(msgKeyPath + ": " + a.Name)
			}
		}
	}

	if len(stmt.OrderBy) > 1 {
		return value.Invalid(msgOrderPath)
	}

	return nil
}

// checkRangeTypes refuses a Query's range key conditions whose values are not
// of the range key's type, as DynamoDB Local does: a BETWEEN with the key's
// message and an IN with the key condition's (verification/SQ-229 probe3).
func checkRangeTypes(rangeCond *partiql.Node, rest []*partiql.Node, attr table.KeyAttribute, r *render) error {
	if rangeCond != nil && rangeCond.Kind == partiql.KindBetween && !ofType(rangeCond.Args[1:], attr.Type, r) {
		return value.Invalid(msgStatementKeyType + attr.Name)
	}

	for _, c := range rest {
		if c.Kind == partiql.KindIn && isAttr(c.Args[0], attr.Name) && !ofType(c.Args[1:], attr.Type, r) {
			return value.Invalid(msgRangeType)
		}
	}

	return nil
}

// ofType reports whether every value of ns is of type typ. A node that is no
// value, or does not resolve, is left to the expression that holds it.
func ofType(ns []*partiql.Node, typ string, r *render) bool {
	for _, n := range ns {
		if !partiql.IsValue(n) {
			continue
		}

		v, err := r.resolve(n)
		if err == nil && value.Type(&v) != typ {
			return false
		}
	}

	return true
}

// shapeError is a WHERE clause that does not name one item by its whole key.
type shapeError struct {
	err error
}

func (e *shapeError) Error() string { return e.err.Error() }

func (e *shapeError) Unwrap() error { return e.err }

// conjuncts are the conditions the top-level ANDs of n join.
func conjuncts(n *partiql.Node) []*partiql.Node {
	if n == nil {
		return nil
	}

	if n.Kind == partiql.KindAnd {
		return append(conjuncts(n.Args[0]), conjuncts(n.Args[1])...)
	}

	return []*partiql.Node{n}
}

// joinAll ANDs ns back together, nil for none.
func joinAll(ns []*partiql.Node) *partiql.Node {
	var out *partiql.Node

	for _, n := range ns {
		if out == nil {
			out = n
		} else {
			out = &partiql.Node{Kind: partiql.KindAnd, At: out.At, Args: []*partiql.Node{out, n}}
		}
	}

	return out
}

// isAttr reports whether n is the top-level attribute name.
func isAttr(n *partiql.Node, name string) bool {
	return n.Kind == partiql.KindPath && len(n.Path) == 1 && n.Path[0].Name == name
}

// equality is the value n compares the attribute name equal to, when n is
// name = value or value = name.
func equality(n *partiql.Node, name string) (*partiql.Node, bool) {
	if n.Kind != partiql.KindCompare || n.Op != "=" {
		return nil, false
	}

	a, b := n.Args[0], n.Args[1]

	switch {
	case isAttr(a, name) && partiql.IsValue(b):
		return b, true
	case isAttr(b, name) && partiql.IsValue(a):
		return a, true
	default:
		return nil, false
	}
}

// mentions reports whether any path in n starts with one of names.
func mentions(n *partiql.Node, names ...string) bool {
	if n.Kind == partiql.KindPath {
		for _, name := range names {
			if name != "" && n.Path[0].Name == name {
				return true
			}
		}
	}

	for _, a := range n.Args {
		if mentions(a, names...) {
			return true
		}
	}

	return false
}

// keyParts are the attributes of key, hash first.
func keyParts(key table.KeySchema) []table.KeyAttribute {
	if key.HasRange() {
		return []table.KeyAttribute{key.Hash, key.Range}
	}

	return []table.KeyAttribute{key.Hash}
}

// singleItem splits a WHERE clause into the item key its equalities name, all
// of key's attributes, and the conditions left over.
func singleItem(where *partiql.Node, key table.KeySchema, r *render) (value.Item, []*partiql.Node, error) {
	attrs := keyParts(key)
	found := make([]*partiql.Node, len(attrs))

	var rest []*partiql.Node

next:
	for _, c := range conjuncts(where) {
		for i, a := range attrs {
			if v, ok := equality(c, a.Name); ok {
				if found[i] != nil {
					return nil, nil, &shapeError{value.Invalid(strings.Replace(msgKeyTwice, "%s", a.Name, 1))}
				}

				found[i] = v

				continue next
			}
		}

		rest = append(rest, c)
	}

	k := make(value.Item, len(attrs))

	for i, a := range attrs {
		if found[i] == nil {
			return nil, nil, &shapeError{value.Invalid(msgWhereKeys)}
		}

		v, err := r.resolve(found[i])
		if err != nil {
			return nil, nil, err
		}

		k[a.Name] = v
	}

	err := checkKeyTypes(k, key, false)
	if err != nil {
		return nil, nil, err
	}

	return k, rest, nil
}

// checkKeyTypes checks that item holds each key attribute, when present asks
// for it, with the schema's type.
//
//nolint:revive // flag-parameter: an INSERT's item must hold the key, a WHERE's already does
func checkKeyTypes(item value.Item, key table.KeySchema, present bool) error {
	for _, a := range keyParts(key) {
		v, ok := item[a.Name]
		if !ok && present {
			return value.Invalid(msgKeyAbsent + a.Name)
		}

		if ok && value.Type(&v) != a.Type {
			return value.Invalid(msgStatementKeyType + a.Name)
		}
	}

	return nil
}

// asAWSError is the AWS error err is, nil when it is none.
func asAWSError(err error) *awserr.Error {
	ae, _ := errors.AsType[*awserr.Error](err)

	return ae
}

// isCode reports whether err is the AWS error code.
func isCode(err error, code string) bool {
	var ae *awserr.Error

	return errors.As(err, &ae) && ae.Code == code
}

// The NextToken of a paged SELECT carries its LastEvaluatedKey, whose values
// are scalars: per attribute its name, its type and its value, each
// length-prefixed, in base64.
func encodeToken(key value.Item) *string {
	if key == nil {
		return nil
	}

	var b strings.Builder

	for name, v := range key {
		var text string

		switch {
		case v.S != nil:
			text = *v.S
		case v.N != nil:
			text = *v.N
		default:
			text = string(v.B)
		}

		for _, s := range []string{name, value.Type(&v), text} {
			b.WriteString(strconv.Itoa(len(s)) + ":" + s)
		}
	}

	return new(base64.RawURLEncoding.EncodeToString([]byte(b.String())))
}

// decodeToken is the key a NextToken carries.
func decodeToken(token *string) (value.Item, error) {
	if token == nil {
		return nil, nil //nolint:nilnil // no token, no start key
	}

	raw, err := base64.RawURLEncoding.DecodeString(*token)
	if err != nil {
		return nil, value.Invalid(msgInvalidToken)
	}

	key := value.Item{}

	for s := string(raw); s != ""; {
		var parts [3]string

		for i := range parts {
			parts[i], s, err = tokenField(s)
			if err != nil {
				return nil, err
			}
		}

		key[parts[0]], err = scalar(parts[1], parts[2])
		if err != nil {
			return nil, err
		}
	}

	if len(key) == 0 {
		return nil, value.Invalid(msgInvalidToken)
	}

	return key, nil
}

// tokenField reads one length-prefixed field from s.
func tokenField(s string) (string, string, error) {
	n, rest, ok := strings.Cut(s, ":")

	size, err := strconv.Atoi(n)
	if !ok || err != nil || size < 0 || size > len(rest) {
		return "", "", value.Invalid(msgInvalidToken)
	}

	return rest[:size], rest[size:], nil
}

func scalar(typ, text string) (api.AttributeValue, error) {
	switch typ {
	case value.TypeS:
		return api.AttributeValue{S: new(text)}, nil
	case value.TypeN:
		return api.AttributeValue{N: new(text)}, nil
	case value.TypeB:
		return api.AttributeValue{B: []byte(text)}, nil
	default:
		return api.AttributeValue{}, value.Invalid(msgInvalidToken)
	}
}
