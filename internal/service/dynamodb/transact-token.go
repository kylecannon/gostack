package dynamodb

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"maps"
	"slices"
	"sync"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// tokenWindow is how long a TransactWriteItems ClientRequestToken stays
// recorded after its transaction succeeds.
const tokenWindow = 10 * time.Minute

// tokens records the ClientRequestTokens of one account and region's
// successful transactions, for their idempotency window. Its zero value is
// empty and ready. Expired tokens are dropped as new ones are recorded; no
// timer runs.
type tokens struct {
	mu   sync.Mutex
	byID map[string]*tokenEntry
	// sweepAt is the number of tokens at which the next record sweeps the
	// expired ones.
	sweepAt int
}

// tokenEntry is one recorded token: the request it was given with, when its
// transaction succeeded, and what that transaction consumed. done is false
// while the transaction runs.
type tokenEntry struct {
	request [sha256.Size]byte
	done    bool
	at      time.Time
	used    tableConsumption
}

// tokenState is what begin found for a token.
type tokenState int

const (
	// tokenNew is a token with no live record, now recorded as running.
	tokenNew tokenState = iota
	// tokenReplay is a token whose transaction succeeded with this request.
	tokenReplay
	// tokenMismatch is a token recorded with another request.
	tokenMismatch
)

// minSweep is the fewest tokens at which recording one sweeps expired ones,
// and sweepGrowth how much the map must grow after a sweep before the next.
const (
	minSweep    = 64
	sweepGrowth = 2
)

// begin looks up token for a transaction with request fingerprint request at
// now. A new token is recorded as running, so a concurrent transaction with
// another request is refused; one with this request touches the same tables
// and so waits for this one under their locks. tokenReplay returns what the
// recorded transaction consumed.
func (t *tokens) begin(token string, request [sha256.Size]byte, now time.Time) (tokenState, tableConsumption) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if e := t.byID[token]; e != nil && (!e.done || now.Sub(e.at) < tokenWindow) {
		if e.request != request {
			return tokenMismatch, nil
		}

		if e.done {
			return tokenReplay, e.used
		}
	}

	if t.byID == nil {
		t.byID = map[string]*tokenEntry{}
	}

	t.sweep(now)
	t.byID[token] = &tokenEntry{request: request}

	return tokenNew, nil
}

// finish records that the transaction begin let through succeeded at now,
// having consumed used.
func (t *tokens) finish(token string, now time.Time, used tableConsumption) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if e := t.byID[token]; e != nil {
		e.done, e.at, e.used = true, now, used
	}
}

// abort forgets the token of a transaction that failed: a failed transaction
// records nothing.
func (t *tokens) abort(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.byID, token)
}

// reset forgets every token.
func (t *tokens) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.byID, t.sweepAt = nil, 0
}

// sweep drops expired tokens once the map has doubled since the last sweep,
// so recording costs constant time on average.
func (t *tokens) sweep(now time.Time) {
	if len(t.byID) < max(t.sweepAt, minSweep) {
		return
	}

	maps.DeleteFunc(t.byID, func(_ string, e *tokenEntry) bool { return e.done && now.Sub(e.at) >= tokenWindow })
	t.sweepAt = sweepGrowth * len(t.byID)
}

// transactFingerprint identifies a TransactWriteItems request for its
// ClientRequestToken: its TransactItems as sent, numbers in their own
// spelling and maps in any order. It must be taken before the request's
// values are validated, which rewrites numbers.
func transactFingerprint(items []api.TransactWriteItem) [sha256.Size]byte {
	f := fingerprinter{h: sha256.New()}

	for i := range items {
		it := &items[i]

		switch {
		case it.Put != nil:
			f.text("Put")
			f.request(it.Put.TableName, it.Put.Item, it.Put.ConditionExpression, nil,
				it.Put.ExpressionAttributeNames, it.Put.ExpressionAttributeValues, it.Put.ReturnValuesOnConditionCheckFailure)
		case it.Update != nil:
			f.text("Update")
			f.request(it.Update.TableName, it.Update.Key, it.Update.ConditionExpression, it.Update.UpdateExpression,
				it.Update.ExpressionAttributeNames, it.Update.ExpressionAttributeValues,
				it.Update.ReturnValuesOnConditionCheckFailure)
		case it.Delete != nil:
			f.text("Delete")
			f.request(it.Delete.TableName, it.Delete.Key, it.Delete.ConditionExpression, nil,
				it.Delete.ExpressionAttributeNames, it.Delete.ExpressionAttributeValues,
				it.Delete.ReturnValuesOnConditionCheckFailure)
		case it.ConditionCheck != nil:
			f.text("ConditionCheck")
			f.request(it.ConditionCheck.TableName, it.ConditionCheck.Key, it.ConditionCheck.ConditionExpression, nil,
				it.ConditionCheck.ExpressionAttributeNames, it.ConditionCheck.ExpressionAttributeValues,
				it.ConditionCheck.ReturnValuesOnConditionCheckFailure)
		default:
			f.text("")
		}
	}

	var sum [sha256.Size]byte

	f.h.Sum(sum[:0])

	return sum
}

// fingerprinter feeds values to a hash, each length-prefixed and tagged, so
// that two different requests cannot feed it the same bytes.
type fingerprinter struct {
	h hash.Hash
}

func (f *fingerprinter) request(
	table *string, item map[string]api.AttributeValue, condition, update *string,
	names map[string]string, values map[string]api.AttributeValue, onFailure *string,
) {
	// The table is fed as the name it resolves to, so a request naming it by
	// ARN and then by name replays, as LocalStack strips the ARN before
	// DynamoDB Local compares the token.
	if table != nil {
		table = new(tableName(*table))
	}

	f.optional(table)
	f.item(item)
	f.optional(condition)
	f.optional(update)
	f.length(mapLength(names))

	for _, k := range slices.Sorted(maps.Keys(names)) {
		f.text(k)
		f.text(names[k])
	}

	f.item(values)
	f.optional(onFailure)
}

// absent is the length fed for a member that is not set, which no set
// member's length equals.
const absent = -1

// length feeds a length, or absent.
func (f *fingerprinter) length(n int) {
	var b [8]byte

	binary.BigEndian.PutUint64(b[:], uint64(int64(n))) //nolint:gosec // absent wraps to a length no value has
	f.h.Write(b[:])
}

func mapLength[V any](m map[string]V) int {
	if m == nil {
		return absent
	}

	return len(m)
}

func sliceLength[T any](s []T) int {
	if s == nil {
		return absent
	}

	return len(s)
}

func (f *fingerprinter) text(s string) {
	f.length(len(s))
	f.h.Write([]byte(s))
}

func (f *fingerprinter) optional(s *string) {
	if s == nil {
		f.length(absent)

		return
	}

	f.text(*s)
}

func (f *fingerprinter) item(m map[string]api.AttributeValue) {
	f.length(mapLength(m))

	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := m[k]
		f.text(k)
		f.value(&v)
	}
}

func (f *fingerprinter) texts(list []string) {
	f.length(sliceLength(list))

	for _, s := range list {
		f.text(s)
	}
}

// value feeds every member of v that is set, so a value with two members set
// differs from either alone.
func (f *fingerprinter) value(v *api.AttributeValue) {
	f.text("S")
	f.optional(v.S)
	f.text("N")
	f.optional(v.N)
	f.text("B")
	f.length(sliceLength(v.B))
	f.h.Write(v.B)
	f.text("SS")
	f.texts(v.SS)
	f.text("NS")
	f.texts(v.NS)
	f.text("BS")
	f.length(sliceLength(v.BS))

	for _, b := range v.BS {
		f.length(len(b))
		f.h.Write(b)
	}

	f.text("M")
	f.item(v.M)
	f.text("L")
	f.length(sliceLength(v.L))

	for i := range v.L {
		f.value(&v.L[i])
	}

	f.text("NULL")
	f.flag(v.NULL)
	f.text("BOOL")
	f.flag(v.BOOL)
}

func (f *fingerprinter) flag(b *bool) {
	switch {
	case b == nil:
		f.length(absent)
	case *b:
		f.length(1)
	default:
		f.length(0)
	}
}
