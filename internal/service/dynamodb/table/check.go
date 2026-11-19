package table

import (
	"errors"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Key size limits, in bytes of the value.
const (
	maxHashKeyBytes  = 2048
	maxRangeKeyBytes = 1024
)

// The messages DynamoDB Local answers for a key or item it refuses, as
// LocalStack's image returns them.
const (
	msgKeyCount         = "The number of conditions on the keys is invalid"
	msgKeyMissing       = "One of the required keys was not given a value"
	msgKeyNotScalar     = "Invalid attribute value type"
	msgKeyTypeMismatch  = "One or more parameter values were invalid: Type mismatch for key"
	msgIndexKeyMismatch = "One or more parameter values were invalid: Type mismatch for Index Key"
	msgKeyTooLong       = "Hash primary key values must be under 2048 bytes, " +
		"and range primary key values must be under 1024 bytes"
	msgItemTooLarge   = "Item size has exceeded the maximum allowed size"
	msgEmptyKeyPrefix = "One or more parameter values are not valid. "
	msgEmptyIndexKey  = "A value specified for a secondary index key is not supported. "
)

// KeyTypeMismatch returns the message of CheckKey's or CheckItem's refusal of
// a table or index key value whose type is not the key attribute's, which a
// transaction reports as one action's cancellation reason rather than
// refusing the request, or "" when err is another error.
func KeyTypeMismatch(err error) string {
	var e *awserr.Error
	if errors.As(err, &e) && (e.Message == msgKeyTypeMismatch || e.Message == msgIndexKeyMismatch) {
		return e.Message
	}

	return ""
}

// CheckKey checks that key names exactly the table's key attributes, each with
// a value of its type, as GetItem and DeleteItem take it. The values must have
// passed value.Validate.
func (t *Table) CheckKey(key value.Item) error {
	want := 1
	if t.key.HasRange() {
		want = 2
	}

	if len(key) != want {
		return value.Invalid(msgKeyCount)
	}

	for _, attr := range t.keyAttributes() {
		v, ok := key[attr.Name]
		if !ok {
			return value.Invalid(msgKeyMissing)
		}

		if t := value.Type(&v); t != value.TypeS && t != value.TypeN && t != value.TypeB {
			return value.Invalid(msgKeyNotScalar)
		}
	}

	return t.checkKeyValues(key)
}

// CheckItem checks a whole item before it is stored: every key attribute of
// the table present with its type and a value DynamoDB accepts, every index key
// attribute it carries of the index's type and not empty, and the item's size.
// The values must have passed value.Validate. It reads the table's indexes
// under its read lock, since UpdateTable adds and drops them.
func (t *Table) CheckItem(item value.Item) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.CheckItemHeld(item)
}

// CheckItemHeld is CheckItem for a caller that holds the table's lock through
// Lock, ReadLock or a write.
func (t *Table) CheckItemHeld(item value.Item) error {
	for _, attr := range t.keyAttributes() {
		if _, ok := item[attr.Name]; !ok {
			return value.Invalid(msgKeyMissing)
		}
	}

	err := t.checkKeyValues(item)
	if err != nil {
		return err
	}

	for _, idx := range t.indexes {
		err := checkIndexKey(&idx.spec, item)
		if err != nil {
			return err
		}
	}

	if value.ItemSize(item) > value.MaxItemSize {
		return value.Invalid(msgItemTooLarge)
	}

	return nil
}

func (t *Table) keyAttributes() []KeyAttribute {
	if t.key.HasRange() {
		return []KeyAttribute{t.key.Hash, t.key.Range}
	}

	return []KeyAttribute{t.key.Hash}
}

// checkKeyValues checks the type, emptiness and length of each key value in
// item, all of which are present.
func (t *Table) checkKeyValues(item value.Item) error {
	for i, attr := range t.keyAttributes() {
		v := item[attr.Name]
		if value.Type(&v) != attr.Type {
			return value.Invalid(msgKeyTypeMismatch)
		}

		if empty := emptyKind(&v); empty != "" {
			return value.Invalid(msgEmptyKeyPrefix + "The AttributeValue for a key attribute cannot contain an empty " +
				empty + " value. Key: " + attr.Name)
		}

		limit := maxHashKeyBytes
		if i == 1 {
			limit = maxRangeKeyBytes
		}

		if value.Size(&v) > limit {
			return value.Invalid(msgKeyTooLong)
		}
	}

	return nil
}

// checkIndexKey checks the index key attributes item carries: an item without
// them is simply not in the index.
func checkIndexKey(spec *IndexSpec, item value.Item) error {
	for _, attr := range []KeyAttribute{spec.Key.Hash, spec.Key.Range} {
		v, ok := item[attr.Name]
		if attr.Name == "" || !ok {
			continue
		}

		if value.Type(&v) != attr.Type {
			return value.Invalid(msgIndexKeyMismatch)
		}

		if empty := emptyKind(&v); empty != "" {
			return value.Invalid(msgEmptyKeyPrefix + msgEmptyIndexKey +
				"The AttributeValue for a key attribute cannot contain an empty " + empty +
				" value. IndexName: " + spec.Name + ", IndexKey: " + attr.Name)
		}
	}

	return nil
}

// emptyKind names the kind of an empty string or binary key value, or returns
// "" for a value that is not empty.
func emptyKind(v *api.AttributeValue) string {
	switch {
	case v.S != nil && *v.S == "":
		return "string"
	case v.B != nil && len(v.B) == 0:
		return "binary"
	default:
		return ""
	}
}
