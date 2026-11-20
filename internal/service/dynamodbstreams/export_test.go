package dynamodbstreams

import "time"

// SetClock replaces the provider's clock, which iterator expiry reads.
func SetClock(p *Provider, now func() time.Time) {
	p.now = now
}

// RoundTripIterator exposes parseIterator to the fuzz target: it reports whether
// s parsed, and the text the parsed iterator writes back.
func RoundTripIterator(s string) (string, bool) {
	it, ok := parseIterator(s)
	if !ok {
		return "", false
	}

	return it.String(), true
}
