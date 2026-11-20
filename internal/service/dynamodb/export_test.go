package dynamodb

import (
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
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

	return e.expire(value.Item{"id": {S: &id}}, attribute, &api.AttributeValue{N: &threshold})
}
