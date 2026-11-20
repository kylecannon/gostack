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

// SetLanes sets how many deliveries a lane holds and how long a publish waits
// for room, so a test fills a lane and reaches the deadline quickly.
func (p *Provider) SetLanes(depth int, wait time.Duration) {
	p.lanes.mu.Lock()
	defer p.lanes.mu.Unlock()

	p.lanes.depth, p.lanes.wait = depth, wait
}

// CutCalls stops the in-process client as a shutdown's drain does at its
// deadline: the calls queued in it are dropped without an answer.
func (p *Provider) CutCalls() {
	p.client.Close()
}

// BeginShutdown is the gateway's word that the shutdown has begun.
func (p *Provider) BeginShutdown() {
	p.shutdown()
}
