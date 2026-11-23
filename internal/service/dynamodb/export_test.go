package dynamodb

import (
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// SetClock replaces the provider's clock, which UpdateTable reads under the
// table's lock.
func SetClock(p *Provider, now func() time.Time) {
	p.now = now
}

// ExpiryArmed reports whether the expired-item sweep has a timer.
func ExpiryArmed(p *Provider) bool {
	p.expiry.mu.Lock()
	defer p.expiry.mu.Unlock()

	return p.expiry.timer != nil
}

// RemoveExpired runs one sweep of expired items and returns how many it removed.
func RemoveExpired(p *Provider) int {
	return p.removeExpired()
}

// Expire removes the item keyed id from the caller's table in region when its
// attribute is still a number at or before threshold, as the sweep does.
func Expire(p *Provider, account, region, table, id, attribute, threshold string) bool {
	e := p.states.Get(account, region).table(table)

	// The sweep names the key by the item it found; the key alone names it
	// as well.
	key, err := value.Pack(value.Item{"id": {S: &id}})
	if err != nil {
		panic(err)
	}

	return e.expire(key, attribute, &api.AttributeValue{N: &threshold})
}

// Cut takes the snapshot's one consistent copy of every table, the span its
// locks are held for, and drops it.
func Cut(p *Provider) error {
	_, err := p.cut()

	return err
}

// CutRecords is the stream records the provider's cuts hold for a streams
// snapshot, the one taken and the one committed together.
func CutRecords(p *Provider) int {
	n := 0

	for _, cut := range []*[]shardCut{p.cutTaken.Load(), p.streamCut.Load()} {
		if cut != nil {
			for _, c := range *cut {
				n += len(c.records)
			}
		}
	}

	return n
}

// ShapeResult is the result item a SELECT of the paths gives for item.
func ShapeResult(item value.Item, paths []partiql.Path) value.Item {
	return shapeItem(item, paths)
}

// Shard returns the latest stream shard of the test account's table in
// region, nil when it never had a stream.
func Shard(p *Provider, region, table string) *stream.Log {
	e := p.states.Get("000000000000", region).table(table)

	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.shard
}
