package sns

import "context"

// AwaitDeliveries waits until no delivery is pending or ctx ends, for the
// benchmarks, which time a publish with its deliveries.
func (p *Provider) AwaitDeliveries(ctx context.Context) {
	p.pending.wait(ctx)
}
