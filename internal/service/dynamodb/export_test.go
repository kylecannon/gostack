package dynamodb

import "time"

// SetClock replaces the provider's clock, which UpdateTable reads under the
// table's lock.
func SetClock(p *Provider, now func() time.Time) {
	p.now = now
}
