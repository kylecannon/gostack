package dynamodb

import (
	"encoding/base64"
	"errors"
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

// selectChecks makes the checks the image makes of a SELECT before it looks its
// table up: the table's name, then an alias, then the paths of the list as
// one (SQ-418 c5). It renders the list into r.
func selectChecks(stmt *partiql.Statement, r *render) (*string, []partiql.Path, error) {
	if stmt.TableAliased {
		return nil, nil, value.Invalid(partiql.MsgAliasing)
	}

	err := statementName(stmt.Table)
	if err != nil {
		return nil, nil, err
	}

	err = aliasRefusal(stmt)
	if err != nil {
		return nil, nil, err
	}

	return projection(stmt.Projection, r)
}

// beforeTable is the answer to a statement whose table or key is refused, or
// that failed on a condition or update of its own: the error of the
// statement's own expressions when they have one, which the image names first
// (SQ-418 c2, c3, c5, p2a f2). They are rendered here whole and afresh, a
// SELECT's list first as the read renders it, so that every path to the error
// quotes the same text, with the same placeholders and operator count, whether
// the table is there or not. An INSERT holds none.
func beforeTable(stmt *partiql.Statement, params []api.AttributeValue, refused error) error {
	if stmt.Kind == partiql.Insert {
		return refused
	}

	r := newRender(params)

	var err error

	if stmt.Kind == partiql.Select {
		// Its list was checked already; this gives its placeholders to r.
		_, _, err = projection(stmt.Projection, r)
		if err != nil {
			return err
		}
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
	if err != nil {
		return err
	}

	return refused
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
