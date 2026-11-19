package dynamodb

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for a transaction it refuses, as the
// LocalStack image returns them.
const (
	msgTransactNull  = "Value null at 'transactItems' failed to satisfy constraint: Member must not be null"
	msgTransactEmpty = "Value [] at 'transactItems' failed to satisfy constraint: " +
		"Member must have length greater than or equal to 1"
	msgTransactTooMany  = "Member must have length less than or equal to 100"
	msgTransactTokenLen = "Value %s at 'clientRequestToken' failed to satisfy constraint: " +
		"Member must have length less than or equal to 36"
	msgTransactNoOp    = "Invalid Request: TransactWriteRequest should contain Delete or Put or Update request"
	msgTransactTwoOps  = "TransactItems can only contain one of Check, Put, Update or Delete"
	msgTransactPayload = "Transaction payload size cannot exceed 4MB. Payload Size: "
	msgTransactMember  = "Value null at 'transactItems.%d.member.%s' failed to satisfy constraint: " +
		"Member must not be null"
	msgTransactSameItem = "Transaction request cannot include multiple operations on one item"
	msgTransactCanceled = "Transaction cancelled, please refer cancellation reasons for specific reasons ["
	msgUpdateKeyAttr    = "One or more parameter values were invalid: Cannot update attribute %s. " +
		"This attribute is part of the key"
	msgConditionCheckFailed = msgConditionFailed + "."
	msgKeyTypeMismatch      = "One or more parameter values were invalid: Type mismatch for key"
)

// transactUnits is how many times the units of a plain read or write a
// transactional one costs.
const transactUnits = 2

// The cancellation reason codes.
const (
	reasonNone       = "None"
	reasonCondition  = "ConditionalCheckFailed"
	reasonValidation = "ValidationError"
)

// The four kinds of transaction action, named as the model's members are.
const (
	opPut            = "put"
	opUpdate         = "update"
	opDelete         = "delete"
	opConditionCheck = "conditionCheck"
)

// txRequest is one action of a TransactWriteItems as sent, whatever its kind.
type txRequest struct {
	op        string
	table     *string
	item      map[string]api.AttributeValue // Put's Item, or the others' Key
	condition *string
	update    *string
	names     map[string]string
	values    map[string]api.AttributeValue
	onFailure *string
}

// txRequestOf returns the one action item holds, and how many it holds.
func txRequestOf(item *api.TransactWriteItem) (txRequest, int) {
	var r txRequest

	n := 0

	if p := item.Put; p != nil {
		n++
		r = txRequest{
			opPut, p.TableName, p.Item, p.ConditionExpression, nil,
			p.ExpressionAttributeNames, p.ExpressionAttributeValues, p.ReturnValuesOnConditionCheckFailure,
		}
	}

	if u := item.Update; u != nil {
		n++
		r = txRequest{
			opUpdate, u.TableName, u.Key, u.ConditionExpression, u.UpdateExpression,
			u.ExpressionAttributeNames, u.ExpressionAttributeValues, u.ReturnValuesOnConditionCheckFailure,
		}
	}

	if d := item.Delete; d != nil {
		n++
		r = txRequest{
			opDelete, d.TableName, d.Key, d.ConditionExpression, nil,
			d.ExpressionAttributeNames, d.ExpressionAttributeValues, d.ReturnValuesOnConditionCheckFailure,
		}
	}

	if c := item.ConditionCheck; c != nil {
		n++
		r = txRequest{
			opConditionCheck, c.TableName, c.Key, c.ConditionExpression, nil,
			c.ExpressionAttributeNames, c.ExpressionAttributeValues, c.ReturnValuesOnConditionCheckFailure,
		}
	}

	return r, n
}

// payload is what the action counts toward the 4 MB transaction limit: its
// item or key, its expressions' text, and its values.
func (r *txRequest) payload() int {
	n := value.ItemSize(r.item)
	if r.update != nil {
		n += len(*r.update)
	}

	if r.condition != nil {
		n += len(*r.condition)
	}

	for _, v := range r.values {
		n += value.Size(&v)
	}

	return n
}

// txAction is one checked action of a transaction.
type txAction struct {
	op    string
	name  string
	entry *tableEntry
	// key locates the item: Put's whole item, or the others' key.
	key       value.Item
	update    *expression.UpdateExpr
	condition expression.Condition
	returnOld bool
	// keyMismatch is a key value of the wrong type, which cancels the
	// transaction with this action's ValidationError rather than refusing it.
	keyMismatch string
}

// txResult is what one action does once every condition holds.
type txResult struct {
	old, new value.Item
	write    bool
}

// TransactWriteItems applies up to 100 puts, updates, deletes and condition
// checks across tables, all or none. Every condition is evaluated against one
// state of the tables, held under their locks, and a transaction any of them
// fails is cancelled with a reason per action, in action order.
// A ClientRequestToken makes a repeat of a successful request within ten
// minutes a no-op.
func (p *Provider) TransactWriteItems(
	ctx context.Context, in *api.TransactWriteItemsInput,
) (*api.TransactWriteItemsOutput, error) {
	err := checkTransactShape(in)
	if err != nil {
		return nil, err
	}

	var request [32]byte
	if in.ClientRequestToken != nil {
		request = transactFingerprint(in.TransactItems)
	}

	c := p.caller(ctx)

	actions, err := c.checkTransactWrite(in.TransactItems)
	if err != nil {
		return nil, err
	}

	used, err := p.runTransaction(c, in.ClientRequestToken, request, actions)
	if err != nil {
		return nil, err
	}

	return &api.TransactWriteItemsOutput{ConsumedCapacity: used.report(in.ReturnConsumedCapacity)}, nil
}

// runTransaction takes the locks of every table the actions touch, settles the
// request's token, and runs the actions.
func (p *Provider) runTransaction(
	c caller, token *string, request [32]byte, actions []*txAction,
) (tableConsumption, error) {
	engines := make([]*table.Table, len(actions))
	for i, a := range actions {
		engines[i] = a.entry.data
	}

	// table.Lock takes the tables' locks in the one global order every
	// multi-table lock follows, the order the tables were created in, so two
	// transactions over the same tables in opposite orders cannot deadlock.
	unlock := table.Lock(engines...)
	defer unlock()

	if token != nil {
		state, used := c.state.tokens.begin(*token, request, p.now())

		switch state {
		case tokenReplay:
			return used, nil
		case tokenMismatch:
			return nil, api.NewIdempotentParameterMismatchException("")
		default: // tokenNew: the transaction runs.
		}
	}

	results, err := applyTransaction(actions)
	if err != nil {
		if token != nil {
			c.state.tokens.abort(*token)
		}

		return nil, err
	}

	used := transactCapacity(actions, results)

	if token != nil {
		c.state.tokens.finish(*token, p.now(), used)
	}

	return used, nil
}

// applyTransaction decides every action against the items stored now, and
// writes them all only when every condition holds. The caller holds the
// tables' locks.
func applyTransaction(actions []*txAction) ([]txResult, error) {
	results := make([]txResult, len(actions))
	reasons := make([]api.CancellationReason, len(actions))
	cancelled := false

	for i, a := range actions {
		reason, result, err := a.decide()
		if err != nil {
			return nil, err
		}

		reasons[i], results[i] = reason, result
		cancelled = cancelled || *reason.Code != reasonNone
	}

	if cancelled {
		return nil, transactionCanceled(reasons)
	}

	for i, a := range actions {
		if !results[i].write {
			continue
		}

		_, err := a.entry.data.WriteHeld(a.key, func(value.Item) (value.Item, error) { return results[i].new, nil })
		if err != nil {
			undo(actions[:i], results)

			return nil, fmt.Errorf("transaction: %w", err)
		}
	}

	return results, nil
}

// undo puts back the items the first actions wrote, last first, after a write
// failed partway. Every new item was checked before the first write, so this
// is a guard, not a path a request takes.
func undo(actions []*txAction, results []txResult) {
	for i, a := range slices.Backward(actions) {
		if results[i].write {
			_, _ = a.entry.data.WriteHeld(a.key, func(value.Item) (value.Item, error) {
				return results[i].old, nil
			})
		}
	}
}

// decide evaluates one action against the stored item: its cancellation
// reason, and what it writes when the transaction goes ahead. An error refuses
// the whole request.
func (a *txAction) decide() (api.CancellationReason, txResult, error) {
	if a.keyMismatch != "" {
		return cancellation(reasonValidation, a.keyMismatch, nil), txResult{}, nil
	}

	old := a.entry.data.GetHeld(a.key)
	result := txResult{old: old}

	if a.condition != nil {
		ok, err := a.condition.Eval(old)
		if err != nil {
			return api.CancellationReason{}, txResult{}, fmt.Errorf("condition: %w", err)
		}

		if !ok {
			return a.conditionFailed(old), result, nil
		}
	}

	switch a.op {
	case opConditionCheck:
		return cancellation(reasonNone, "", nil), result, nil
	case opPut:
		result.new = a.key
	case opUpdate:
		updated, err := applyUpdate(a.update, a.key, old)
		if err != nil {
			return api.CancellationReason{}, txResult{}, err
		}

		result.new = updated
	default: // opDelete stores nothing.
	}

	if result.new != nil {
		err := a.entry.data.CheckItem(result.new)
		if err != nil {
			return api.CancellationReason{}, txResult{}, fmt.Errorf("transaction: %w", err)
		}
	}

	result.write = true

	return cancellation(reasonNone, "", nil), result, nil
}

// conditionFailed is the reason of an action whose condition does not hold:
// with the stored item when the action asked for it with ALL_OLD and there is
// one. DynamoDB Local ends a ConditionCheck's message with a full stop.
func (a *txAction) conditionFailed(old value.Item) api.CancellationReason {
	msg := msgConditionFailed
	if a.op == opConditionCheck {
		msg = msgConditionCheckFailed
	}

	var item value.Item
	if a.returnOld {
		item = old
	}

	return cancellation(reasonCondition, msg, item)
}

func cancellation(code, msg string, item value.Item) api.CancellationReason {
	r := api.CancellationReason{Code: new(code), Item: item}
	if msg != "" {
		r.Message = new(msg)
	}

	return r
}

// transactionCanceled is the TransactionCanceledException for reasons, which
// its message lists by code.
func transactionCanceled(reasons []api.CancellationReason) error {
	codes := make([]string, len(reasons))
	for i, r := range reasons {
		codes[i] = *r.Code
	}

	return (&api.TransactionCanceledException{CancellationReasons: reasons}).
		Err(msgTransactCanceled + strings.Join(codes, ", ") + "]")
}

// transactCapacity is what a transaction consumed on each table, as DynamoDB
// Local counts each action: a transactional write twice the units of a plain
// one, a delete of no item one unit, and a condition check twice the units of
// a consistent read of its item. The units are summed per table, as AWS
// documents transactional capacity; DynamoDB Local instead counts equal
// per-action amounts once.
func transactCapacity(actions []*txAction, results []txResult) tableConsumption {
	used := tableConsumption{}

	for i, a := range actions {
		r := results[i]

		var units float64

		switch {
		case a.op == opConditionCheck:
			units = transactUnits * readUnits(value.ItemSize(r.old), new(true))
		case a.op == opDelete && r.old == nil:
			units = 1
		default:
			units = transactUnits * writeUnits(r.old, r.new)
		}

		used.on(a.name).table += units
	}

	return used
}

// checkTransactShape checks what DynamoDB Local checks of a transaction
// before any one action: the number of actions, the token's length, that each
// holds exactly one, and their total size.
func checkTransactShape(in *api.TransactWriteItemsInput) error {
	switch n := len(in.TransactItems); {
	case in.TransactItems == nil:
		return value.Invalid(msgTransactNull)
	case n == 0:
		return value.Invalid(msgTransactEmpty)
	case n > maxTransactItems:
		return value.Invalid(msgTransactTooMany)
	}

	if t := in.ClientRequestToken; t != nil && len([]rune(*t)) > maxClientTokenRunes {
		return value.Invalid(fmt.Sprintf(msgTransactTokenLen, *t))
	}

	size := 0

	for i := range in.TransactItems {
		r, n := txRequestOf(&in.TransactItems[i])

		switch {
		case n == 0:
			return value.Invalid(msgTransactNoOp)
		case n > 1:
			return value.Invalid(msgTransactTwoOps)
		}

		size += r.payload()
	}

	if size > maxTransactBytes {
		return value.Invalid(msgTransactPayload + strconv.Itoa(size))
	}

	return nil
}

// checkTransactWrite checks each action in turn, as DynamoDB Local does, and
// refuses a second action on an item.
func (c caller) checkTransactWrite(items []api.TransactWriteItem) ([]*txAction, error) {
	actions := make([]*txAction, len(items))
	tables := map[string]*tableEntry{}
	seen := make(map[*tableEntry]map[string]struct{})

	for i := range items {
		r, _ := txRequestOf(&items[i])

		a, err := c.checkTransactAction(i+1, &r, tables)
		if err != nil {
			return nil, err
		}

		if seen[a.entry] == nil {
			seen[a.entry] = map[string]struct{}{}
		}

		id := itemKey(a.entry, a.key)
		if _, dup := seen[a.entry][id]; dup {
			return nil, value.Invalid(msgTransactSameItem)
		}

		seen[a.entry][id] = struct{}{}
		actions[i] = a
	}

	return actions, nil
}

// transactTable resolves an action's TableName, which may be the table's ARN,
// to the caller's table, the same entry for every action naming it.
func (c caller) transactTable(name string, tables map[string]*tableEntry) (string, *tableEntry, error) {
	// LocalStack keeps what follows ":table/", as DynamoDB Local takes no
	// ARN (transact_write_items in provider.py).
	if i := strings.LastIndex(name, ":table/"); i >= 0 {
		name = name[i+len(":table/"):]
	}

	if e := tables[name]; e != nil {
		return name, e, nil
	}

	e, err := c.lookup(&name)
	if err != nil {
		return "", nil, err
	}

	tables[name] = e

	return name, e, nil
}

// checkTransactAction checks the action at position (from 1).
func (c caller) checkTransactAction(position int, r *txRequest, tables map[string]*tableEntry) (*txAction, error) {
	member := func(name string) error {
		return value.Invalid(fmt.Sprintf(msgTransactMember, position, r.op+"."+name))
	}

	if r.table == nil {
		return nil, member("tableName")
	}

	name, e, err := c.transactTable(*r.table, tables)
	if err != nil {
		return nil, err
	}

	switch {
	case r.item == nil && r.op == opPut:
		return nil, member("item")
	case r.item == nil:
		return nil, member("key")
	case r.condition == nil && r.op == opConditionCheck:
		return nil, member("conditionExpression")
	}

	a := &txAction{
		op: r.op, name: name, entry: e, key: r.item,
		returnOld: r.onFailure != nil && *r.onFailure == returnAllOld,
	}

	err = a.checkKey()
	if err != nil {
		return nil, err
	}

	err = a.parse(r)
	if err != nil {
		return nil, err
	}

	return a, nil
}

// checkKey checks the action's item or key. A key value of the wrong type is
// kept as the action's cancellation reason.
func (a *txAction) checkKey() error {
	err := value.ValidateItem(a.key)
	if err != nil {
		return fmt.Errorf("transaction item: %w", err)
	}

	if a.op == opPut {
		err = a.entry.data.CheckItem(a.key)
	} else {
		err = a.entry.data.CheckKey(a.key)
	}

	if table.IsKeyTypeMismatch(err) {
		a.keyMismatch = msgKeyTypeMismatch

		return nil
	}

	if err != nil {
		return fmt.Errorf("transaction item: %w", err)
	}

	return nil
}

// parse parses the action's expressions and checks its placeholders, as
// PutItem, UpdateItem and DeleteItem do. An update of a key attribute cancels
// the transaction with that one reason.
func (a *txAction) parse(r *txRequest) error {
	if a.op != opUpdate {
		cond, err := writeCondition(r.condition, r.names, r.values)
		if err != nil {
			return err
		}

		a.condition = cond

		return nil
	}

	update, cond, err := updateExpressions(&api.UpdateItemInput{
		UpdateExpression: r.update, ConditionExpression: r.condition,
		ExpressionAttributeNames: r.names, ExpressionAttributeValues: r.values,
	})
	if err != nil {
		return err
	}

	a.update, a.condition = update, cond

	if update == nil {
		return nil
	}

	key := a.entry.data.Key()
	for _, path := range update.Paths() {
		if name := path[0].Name; name == key.Hash.Name || (key.HasRange() && name == key.Range.Name) {
			return transactionCanceled([]api.CancellationReason{
				cancellation(reasonValidation, fmt.Sprintf(msgUpdateKeyAttr, name), nil),
			})
		}
	}

	return nil
}
