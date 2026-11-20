package dynamodb

import (
	"fmt"
	"maps"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for an AttributeUpdates entry it
// refuses, as the LocalStack image returns them (verification/SQ-329).
const (
	msgUpdateAction   = "Member must satisfy enum value set: [ADD, DELETE, PUT]"
	msgUpdateNoValue  = msgInvalid + "Only DELETE action is allowed when no attribute value is specified"
	msgUpdateAddType  = msgInvalid + "ADD action is not supported for the type %s"
	msgUpdateDelType  = msgInvalid + "DELETE action with value is not supported for the type %s"
	msgUpdateMismatch = "Type mismatch for attribute to update"
	msgUpdateKey      = msgInvalid + "Cannot update attribute %s. This attribute is part of the key"
)

// legacyUpdate checks the entries of an UpdateItem's AttributeUpdates against
// the key of the item and the item stored, old, and translates them into an
// update. The image checks the entries in the request's order, which the
// decoded map does not keep; they are checked in attribute name order, and
// each as a whole before the next: the key, the name, the Action, the value,
// what the Action takes of it, and last the type of what is stored.
func legacyUpdate(updates map[string]api.AttributeValueUpdate, key, old value.Item) (*expression.UpdateExpr, error) {
	entries := make([]expression.LegacyUpdate, 0, len(updates))

	for _, name := range slices.Sorted(maps.Keys(updates)) {
		au := updates[name]

		entry, err := legacyUpdateEntry(name, &au, key, old)
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	return expression.NewLegacyUpdate(entries, old), nil
}

func legacyUpdateEntry(
	name string, au *api.AttributeValueUpdate, key, old value.Item,
) (expression.LegacyUpdate, error) {
	entry := expression.LegacyUpdate{Name: name, Action: expression.LegacyPut}

	if _, isKey := key[name]; isKey {
		return entry, value.Invalid(fmt.Sprintf(msgUpdateKey, name))
	}

	if name == "" {
		return entry, value.Invalid(msgAttrEmpty)
	}

	// The model documents PUT as the Action of an entry that has none; the
	// image checks such an entry and then leaves the item alone.
	if au.Action != nil {
		entry.Action = *au.Action
	}

	switch entry.Action {
	case expression.LegacyPut, expression.LegacyAdd, expression.LegacyDelete:
	default:
		return entry, value.Invalid(msgUpdateAction)
	}

	if au.Value != nil {
		v := *au.Value

		err := value.Validate(&v)
		if err != nil {
			return entry, fmt.Errorf("attribute update: %w", err)
		}

		entry.Value = &v
	}

	return entry, checkLegacyUpdateValue(&entry, old[name])
}

// checkLegacyUpdateValue checks what an Action takes of its value: PUT any,
// ADD a number, a set or a list, DELETE none or a set; and that ADD and
// DELETE leave the attribute stored, cur, the type it is.
func checkLegacyUpdateValue(entry *expression.LegacyUpdate, cur api.AttributeValue) error {
	if entry.Value == nil {
		if entry.Action != expression.LegacyDelete {
			return value.Invalid(msgUpdateNoValue)
		}

		return nil
	}

	t := value.Type(entry.Value)

	switch entry.Action {
	case expression.LegacyAdd:
		if t != value.TypeN && t != value.TypeL && !isSet(t) {
			return value.Invalid(fmt.Sprintf(msgUpdateAddType, expression.LegacyTypeName(t)))
		}
	case expression.LegacyDelete:
		if !isSet(t) {
			return value.Invalid(fmt.Sprintf(msgUpdateDelType, expression.LegacyTypeName(t)))
		}
	default:
		return nil
	}

	if stored := value.Type(&cur); stored != "" && stored != t {
		return value.Invalid(msgUpdateMismatch)
	}

	return nil
}

func isSet(t string) bool {
	return t == value.TypeSS || t == value.TypeNS || t == value.TypeBS
}
