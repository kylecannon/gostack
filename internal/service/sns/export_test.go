package sns

import "context"

// AwaitDeliveries waits until no delivery is in a lane or ctx ends, for the
// benchmarks, which time a publish with its deliveries.
func (p *Provider) AwaitDeliveries(ctx context.Context) {
	p.lanes.awaitIdle(ctx)
}

// CutCalls stops the in-process client as a shutdown's drain does at its
// deadline: the calls queued in it are dropped without an answer.
func (p *Provider) CutCalls() {
	p.client.Close()
}
