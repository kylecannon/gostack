package expression

import (
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The Actions of a legacy AttributeUpdates entry.
const (
	LegacyPut    = "PUT"
	LegacyAdd    = "ADD"
	LegacyDelete = "DELETE"
)

// LegacyUpdate is one entry of an UpdateItem's AttributeUpdates: the Action on
// the attribute named Name, with Value, nil for a DELETE of the whole
// attribute. The caller has checked it: the action, that Value suits it, and
// that it does not name a key attribute or change the type of the attribute
// that is stored. Value is a validated value.
type LegacyUpdate struct {
	Name   string
	Action string
	Value  *api.AttributeValue
}

// LegacyTypeName is the name DynamoDB gives a type in the messages it answers
// for an AttributeUpdates entry: STRING for TypeS, NUMBER_SET for TypeNS.
func LegacyTypeName(t string) string {
	return updateTypeNames[t]
}

// NewLegacyUpdate translates the entries of an AttributeUpdates into the update
// an UpdateExpression parses to, so one apply path serves both: PUT is a SET,
// DELETE with no value a REMOVE, ADD of a number or a set and DELETE of a set
// the clauses of those names. ADD of a list, which only an AttributeUpdates
// takes, appends to the list stored, or sets the attribute when there is none:
// it is a SET of list_append, or of the value, as old, the item before the
// update, says. A name is one attribute, whatever it holds: it is never read
// as a document path.
func NewLegacyUpdate(updates []LegacyUpdate, old value.Item) *UpdateExpr {
	u := &UpdateExpr{
		actions: make([]*updateAction, 0, len(updates)),
		paths:   make([]Path, 0, len(updates)),
	}

	for i := range updates {
		act := legacyAction(&updates[i], old)

		u.actions = append(u.actions, act)
		u.paths = append(u.paths, act.target.path)
	}

	u.order = updateOrder(u.actions)

	return u
}

func legacyAction(up *LegacyUpdate, old value.Item) *updateAction {
	target := &updatePathExpr{path: Path{{Name: up.Name}}}
	act := &updateAction{clause: updateSet, target: target}

	if up.Value == nil {
		act.clause = updateRemove

		return act
	}

	val := &updateOperand{kind: updateOpValue, val: *up.Value}
	act.rhs = val

	switch {
	case up.Action == LegacyDelete:
		act.clause = updateDelete
	case up.Action != LegacyAdd:
	case up.Value.L == nil:
		act.clause = updateAdd
	default:
		if _, stored := old[up.Name]; stored {
			act.rhs = &updateOperand{
				kind: updateOpCall,
				tok:  token{kind: tokIdent, text: funcListAppend},
				args: []*updateOperand{{kind: updateOpPath, path: target}, val},
			}
		}
	}

	return act
}
