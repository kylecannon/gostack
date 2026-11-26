// Package partiql parses the PartiQL subset DynamoDB runs: SELECT, INSERT,
// UPDATE, DELETE and, in a transaction, EXISTS. It knows no tables; the
// dynamodb package compiles a parsed statement to the expressions and calls
// its other operations make.
package partiql

import (
	"strconv"
	"strings"
)

// StatementKind is the kind of a statement.
type StatementKind int

// The statement kinds.
const (
	Select StatementKind = iota
	Insert
	Update
	Delete
	Exists
)

// Returning is an UPDATE or DELETE statement's RETURNING clause.
type Returning int

// The RETURNING clauses: none, then ALL OLD *, MODIFIED OLD *, ALL NEW * and
// MODIFIED NEW *.
const (
	ReturnNone Returning = iota
	ReturnAllOld
	ReturnModifiedOld
	ReturnAllNew
	ReturnModifiedNew
)

// Statement is one parsed statement.
type Statement struct {
	Kind StatementKind
	// Table is the table the statement names, and Index the index a SELECT
	// reads, "" for the table itself.
	Table, Index string
	// Projection is a SELECT's attribute list, nil for *.
	Projection []Path
	// Where is the WHERE clause, nil when there is none.
	Where *Node
	// OrderBy is a SELECT's ORDER BY attribute, nil when there is none, and
	// Desc whether it sorts descending.
	OrderBy Path
	Desc    bool
	// Value is an INSERT's VALUE.
	Value *Node
	// Actions are an UPDATE's SET and REMOVE actions, in statement order.
	Actions   []Action
	Returning Returning
	// Params is how many ? parameters the statement holds.
	Params int
	// Aliased is set on a statement that names an alias, for a projected path
	// or the table. The image parses one and refuses it later, where the
	// caller puts it among the statement's other refusals (MsgAliasing).
	Aliased bool
	// TableAliased is set when the alias is the table's, which a SELECT refuses
	// before it checks the table's name, where one of its list follows the name.
	TableAliased bool
}

// Action is one SET or REMOVE of an UPDATE. Value is nil for a REMOVE.
type Action struct {
	Path  Path
	Value *Node
}

// Step is one step of a document path: an attribute name, or a list index
// when IsIndex.
type Step struct {
	Name    string
	Index   int
	IsIndex bool
}

// Path is a document path, its first step a name.
type Path []Step

// Alias is the name a projected path's value takes in a SELECT's result: the
// path from its last name on, as DynamoDB Local names it ("m.k" is "k",
// "l[1]" is "l[1]").
func (p Path) Alias() string {
	last := 0
	for i, s := range p {
		if !s.IsIndex {
			last = i
		}
	}

	var b strings.Builder

	b.WriteString(p[last].Name)

	for _, s := range p[last+1:] {
		b.WriteString("[" + strconv.Itoa(s.Index) + "]")
	}

	return b.String()
}

// Kind is the kind of an expression node.
type Kind int

// The node kinds.
const (
	// KindPath is a document path, in Path.
	KindPath Kind = iota
	// KindLiteral is a scalar literal or NULL, in Value.
	KindLiteral
	// KindMissing is the MISSING literal.
	KindMissing
	// KindParam is the ?, Param counting from 0.
	KindParam
	// KindTuple is {'k': v, ...}: Keys and KeyAt name the Args.
	KindTuple
	// KindList is [v, ...] in Args.
	KindList
	// KindBag is <<v, ...>> in Args.
	KindBag
	// KindCompare is Args[0] Op Args[1], Op one of = <> < <= > >=.
	KindCompare
	// KindArith is Args[0] Op Args[1], Op + or -.
	KindArith
	// KindAnd and KindOr join Args[0] and Args[1].
	KindAnd
	KindOr
	// KindNot negates Args[0].
	KindNot
	// KindBetween is Args[0] BETWEEN Args[1] AND Args[2].
	KindBetween
	// KindIn is Args[0] IN Args[1:].
	KindIn
	// KindIsNull and KindIsMissing are Args[0] IS [NOT] NULL and MISSING,
	// Negated for NOT.
	KindIsNull
	KindIsMissing
	// KindCall is the function Name over Args.
	KindCall
)

// Node is one expression node.
type Node struct {
	Kind Kind
	At   Pos
	// Text is the token as written, for messages.
	Text    string
	Op      string
	Name    string
	Path    Path
	Literal Literal
	Param   int
	Keys    []string
	KeyAt   []Pos
	Args    []*Node
	Negated bool
}

// LiteralKind is the type of a scalar literal.
type LiteralKind int

// The scalar literal types.
const (
	LitString LiteralKind = iota
	LitNumber
	LitBool
	LitNull
)

// Literal is a scalar literal: Text holds a string or a number as written,
// Bool a boolean.
type Literal struct {
	Kind LiteralKind
	Text string
	Bool bool
}
