package dynamodb

import "time"

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
