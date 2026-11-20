package value

import (
	"bytes"
	"net/http"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
)

// Item is a DynamoDB item: attribute values by name.
type Item = map[string]api.AttributeValue

// The type names of the attribute values, as AttributeDefinition and the wire
// spell them.
const (
	TypeS    = "S"
	TypeN    = "N"
	TypeB    = "B"
	TypeSS   = "SS"
	TypeNS   = "NS"
	TypeBS   = "BS"
	TypeM    = "M"
	TypeL    = "L"
	TypeNULL = "NULL"
	TypeBOOL = "BOOL"
)

// The messages DynamoDB Local answers for a value it refuses, as LocalStack's
// image returns them. Each was observed against gostack-parity-localstack.
const (
	msgEmptyValue = "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes"
	msgTwoTypes   = "Supplied AttributeValue has more than one datatypes set, " +
		"must contain exactly one of the supported datatypes"
	msgEmptySS     = "One or more parameter values were invalid: An string set  may not be empty"
	msgEmptyNS     = "One or more parameter values were invalid: An number set  may not be empty"
	msgEmptyBS     = "One or more parameter values were invalid: Binary sets should not be empty"
	msgDuplicateNS = "Input collection contains duplicates"
	msgDuplicateBS = "One or more parameter values were invalid: Input collection of type BS contains duplicates."
	msgNullNotTrue = "One or more parameter values were invalid: Null attribute value types must have the value of true"
	msgEmptyName   = "Empty attribute name"
	msgTooDeep     = "Nesting Levels have exceeded supported limits: " +
		"Attributes in the item have nested levels beyond supported limit"
	invalidPrefix   = "One or more parameter values were invalid: "
	duplicatePrefix = invalidPrefix + "Input collection "
)

// MaxDepth is the most levels a value may nest, the value itself and every
// map or list around a value inside it counted: DynamoDB refuses a value whose
// innermost member sits 33 levels down. Validate refuses deeper values before
// walking them, so every later walk of a validated value is bounded.
const MaxDepth = 32

// MaxItemSize is the largest item DynamoDB stores, in the bytes ItemSize
// counts.
const MaxItemSize = 400 * 1024

// Invalid returns DynamoDB's ValidationException with msg.
func Invalid(msg string) *awserr.Error {
	return &awserr.Error{
		Code:       "ValidationException",
		Message:    msg,
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}

// Type returns the name of the one member v sets, or "" when it sets none.
func Type(v *api.AttributeValue) string {
	switch {
	case v.S != nil:
		return TypeS
	case v.N != nil:
		return TypeN
	case v.B != nil:
		return TypeB
	case v.SS != nil:
		return TypeSS
	case v.NS != nil:
		return TypeNS
	case v.BS != nil:
		return TypeBS
	case v.M != nil:
		return TypeM
	case v.L != nil:
		return TypeL
	case v.NULL != nil:
		return TypeNULL
	case v.BOOL != nil:
		return TypeBOOL
	default:
		return ""
	}
}

// ValidateItem checks every attribute of item with Validate, and that no name
// is empty. It puts the numbers in canonical form in place.
func ValidateItem(item Item) error {
	return checkItem(item, 1)
}

func checkItem(item Item, depth int) error {
	for name, v := range item {
		if name == "" {
			return Invalid(msgEmptyName)
		}

		err := checkValue(&v, depth)
		if err != nil {
			return err
		}

		item[name] = v
	}

	return nil
}

// Validate checks v as DynamoDB does: exactly one member, a NULL that is true,
// numbers DynamoDB can hold, sets that are not empty and hold no duplicate, and
// the same of every value inside a map or list, nested no deeper than MaxDepth.
// It rewrites every number in v to its canonical form.
func Validate(v *api.AttributeValue) error {
	return checkValue(v, 1)
}

func checkValue(v *api.AttributeValue, depth int) error {
	if depth > MaxDepth {
		return Invalid(msgTooDeep)
	}

	switch v.MembersSet() {
	case 0:
		return Invalid(msgEmptyValue)
	case 1:
	default:
		return Invalid(msgTwoTypes)
	}

	switch Type(v) {
	case TypeN:
		return canonicalize(v.N)
	case TypeSS:
		return validateSS(v.SS)
	case TypeNS:
		return validateNS(v.NS)
	case TypeBS:
		return validateBS(v.BS)
	case TypeM:
		return checkItem(v.M, depth+1)
	case TypeL:
		return validateList(v.L, depth+1)
	case TypeNULL:
		if !*v.NULL {
			return Invalid(msgNullNotTrue)
		}
	default:
	}

	return nil
}

func canonicalize(s *string) error {
	n, err := ParseNumber(*s)
	if err != nil {
		return err
	}

	*s = n.String()

	return nil
}

func validateList(list []api.AttributeValue, depth int) error {
	for i := range list {
		err := checkValue(&list[i], depth)
		if err != nil {
			return err
		}
	}

	return nil
}

func validateSS(set []string) error {
	if len(set) == 0 {
		return Invalid(msgEmptySS)
	}

	seen := make(map[string]struct{}, len(set))
	for _, s := range set {
		if _, dup := seen[s]; dup {
			return Invalid(duplicatePrefix + "[" + strings.Join(set, ", ") + "] contains duplicates")
		}

		seen[s] = struct{}{}
	}

	return nil
}

func validateNS(set []string) error {
	if len(set) == 0 {
		return Invalid(msgEmptyNS)
	}

	seen := make(map[string]struct{}, len(set))

	for i := range set {
		err := canonicalize(&set[i])
		if err != nil {
			return err
		}

		if _, dup := seen[set[i]]; dup {
			return Invalid(msgDuplicateNS)
		}

		seen[set[i]] = struct{}{}
	}

	return nil
}

func validateBS(set [][]byte) error {
	if len(set) == 0 {
		return Invalid(msgEmptyBS)
	}

	seen := make(map[string]struct{}, len(set))
	for _, b := range set {
		if _, dup := seen[string(b)]; dup {
			return Invalid(msgDuplicateBS)
		}

		seen[string(b)] = struct{}{}
	}

	return nil
}

// Compare orders two key values of the same scalar type as DynamoDB sorts
// them: strings and binaries by their bytes, numbers by value. The numbers must
// be canonical, as Validate leaves them.
func Compare(a, b *api.AttributeValue) int {
	switch {
	case a.S != nil && b.S != nil:
		return strings.Compare(*a.S, *b.S)
	case a.B != nil && b.B != nil:
		return bytes.Compare(a.B, b.B)
	case a.N != nil && b.N != nil:
		x, _ := ParseNumber(*a.N)
		y, _ := ParseNumber(*b.N)

		return x.Compare(y)
	default:
		return strings.Compare(Type(a), Type(b))
	}
}

// AppendKeyString appends KeyString(v) to dst, for a lookup that allocates
// nothing.
func AppendKeyString(dst []byte, v *api.AttributeValue) []byte {
	switch {
	case v.S != nil:
		return append(append(dst, 'S'), *v.S...)
	case v.N != nil:
		return append(append(dst, 'N'), *v.N...)
	case v.B != nil:
		return append(append(dst, 'B'), v.B...)
	default:
		return dst
	}
}

// KeyString encodes a scalar key value as a string that is equal for two
// values exactly when they are the same key: the type, then the bytes. The
// numbers must be canonical.
func KeyString(v *api.AttributeValue) string {
	switch {
	case v.S != nil:
		return "S" + *v.S
	case v.N != nil:
		return "N" + *v.N
	case v.B != nil:
		return "B" + string(v.B)
	default:
		return ""
	}
}

// Equal reports whether two validated values are the same: the same type and
// the same contents, sets in any order.
func Equal(a, b *api.AttributeValue) bool {
	t := Type(a)
	if t != Type(b) {
		return false
	}

	switch t {
	case TypeS:
		return *a.S == *b.S
	case TypeN:
		return *a.N == *b.N
	case TypeB:
		return bytes.Equal(a.B, b.B)
	case TypeSS, TypeNS:
		return sameStrings(a.SS, b.SS) && sameStrings(a.NS, b.NS)
	case TypeBS:
		return sameBytes(a.BS, b.BS)
	case TypeM:
		return EqualItems(a.M, b.M)
	case TypeL:
		return equalLists(a.L, b.L)
	case TypeNULL:
		return true
	default:
		return *a.BOOL == *b.BOOL
	}
}

// EqualItems reports whether two items hold the same attributes with Equal
// values.
func EqualItems(a, b Item) bool {
	if len(a) != len(b) {
		return false
	}

	for name, av := range a {
		bv, ok := b[name]
		if !ok || !Equal(&av, &bv) {
			return false
		}
	}

	return true
}

func equalLists(a, b []api.AttributeValue) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !Equal(&a[i], &b[i]) {
			return false
		}
	}

	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}

	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
	}

	return true
}

func sameBytes(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}

	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[string(s)] = struct{}{}
	}

	for _, s := range b {
		if _, ok := set[string(s)]; !ok {
			return false
		}
	}

	return true
}

// ItemSize is the size of item as DynamoDB counts it against MaxItemSize: for
// each attribute, the bytes of its name and the size of its value.
func ItemSize(item Item) int {
	total := 0
	for name, v := range item {
		total += len(name) + Size(&v)
	}

	return total
}

// Size is the size of one validated value as DynamoDB counts it: the bytes of a
// string or binary, one byte per two significant digits of a number plus one,
// one byte for a boolean or null, the sum of a set's elements, and three bytes
// plus one per element and the elements' own sizes for a map or list.
func Size(v *api.AttributeValue) int {
	const (
		containerOverhead = 3
		elementOverhead   = 1
		oneByte           = 1
	)

	switch Type(v) {
	case TypeS:
		return len(*v.S)
	case TypeN:
		return numberSize(*v.N)
	case TypeB:
		return len(v.B)
	case TypeSS:
		return sumLen(v.SS)
	case TypeNS:
		total := 0
		for _, n := range v.NS {
			total += numberSize(n)
		}

		return total
	case TypeBS:
		total := 0
		for _, b := range v.BS {
			total += len(b)
		}

		return total
	case TypeM:
		return containerOverhead + len(v.M)*elementOverhead + ItemSize(v.M)
	case TypeL:
		total := containerOverhead + len(v.L)*elementOverhead
		for i := range v.L {
			total += Size(&v.L[i])
		}

		return total
	default:
		return oneByte
	}
}

func numberSize(s string) int {
	n, _ := ParseNumber(s)

	return n.size()
}

func sumLen(set []string) int {
	total := 0
	for _, s := range set {
		total += len(s)
	}

	return total
}
