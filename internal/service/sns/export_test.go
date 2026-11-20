package sns

import (
	"context"
	"time"
)

// AwaitDeliveries waits until no delivery is in a lane or ctx ends, for the
// benchmarks, which time a publish with its deliveries.
func (p *Provider) AwaitDeliveries(ctx context.Context) {
	p.lanes.awaitIdle(ctx)
}

// MaxPosts is the most HTTP deliveries in flight at once.
const MaxPosts = maxPosts

// SetPostTimeout sets the time one HTTP delivery may take, 10 s by default,
// so a test of a hung endpoint does not wait for it.
func (p *Provider) SetPostTimeout(d time.Duration) {
	p.lanes.poster.timeout = d
}

// SetIdleConnTimeout sets how long a connection to an endpoint is kept after
// its last delivery. It is called before the first delivery.
func (p *Provider) SetIdleConnTimeout(d time.Duration) {
	p.lanes.poster.transport.IdleConnTimeout = d
}

// LockWaits takes and lets go of the lanes lock, pausing between takes, until
// stop is closed, and returns how long each take waited: the burst benchmark's
// measure of what holding the lock costs everything else that needs it.
func (p *Provider) LockWaits(stop <-chan struct{}, pause time.Duration) []time.Duration {
	var waits []time.Duration

	for {
		select {
		case <-stop:
			return waits
		default:
		}

		start := time.Now()

		p.lanes.mu.Lock()

		waited := time.Since(start)

		p.lanes.mu.Unlock()

		waits = append(waits, waited)

		time.Sleep(pause)
	}
}

// CutCalls stops the in-process client as a shutdown's drain does at its
// deadline: the calls queued in it are dropped without an answer.
func (p *Provider) CutCalls() {
	p.client.Close()
}
