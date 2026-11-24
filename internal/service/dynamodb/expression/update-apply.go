package expression

import (
	"bytes"
	"maps"
	"math/big"
	"slices"
	"strconv"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for an update it refuses while applying
// it to the item, as LocalStack's image returns them.
const (
	msgUpdateKey         = "One or more parameter values were invalid: Cannot update attribute "
	msgUpdateKeyTail     = ". This attribute is part of the key"
	msgUpdateInvalidPath = "The document path provided in the update expression is invalid for update"
	msgUpdateMissing     = "The provided expression refers to an attribute that does not exist in the item"
	msgUpdateType        = "An operand in the update expression has an incorrect data type"
	msgUpdateTooLarge    = "Item size to update has exceeded the maximum allowed size"
	msgUpdateTooDeep     = "Nesting Levels have exceeded supported limits"
)

// updateFinal is the last step of an action: it reads or changes the element
// el of parent, a map or list the update owns.
type updateFinal func(parent *api.AttributeValue, el PathElement) error

// ApplyStored returns the item an update makes of stored, the item the table
// holds at key, empty when there is none. It unpacks stored once, into an item
// that is its own, and that is the only item an update writes into: a map
// shared with anything else, such as a pooled Decoder.View, cannot be passed.
//
// read sees the unpacked item before anything is written. It returns the
// update to apply, nil for none, or an error that stops the write; a failed
// condition is one, and may keep the item to report it. read must not keep
// old unless it returns an error: the update then writes into it.
//
// Every right-hand side reads the item as stored, so SET a = b, b = a swaps
// the two. A list index names an element of the stored list: SET and ADD act
// first, a SET past the end appends, and REMOVE takes elements out last. A
// missing parent is never created. The update writes into the top-level map
// in place of a copy of it, but a map or list below an attribute is still
// copied before an action changes it, since a value the update read,
// SET a = b, can share it. With no update the item is returned as stored, or
// the key alone when nothing is.
func ApplyStored(
	key value.Item, stored value.Packed, read func(old value.Item) (*UpdateExpr, error),
) (value.Item, error) {
	item := value.Unpack(stored)

	u, err := read(item)
	if err != nil {
		return nil, err
	}

	if u == nil {
		if item != nil {
			return item, nil
		}

		return maps.Clone(key), nil
	}

	err = u.checkKey(key)
	if err != nil {
		return nil, err
	}

	if item == nil {
		item = make(value.Item, len(key))
	}

	return u.applyTo(key, item)
}

// checkKey refuses an update of an attribute of key.
func (u *UpdateExpr) checkKey(key value.Item) error {
	for _, path := range u.paths {
		if _, ok := key[path[0].Name]; ok {
			return value.Invalid(msgUpdateKey + path[0].Name + msgUpdateKeyTail)
		}
	}

	return nil
}

// applyTo evaluates the update against item and then writes it into item,
// which holds the stored item's attributes and is ApplyStored's own. On an
// error item may be part written.
func (u *UpdateExpr) applyTo(key, item value.Item) (value.Item, error) {
	// Every right-hand side is computed before the first write.
	results, err := u.evaluate(item)
	if err != nil {
		return nil, err
	}

	maps.Copy(item, key)

	root := api.AttributeValue{M: item}

	// Only an action below an attribute copies one, so an update of top-level
	// attributes allocates no record.
	var copied *copies

	for _, act := range u.order {
		if copied == nil && len(act.target.path) > 1 {
			copied = new(copies)
		}

		err = updateAt(&root, act.target.path, copied, updateFinalFor(act, results[act]))
		if err != nil {
			return nil, err
		}
	}

	err = u.checkNesting(item)
	if err != nil {
		return nil, err
	}

	if value.ItemSize(item) > value.MaxItemSize {
		return nil, value.Invalid(msgUpdateTooLarge)
	}

	return item, nil
}

// checkNesting refuses an item whose update pushed an attribute past
// value.MaxDepth levels: a value that fits an ExpressionAttributeValues can
// be one level too deep once it sits inside the item, and so can a list_append
// or a SET at a nested path. Only the attributes an action wrote can have
// grown, and the walk stops at the limit. An attribute is walked once however
// many actions wrote below it: 1,000 SET actions under one map walked it 1,000
// times (SQ-452).
func (u *UpdateExpr) checkNesting(item value.Item) error {
	var walked map[string]struct{}

	for _, act := range u.order {
		if act.clause == updateRemove || act.clause == updateDelete {
			continue
		}

		name := act.target.path[0].Name

		// A path of one element names an attribute no other action writes
		// below, since no path is a prefix of another.
		if len(act.target.path) > 1 {
			if _, done := walked[name]; done {
				continue
			}

			if walked == nil {
				walked = make(map[string]struct{})
			}

			walked[name] = struct{}{}
		}

		v := item[name]
		if value.TooDeep(&v) {
			return value.Invalid(msgUpdateTooDeep)
		}
	}

	return nil
}

// evaluate computes the value of every SET right-hand side against old. Every
// path an operand reads must be in old, except the first operand of
// if_not_exists; that is checked for all of them before any is computed.
func (u *UpdateExpr) evaluate(old value.Item) (map[*updateAction]api.AttributeValue, error) {
	for _, act := range u.actions {
		if act.clause == updateSet && !updateOperandPathsExist(act.rhs, old) {
			return nil, value.Invalid(msgUpdateMissing)
		}
	}

	results := make(map[*updateAction]api.AttributeValue, len(u.actions))

	for _, act := range u.actions {
		switch act.clause {
		case updateRemove:
			continue
		case updateAdd, updateDelete:
			results[act] = act.rhs.val

			continue
		default:
		}

		v, err := updateEval(act.rhs, old)
		if err != nil {
			return nil, err
		}

		results[act] = v
	}

	return results, nil
}

func updateOperandPathsExist(op *updateOperand, old value.Item) bool {
	switch op.kind {
	case updateOpPath:
		_, ok := updateGet(old, op.path.path)

		return ok
	case updateOpCall:
		args := op.args
		if op.tok.text == funcIfNotExists {
			args = args[1:]
		}

		for _, arg := range args {
			if !updateOperandPathsExist(arg, old) {
				return false
			}
		}
	case updateOpPlus, updateOpMinus:
		return updateOperandPathsExist(op.args[0], old) && updateOperandPathsExist(op.args[1], old)
	default:
	}

	return true
}

// updateEval computes an operand against old.
func updateEval(op *updateOperand, old value.Item) (api.AttributeValue, error) {
	switch op.kind {
	case updateOpValue:
		return op.val, nil
	case updateOpPath:
		v, ok := updateGet(old, op.path.path)
		if !ok {
			return api.AttributeValue{}, value.Invalid(msgUpdateMissing)
		}

		return v, nil
	case updateOpCall:
		if op.tok.text == funcIfNotExists {
			if v, ok := updateGet(old, op.args[0].path.path); ok {
				return v, nil
			}

			return updateEval(op.args[1], old)
		}

		return updateEvalPair(op, old)
	default:
		return updateEvalPair(op, old)
	}
}

// updateEvalPair computes list_append, + or - over its two operands.
func updateEvalPair(op *updateOperand, old value.Item) (api.AttributeValue, error) {
	a, err := updateEval(op.args[0], old)
	if err != nil {
		return api.AttributeValue{}, err
	}

	b, err := updateEval(op.args[1], old)
	if err != nil {
		return api.AttributeValue{}, err
	}

	if op.kind == updateOpCall {
		if a.L == nil || b.L == nil {
			return api.AttributeValue{}, value.Invalid(msgUpdateType)
		}

		return api.AttributeValue{L: append(append(make([]api.AttributeValue, 0, len(a.L)+len(b.L)), a.L...), b.L...)}, nil
	}

	if a.N == nil || b.N == nil {
		return api.AttributeValue{}, value.Invalid(msgUpdateType)
	}

	b2 := *b.N
	if op.kind == updateOpMinus {
		b2 = updateNegate(b2)
	}

	n, err := updateArith(*a.N, b2)
	if err != nil {
		return api.AttributeValue{}, err
	}

	return api.AttributeValue{N: &n}, nil
}

// updateArith returns a+b in canonical form. The numbers
// are canonical, as validation leaves them. A result DynamoDB cannot hold is
// refused with ParseNumber's message.
func updateArith(a, b string) (string, error) {
	x, xe := updateBig(a)
	y, ye := updateBig(b)

	exp := min(xe, ye)
	ten := big.NewInt(decimalBase)

	x.Mul(x, new(big.Int).Exp(ten, big.NewInt(int64(xe-exp)), nil))
	y.Mul(y, new(big.Int).Exp(ten, big.NewInt(int64(ye-exp)), nil))

	n, err := value.ParseNumber(x.Add(x, y).String() + "E" + strconv.Itoa(exp))
	if err != nil {
		return "", err //nolint:wrapcheck // the ValidationException is the reply itself
	}

	return n.String(), nil
}

// updateBig returns a canonical number as an integer and the power of ten it
// is scaled by.
func updateBig(s string) (*big.Int, int) {
	n, _ := value.ParseNumber(s)

	i, _ := new(big.Int).SetString("0"+n.Digits, decimalBase)
	if n.Neg {
		i.Neg(i)
	}

	return i, n.Exp
}

// updateNegate returns the canonical number n negated.
func updateNegate(n string) string {
	switch {
	case n == "0":
		return n
	case n[0] == '-':
		return n[1:]
	default:
		return "-" + n
	}
}

// updateFinalFor returns the last step of act, which writes v.
func updateFinalFor(act *updateAction, v api.AttributeValue) updateFinal {
	switch act.clause {
	case updateSet:
		return func(parent *api.AttributeValue, el PathElement) error {
			return updatePut(parent, el, v)
		}
	case updateRemove:
		return updateRemoveAt
	case updateAdd:
		return func(parent *api.AttributeValue, el PathElement) error {
			return updateAddAt(parent, el, v)
		}
	default:
		return func(parent *api.AttributeValue, el PathElement) error {
			return updateDeleteAt(parent, el, v)
		}
	}
}

// copies records, by the path to each, the maps and lists an ApplyStored has already
// copied, so that the actions under one of them change the copy and a thousand
// of them under one map copy it once and not a thousand times (SQ-452). No
// action path is a prefix of another (updateCheckOverlap), so a copy is never
// replaced by a later action: what a path names is the copy its first action
// made. The zero value is an empty record.
type copies struct {
	done bool
	next map[PathElement]*copies
}

// at is the record of the element el below c.
func (c *copies) at(el PathElement) *copies {
	n, ok := c.next[el]
	if !ok {
		if c.next == nil {
			c.next = make(map[PathElement]*copies)
		}

		n = new(copies)
		c.next[el] = n
	}

	return n
}

// updateAt walks path from parent, which the update owns, copying every map or
// list it passes through that copied does not already hold, so old is never
// changed, and runs final on the last step. A step through a missing value, or
// one of the wrong kind, is refused.
func updateAt(parent *api.AttributeValue, path Path, copied *copies, final updateFinal) error {
	if len(path) == 1 {
		return final(parent, path[0])
	}

	child, ok := updateChild(parent, path[0])
	if !ok {
		return value.Invalid(msgUpdateInvalidPath)
	}

	own := copied.at(path[0])

	switch {
	case path[1].Name != "" && child.M != nil:
		if !own.done {
			child = api.AttributeValue{M: maps.Clone(child.M)}
		}
	case path[1].Name == "" && child.L != nil:
		if !own.done {
			child = api.AttributeValue{L: slices.Clone(child.L)}
		}
	default:
		return value.Invalid(msgUpdateInvalidPath)
	}

	own.done = true

	err := updateAt(&child, path[1:], own, final)
	if err != nil {
		return err
	}

	return updatePut(parent, path[0], child)
}

// updateChild returns the element el of parent, and false when parent is not
// the kind el steps into or has no such element.
func updateChild(parent *api.AttributeValue, el PathElement) (api.AttributeValue, bool) {
	if el.Name != "" {
		v, ok := parent.M[el.Name]

		return v, ok && parent.M != nil
	}

	if el.Index < len(parent.L) {
		return parent.L[el.Index], true
	}

	return api.AttributeValue{}, false
}

// updateGet returns the value at path in item, and false when it is not there.
func updateGet(item value.Item, path Path) (api.AttributeValue, bool) {
	cur := api.AttributeValue{M: item}

	for _, el := range path {
		next, ok := updateChild(&cur, el)
		if !ok {
			return api.AttributeValue{}, false
		}

		cur = next
	}

	return cur, true
}

// updatePut sets the element el of parent to v. An index past the end of the
// list appends.
func updatePut(parent *api.AttributeValue, el PathElement, v api.AttributeValue) error {
	switch {
	case el.Name != "" && parent.M != nil:
		parent.M[el.Name] = v
	case el.Name == "" && parent.L != nil:
		if el.Index < len(parent.L) {
			parent.L[el.Index] = v
		} else {
			parent.L = append(parent.L, v)
		}
	default:
		return value.Invalid(msgUpdateInvalidPath)
	}

	return nil
}

// updateRemoveAt removes the element el of parent; a missing one is no change.
func updateRemoveAt(parent *api.AttributeValue, el PathElement) error {
	switch {
	case el.Name != "" && parent.M != nil:
		delete(parent.M, el.Name)
	case el.Name == "" && parent.L != nil:
		if el.Index < len(parent.L) {
			parent.L = slices.Delete(parent.L, el.Index, el.Index+1)
		}
	default:
		return value.Invalid(msgUpdateInvalidPath)
	}

	return nil
}

// updateAddAt adds the number v to the number at el, or the elements of the
// set v to the set at el, or sets el to v when it holds nothing.
func updateAddAt(parent *api.AttributeValue, el PathElement, v api.AttributeValue) error {
	cur, ok := updateChild(parent, el)
	if !ok {
		return updatePut(parent, el, v)
	}

	var sum api.AttributeValue

	switch {
	case cur.N != nil && v.N != nil:
		n, err := updateArith(*cur.N, *v.N)
		if err != nil {
			return err
		}

		sum.N = &n
	case cur.SS != nil && v.SS != nil:
		sum.SS = updateUnion(cur.SS, v.SS)
	case cur.NS != nil && v.NS != nil:
		sum.NS = updateUnion(cur.NS, v.NS)
	case cur.BS != nil && v.BS != nil:
		sum.BS = updateUnionBytes(cur.BS, v.BS)
	default:
		return value.Invalid(msgUpdateType)
	}

	return updatePut(parent, el, sum)
}

// updateDeleteAt takes the elements of the set v out of the set at el, and
// removes el when that empties it. A missing el is no change.
func updateDeleteAt(parent *api.AttributeValue, el PathElement, v api.AttributeValue) error {
	cur, ok := updateChild(parent, el)
	if !ok {
		if (el.Name != "" && parent.M == nil) || (el.Name == "" && parent.L == nil) {
			return value.Invalid(msgUpdateInvalidPath)
		}

		return nil
	}

	var rest api.AttributeValue

	switch {
	case cur.SS != nil && v.SS != nil:
		rest.SS = updateMinus(cur.SS, v.SS)
	case cur.NS != nil && v.NS != nil:
		rest.NS = updateMinus(cur.NS, v.NS)
	case cur.BS != nil && v.BS != nil:
		rest.BS = slices.DeleteFunc(slices.Clone(cur.BS), func(b []byte) bool {
			return slices.ContainsFunc(v.BS, func(d []byte) bool { return bytes.Equal(b, d) })
		})
	default:
		return value.Invalid(msgUpdateType)
	}

	if len(rest.SS)+len(rest.NS)+len(rest.BS) == 0 {
		return updateRemoveAt(parent, el)
	}

	return updatePut(parent, el, rest)
}

// updateUnion returns a with the elements of b it lacks appended.
func updateUnion(a, b []string) []string {
	out := slices.Clip(slices.Clone(a))
	for _, s := range b {
		if !slices.Contains(a, s) {
			out = append(out, s)
		}
	}

	return out
}

func updateUnionBytes(a, b [][]byte) [][]byte {
	out := slices.Clip(slices.Clone(a))
	for _, s := range b {
		if !slices.ContainsFunc(a, func(x []byte) bool { return bytes.Equal(x, s) }) {
			out = append(out, s)
		}
	}

	return out
}

// updateMinus returns a without the elements of b.
func updateMinus(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(s string) bool { return slices.Contains(b, s) })
}
